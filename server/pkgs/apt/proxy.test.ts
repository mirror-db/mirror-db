import { beforeEach, describe, expect, it, vi } from "vitest";

import { AptRepo } from "./repo";
import { createAptProxy } from "./proxy";

const HASH = "a".repeat(64);
const BASE = "https://up.example/debian/";

// The proxy fetches upstream through `cachedfetch` (by-hash) or `ezfetch`
// (mutable dists/pool files); mock both to drive responses.
const { cachedfetchMock, ezfetchMock } = vi.hoisted(() => ({
  cachedfetchMock: vi.fn(),
  ezfetchMock: vi.fn(),
}));
vi.mock("@server/pkgs/fetch", async (importOriginal) => {
  const orig: any = await importOriginal();
  return {
    ...orig,
    cachedfetch: cachedfetchMock,
    ezfetch: ezfetchMock,
  };
});

// WebListFs.readdir needs HTMLRewriter (Workers runtime), unavailable in node.
// Mock the parseListing function so readdir returns a fixed listing.
vi.mock("@server/pkgs/web-list/parse", async (importOriginal) => {
  const orig: any = await importOriginal();
  return {
    ...orig,
    parseListing: async () => [
      { name: "dists", href: "dists/", type: "directory", lastModified: null, size: null },
      { name: "pool", href: "pool/", type: "directory", lastModified: null, size: null },
      { name: "README", href: "README", type: "file", lastModified: null, size: 100 },
    ],
  };
});

beforeEach(() => {
  cachedfetchMock.mockReset();
  ezfetchMock.mockReset();
  const ok = async () =>
    new Response("data", { status: 200, headers: { "content-length": "4" } });
  cachedfetchMock.mockImplementation(ok);
  ezfetchMock.mockImplementation(ok);
});

/** A repo with a hand-set index for direct enforcement testing. */
function makeRepo(opts: {
  ready: boolean;
  paths?: string[];
  hashes?: string[];
  suites?: string[];
  validUntil?: number;
}): AptRepo {
  const repo = new AptRepo({ base: BASE });
  repo.state = opts.ready ? "ready" : "idle";
  repo.knownPaths = new Set(opts.paths ?? []);
  repo.knownHashes = new Set(opts.hashes ?? []);
  // Default the resolved-suite set to whatever the seeded paths imply, so most
  // tests don't have to spell it out; an explicit list overrides.
  repo.knownSuites = new Set(
    opts.suites ??
      (opts.paths ?? [])
        .filter((p) => p.startsWith("dists/"))
        .map((p) => p.slice("dists/".length).split("/")[0]),
  );
  repo.validUntil = opts.validUntil ?? Date.now() + 3_600_000;
  // Prevent the proxy's awaitResolved from triggering a real resolve that would
  // mutate the hand-set state.
  repo.awaitResolved = async () => {};
  return repo;
}

function makeProxy(opts: Parameters<typeof makeRepo>[0]) {
  return createAptProxy(makeRepo(opts));
}

const get = (path: string) =>
  new Request(`https://mirs.uk/${path}`, { method: "GET" });

