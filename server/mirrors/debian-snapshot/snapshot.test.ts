import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  ARCHIVES,
  PREFIX,
  parseTimestamp,
  resolveTarget,
  UPSTREAM,
} from "./index";
import { createDebianSnapshot, type SnapshotCache } from "./index";

/** A Cache stub that records what was written. */
class FakeCache implements SnapshotCache {
  entries = new Map<string, Response>();
  puts = 0;

  async match(req: Request) {
    return this.entries.get(req.url);
  }

  async put(req: Request, resp: Response) {
    this.puts++;
    this.entries.set(req.url, resp);
  }
}

const ok = (body = "payload", headers: Record<string, string> = {}) =>
  new Response(body, {
    status: 200,
    headers: { "content-length": String(body.length), ...headers },
  });

function harness(handler: (url: string, init?: RequestInit) => Promise<Response>) {
  const cache = new FakeCache();
  const fetchImpl = vi.fn(async (input: Request) => handler(input.url, input));
  const mirror = createDebianSnapshot({
    cache,
    fetchImpl,
    waitUntil: () => {},
    // Fixed clock: 2026-01-01T00:00:00Z.
    now: () => Date.UTC(2026, 0, 1),
  });
  const get = (path: string, init?: RequestInit) =>
    mirror.fetch(new Request(`https://mirs.uk${PREFIX}${path}`, init));
  return { cache, fetchImpl, get };
}

describe("resolveTarget", () => {
  it("accepts a known archive with a well-formed timestamp", () => {
    const url = resolveTarget("archive/debian/20240101T000000Z/dists/bookworm/InRelease");
    expect(url?.toString()).toBe(
      `${UPSTREAM}archive/debian/20240101T000000Z/dists/bookworm/InRelease`,
    );
  });

  it("accepts every documented archive name", () => {
    for (const archive of ARCHIVES) {
      expect(resolveTarget(`archive/${archive}/20240101T000000Z/dists/`)).toBeDefined();
    }
  });

  it("accepts the machine-readable API", () => {
    expect(resolveTarget("mr/package/bash/")?.toString()).toBe(
      `${UPSTREAM}mr/package/bash/`,
    );
  });

  it("rejects browsing UI, unknown archives and malformed timestamps", () => {
    for (const rel of [
      "",
      "archive",
      "archive/debian",
      "archive/debian/20240101/dists/",
      "archive/debian/20240101T0000Z/dists/",
      "archive/nope/20240101T000000Z/dists/",
      "archive/debian/20240101T000000Z/../../etc/passwd",
      "package/bash/",
      "binary/bash/",
      "archive/debian/20240101T000000Z/../../mr/",
    ]) {
      expect(resolveTarget(rel), rel).toBeUndefined();
    }
  });
});

describe("parseTimestamp", () => {
  it("parses the UTC form", () => {
    expect(parseTimestamp("20240101T000000Z")?.toISOString()).toBe(
      "2024-01-01T00:00:00.000Z",
    );
  });

  it("rejects anything else", () => {
    for (const bad of ["20240101T000000", "2024-01-01T00:00:00Z", "", "20241341T000000Z"]) {
      expect(parseTimestamp(bad), bad).toBeUndefined();
    }
  });
});

describe("debian-snapshot mirror", () => {
  beforeEach(() => vi.clearAllMocks());

  it("does not cache an archive response carrying Vary and keeps it consistent", async () => {
    const { get, fetchImpl, cache } = harness(async () =>
      ok("InRelease-bytes", { "content-type": "text/html", vary: "Accept" }),
    );
    const path = "archive/debian/20240101T000000Z/dists/bookworm/InRelease";

    const first = await get(path);
    const second = await get(path);

    expect(fetchImpl).toHaveBeenCalledTimes(2);
    expect(fetchImpl.mock.calls[0][0].url).toBe(
      `${UPSTREAM}archive/debian/20240101T000000Z/dists/bookworm/InRelease`,
    );
    expect(first.status).toBe(200);
    expect(first.headers.get("cache-control")).toBe("public, max-age=31536000, immutable");
    // Delivery keeps the upstream `Vary`; a Vary response is not stored at all,
    // so the next request cannot come from a copy without it.
    expect(first.headers.get("vary")).toBe("Accept");
    expect(second.headers.get("vary")).toBe("Accept");
    expect(cache.puts).toBe(0);
  });

  it("serves a second request from the cache", async () => {
    const { get, fetchImpl } = harness(async () => ok("payload"));
    const path = "archive/debian/20240101T000000Z/dists/bookworm/Release";

    await get(path);
    const res = await get(path);

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(await res.text()).toBe("payload");
  });

  it("does not cache a timestamp in the future", async () => {
    // The upstream resolves a future timestamp to the newest snapshot, which
    // keeps moving — caching it for a year would freeze that answer.
    const { get, cache } = harness(async () => ok("moving"));
    const res = await get("archive/debian/20990101T000000Z/dists/bookworm/InRelease");

    expect(res.status).toBe(200);
    expect(cache.puts).toBe(0);
    // Advertised with a short lifetime: the answer will differ tomorrow.
    expect(res.headers.get("cache-control")).toBe("public, max-age=600");
  });

  it("never caches the mr API", async () => {
    const { get, cache } = harness(async () => ok("{}", { "content-type": "application/json" }));
    await get("mr/package/bash/");
    expect(cache.puts).toBe(0);
  });

  it("forwards Range and keeps the partial response out of the cache", async () => {
    const { get, fetchImpl, cache } = harness(
      async () =>
        new Response("part", {
          status: 206,
          headers: { "content-range": "bytes 0-3/100", "content-length": "4" },
        }),
    );

    const res = await get("archive/debian/20240101T000000Z/pool/main/b/bash/bash_5.2.deb", {
      headers: { range: "bytes=0-3" },
    });

    expect(fetchImpl.mock.calls[0][0].headers.get("range")).toBe("bytes=0-3");
    expect(res.status).toBe(206);
    expect(res.headers.get("content-range")).toBe("bytes 0-3/100");
    expect(cache.puts).toBe(0);
  });

  it("passes an upstream 404 through", async () => {
    const { get, cache } = harness(async () => new Response("nope", { status: 404 }));
    const res = await get("archive/debian/20150102T030303Z/dists/bookworm/InRelease");
    expect(res.status).toBe(404);
    expect(cache.puts).toBe(0);
  });

  it("answers 404 for paths outside the mount's contract", async () => {
    const { get, fetchImpl } = harness(async () => ok());
    for (const path of [
      "archive/debian/20240101T000000Z/../../etc/passwd",
      "archive/unknown/20240101T000000Z/dists/",
      "package/bash/",
      "binary/bash/1.0/amd64/download",
    ]) {
      const res = await get(path);
      expect(res.status, path).toBe(404);
    }
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("does not store a body over the Cache API limit", async () => {
    const cache = new FakeCache();
    const mirror = createDebianSnapshot({
      cache,
      maxCacheBytes: 10,
      waitUntil: () => {},
      now: () => Date.UTC(2026, 0, 1),
      fetchImpl: async () => ok("x".repeat(100)),
    });

    await mirror.fetch(
      new Request(
        `https://mirs.uk${PREFIX}archive/debian/20240101T000000Z/pool/main/b/big/big_1.deb`,
      ),
    );
    expect(cache.puts).toBe(0);
  });

  it("answers 502 when the upstream fetch throws", async () => {
    const { get } = harness(async () => {
      throw new TypeError("network down");
    });
    expect((await get("archive/debian/20240101T000000Z/dists/bookworm/InRelease")).status).toBe(
      502,
    );
  });
});
