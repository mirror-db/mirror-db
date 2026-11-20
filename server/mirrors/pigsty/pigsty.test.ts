import { afterEach, describe, expect, it, vi } from "vitest";

import { pigsty } from "./index";

/**
 * Drive the mount end to end and record what reaches the upstream `fetch`.
 *
 * The upstream advertises `Cache-Control: no-store`, so the stub answers that
 * too — the read-through cache then declines to store, exactly as in
 * production.
 */
function captureUpstream() {
  const sent: Request[] = [];
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    sent.push(input instanceof Request ? input : new Request(input));
    return new Response("pkg", {
      status: 200,
      headers: {
        "content-type": "application/octet-stream",
        "cache-control": "no-store",
      },
    });
  });

  // The Cache API is absent under Node; caching is a no-op there.
  const fakeCache = {
    async match() {
      return undefined;
    },
    async put() {},
  };
  vi.stubGlobal("caches", { open: async () => fakeCache, default: fakeCache });

  return {
    sent,
    restore() {
      spy.mockRestore();
      vi.unstubAllGlobals();
    },
  };
}

describe("pigsty apt mirror", () => {
  let captured: ReturnType<typeof captureUpstream> | undefined;

  afterEach(() => {
    captured?.restore();
    captured = undefined;
  });

  it("is a plain-HTTP path mount tagged `apt`", () => {
    expect(pigsty.name).toBe("pigsty");
    expect(pigsty.path).toBe("pigsty");
    expect(pigsty.keepHTTP).toBe(true);
    expect(pigsty.policyTags).toEqual(["apt"]);
  });

  it.each([
    // `infra` is one suite rooted at `infra/dists/generic`.
    [
      "/pigsty/infra/dists/generic/InRelease",
      "https://repo.pigsty.io/apt/infra/dists/generic/InRelease",
    ],
    [
      "/pigsty/infra/pool/main/p/pgbackrest/pgbackrest.deb",
      "https://repo.pigsty.io/apt/infra/pool/main/p/pgbackrest/pgbackrest.deb",
    ],
    // Each `pgsql/<codename>` is its own repository root.
    [
      "/pigsty/pgsql/bookworm/dists/bookworm/InRelease",
      "https://repo.pigsty.io/apt/pgsql/bookworm/dists/bookworm/InRelease",
    ],
    [
      "/pigsty/pgsql/bookworm/pool/main/p/postgresql-17/postgresql-17.deb",
      "https://repo.pigsty.io/apt/pgsql/bookworm/pool/main/p/postgresql-17/postgresql-17.deb",
    ],
  ])("forwards %s to %s", async (path, upstream) => {
    captured = captureUpstream();

    const res = await pigsty.fetch(new Request(`https://mirs.uk${path}`));

    expect(res.status).toBe(200);
    expect(captured.sent.map((r) => r.url)).toEqual([upstream]);
  });

  it("preserves the query string", async () => {
    captured = captureUpstream();

    await pigsty.fetch(
      new Request("https://mirs.uk/pigsty/infra/dists/generic/Release?x=1&y=2"),
    );

    expect(captured.sent[0].url).toBe(
      "https://repo.pigsty.io/apt/infra/dists/generic/Release?x=1&y=2",
    );
  });

  it("passes Range through to the upstream", async () => {
    captured = captureUpstream();

    const res = await pigsty.fetch(
      new Request("https://mirs.uk/pigsty/infra/pool/main/p/x/x.deb", {
        headers: { Range: "bytes=0-1023" },
      }),
    );

    expect(res.status).toBe(200);
    expect(captured.sent[0].headers.get("range")).toBe("bytes=0-1023");
  });
});