describe("createAptProxy — file enforcement", () => {
  it("passes through unjudged while the repo is not ready", async () => {
    const proxy = makeProxy({ ready: false });
    const res = await proxy.fetch(get("dists/trixie/does-not-exist"));

    expect(res.status).toBe(200);
    expect(ezfetchMock).toHaveBeenCalled();
  });

  it("404s an unknown by-hash file once ready, without fetching", async () => {
    const proxy = makeProxy({ ready: true, hashes: [] });
    const res = await proxy.fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA256/${"b".repeat(64)}`),
    );

    expect(res.status).toBe(404);
    expect(cachedfetchMock).not.toHaveBeenCalled();
  });

  it("matches a known by-hash file (SHA512)", async () => {
    const sha512 = "c".repeat(128);
    const proxy = makeProxy({ ready: true, hashes: [sha512] });
    const res = await proxy.fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA512/${sha512}`),
    );

    expect(res.status).toBe(200);
    expect(cachedfetchMock).toHaveBeenCalledTimes(1);
  });

  it("404s an unknown dists file once ready", async () => {
    const proxy = makeProxy({ ready: true, paths: ["dists/trixie/Release"] });
    const res = await proxy.fetch(get("dists/trixie/ghost"));

    expect(res.status).toBe(404);
    expect(cachedfetchMock).not.toHaveBeenCalled();
  });

  it("passes through a dists path under an un-indexed suite", async () => {
    // Curated mirrors resolve only a subset of suites; a request for a suite the
    // index never touched (e.g. bullseye) must reach upstream, not 404.
    const proxy = makeProxy({ ready: true, paths: ["dists/trixie/Release"] });
    const res = await proxy.fetch(get("dists/bullseye/InRelease"));

    expect(res.status).toBe(200);
    expect(ezfetchMock).toHaveBeenCalledTimes(1);
  });

  it("always passes through pool/ paths (not in Release)", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(get("pool/main/n/nginx/nginx_1.0_amd64.deb"));

    expect(res.status).toBe(200);
    expect(ezfetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("createAptProxy — cache-control", () => {
  it("caches a known by-hash file as immutable", async () => {
    const proxy = makeProxy({ ready: true, hashes: [HASH] });
    const res = await proxy.fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`),
    );

    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
  });

  it("caches a dist file with the source freshness window", async () => {
    const proxy = makeProxy({
      ready: true,
      paths: ["dists/trixie/InRelease"],
      validUntil: Date.now() + 3600_000,
    });
    const res = await proxy.fetch(get("dists/trixie/InRelease"));

    expect(ezfetchMock).toHaveBeenCalled();
    const cc = res.headers.get("cache-control") ?? "";
    const maxAge = Number(cc.match(/max-age=(\d+)/)?.[1]);
    expect(maxAge).toBeGreaterThan(3500);
    expect(maxAge).toBeLessThanOrEqual(3600);
  });

  it("strips set-cookie from upstream responses", async () => {
    ezfetchMock.mockImplementation(async () =>
      new Response("x", {
        status: 200,
        headers: { "set-cookie": "foo=bar", "content-length": "1" },
      }),
    );
    const proxy = makeProxy({ ready: true, paths: ["dists/trixie/Release"] });
    const res = await proxy.fetch(get("dists/trixie/Release"));

    expect(res.headers.has("set-cookie")).toBe(false);
  });
});

describe("createAptProxy — directory listing", () => {
  it("renders root directory as HTML via defaultRender", async () => {
    const proxy = makeProxy({ ready: true, paths: ["dists/trixie/InRelease"] });
    const res = await proxy.fetch(get(""));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/html");
    const html = await res.text();
    expect(html).toContain("dists/");
  });

  it("returns JSON with ?format=json", async () => {
    const proxy = makeProxy({ ready: true, paths: ["dists/trixie/InRelease"] });
    const res = await proxy.fetch(
      new Request("https://mirs.uk/?format=json"),
    );

    expect(res.headers.get("content-type")).toContain("application/json");
    const body: any = await res.json();
    expect(body.path).toBe("");
    expect(body.entries.length).toBeGreaterThan(0);
  });

  it("redirects to trailing slash for a dists dir without trailing slash", async () => {
    const proxy = makeProxy({
      ready: true,
      paths: ["dists/trixie/InRelease"],
    });
    // "dists" has children in knownPaths → fetchHook returns 301 redirect
    const res = await proxy.fetch(get("dists"));

    expect(res.status).toBe(301);
    expect(res.headers.get("location")).toContain("dists/");
  });
});

describe("createAptProxy — WebDAV", () => {
  it("OPTIONS returns DAV:1", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(
      new Request("https://mirs.uk/", { method: "OPTIONS" }),
    );

    expect(res.status).toBe(204);
    expect(res.headers.get("dav")).toBe("1");
  });

  it("PROPFIND returns 207 multistatus", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(
      new Request("https://mirs.uk/", { method: "PROPFIND", headers: { depth: "1" } }),
    );

    expect(res.status).toBe(207);
    const xml = await res.text();
    expect(xml).toContain("<D:multistatus");
  });

  it("PUT returns 405", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(
      new Request("https://mirs.uk/foo", { method: "PUT" }),
    );

    expect(res.status).toBe(405);
  });
});
