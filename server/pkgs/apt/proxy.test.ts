import { beforeEach, describe, expect, it, vi } from "vitest";

import { withRelayDirectives } from "@server/relay-control";

import { AptRepo } from "./repo";
import { createAptProxy } from "./proxy";

const HASH = "a".repeat(64);
const BASE = "https://up.example/debian/";

// Mock env.ASSETS for defaultRender
vi.mock("cloudflare:workers", () => ({
  env: {
    ASSETS: {
      fetch: vi.fn(() => 
        Promise.resolve(
          new Response(
            "<html><head><title><!--title--></title></head><body><!--app-html--></body></html>"
          )
        )
      ),
    },
    BASE_DOMAIN: "mirs.uk",
  },
}));

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
  it("caches a known by-hash file as immutable without no-store", async () => {
    const proxy = makeProxy({ ready: true, hashes: [HASH] });
    const res = await proxy.fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`),
    );

    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
    expect(res.headers.get("x-mdb-relay")).toBeNull();
  });

  it.each([
    "dists/trixie/InRelease",
    "dists/trixie/Release",
    "dists/trixie/main/binary-amd64/Packages.gz",
  ])("marks the mutable index %s with relay no-store", async (path) => {
    const proxy = makeProxy({
      ready: true,
      paths: [path],
      validUntil: Date.now() + 3600_000,
    });
    const res = await proxy.fetch(get(path));

    expect(ezfetchMock).toHaveBeenCalled();
    expect(res.headers.get("x-mdb-relay")).toBe("no-store");

    // Existing freshness is unchanged: the relay decision is additive.
    const cc = res.headers.get("cache-control") ?? "";
    const maxAge = Number(cc.match(/max-age=(\d+)/)?.[1]);
    expect(maxAge).toBeGreaterThan(3500);
    expect(maxAge).toBeLessThanOrEqual(3600);
  });

  it("keeps no-store when the front middleware adds no-vary", async () => {
    const proxy = makeProxy({
      ready: true,
      paths: ["dists/trixie/InRelease"],
    });
    const res = await proxy.fetch(get("dists/trixie/InRelease"));

    const merged = withRelayDirectives(res, ["no-vary"]);
    expect(merged.headers.get("x-mdb-relay")).toBe("no-store, no-vary");
    expect(merged.headers.get("cache-control")).toBe(
      res.headers.get("cache-control"),
    );
  });

  it("does not mark pool payloads no-store", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(get("pool/main/n/nginx/nginx_1.0_amd64.deb"));

    expect(ezfetchMock).toHaveBeenCalled();
    expect(res.headers.get("x-mdb-relay")).toBeNull();
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

  it("adds prefetch to Release/InRelease only when the mirror opts in", async () => {
    ezfetchMock.mockImplementation(async () =>
      new Response("r", {
        status: 200,
        headers: { "x-mdb-relay": "evil", "content-length": "1" },
      }),
    );
    const proxy = createAptProxy(
      makeRepo({ ready: true, paths: ["dists/trixie/InRelease", "dists/trixie/main/binary-amd64/Packages"] }),
      { prefetchRelease: true },
    );

    const release = await proxy.fetch(get("dists/trixie/InRelease"));
    // The upstream's own directive is sanitised away, then ours is merged in.
    expect(release.headers.get("x-mdb-relay")).toBe("no-store, prefetch");

    const index = await proxy.fetch(get("dists/trixie/main/binary-amd64/Packages"));
    expect(index.headers.get("x-mdb-relay")).toBe("no-store");

    // A failed release fetch is not an invitation to refresh it.
    ezfetchMock.mockImplementation(async () => new Response(null, { status: 404 }));
    const missing = await proxy.fetch(get("dists/trixie/InRelease"));
    expect(missing.headers.get("x-mdb-relay")).toBe("no-store");
  });

  it("keeps prefetch on a 304 revalidation of Release/InRelease", async () => {
    ezfetchMock.mockImplementation(async () =>
      new Response(null, { status: 304, headers: { etag: '"release"' } }),
    );
    const proxy = createAptProxy(
      makeRepo({ ready: true, paths: ["dists/trixie/InRelease"] }),
      { prefetchRelease: true },
    );

    const res = await proxy.fetch(get("dists/trixie/InRelease"));

    expect(res.status).toBe(304);
    expect(res.headers.get("x-mdb-relay")).toBe("no-store, prefetch");
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
    const res = await proxy.fetch(get("dists"));

    // 308 keeps the method and body; the Location stays relative so the client
    // does not get pinned to the Worker's own origin (bypassing the relay).
    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("/dists/");
  });
});

describe("createAptProxy — method forwarding", () => {
  const head = (path: string) =>
    new Request(`https://mirs.uk/${path}`, { method: "HEAD" });

  it("forwards HEAD for an ordinary pool file and answers without a body", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(
      head("pool/main/n/nginx/nginx_1.0_amd64.deb"),
    );

    expect(ezfetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = ezfetchMock.mock.calls[0]!;
    expect(url).toBe("https://up.example/debian/pool/main/n/nginx/nginx_1.0_amd64.deb");
    expect(init).toEqual({ method: "HEAD" });
    // The mocked upstream returns a body regardless; a HEAD must not relay it.
    expect(await res.text()).toBe("");
  });

  it("forwards HEAD for a by-hash file and answers without a body", async () => {
    const proxy = makeProxy({ ready: true, hashes: [HASH] });
    const res = await proxy.fetch(
      head(`dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`),
    );

    expect(cachedfetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = cachedfetchMock.mock.calls[0]!;
    expect(url).toBe(
      `https://up.example/debian/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
    );
    expect(init).toEqual({ method: "HEAD" });
    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
    expect(await res.text()).toBe("");
  });

  it("leaves a GET without a method override", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(get("pool/main/n/nginx/nginx_1.0_amd64.deb"));

    expect(ezfetchMock.mock.calls[0]![1]).toBeUndefined();
    expect(await res.text()).toBe("data");
  });

  it("forwards Range for a known by-hash GET", async () => {
    cachedfetchMock.mockImplementation(async () =>
      new Response("CDATA", {
        status: 206,
        headers: { "content-range": "bytes 2-6/8", "content-length": "5" },
      }),
    );
    const proxy = makeProxy({ ready: true, hashes: [HASH] });
    const res = await proxy.fetch(
      new Request(
        `https://mirs.uk/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
        { headers: { range: "bytes=2-6" } },
      ),
    );

    expect(res.status).toBe(206);
    expect(res.headers.get("content-range")).toBe("bytes 2-6/8");
    const [url, init] = cachedfetchMock.mock.calls[0]!;
    expect(url).toBe(
      `https://up.example/debian/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
    );
    expect(new Headers((init as RequestInit).headers).get("range")).toBe("bytes=2-6");
  });

  it("does not forward Range for a pool file", async () => {
    const proxy = makeProxy({ ready: true, paths: [] });
    const res = await proxy.fetch(
      new Request("https://mirs.uk/pool/main/n/nginx/nginx_1.0_amd64.deb", {
        headers: { range: "bytes=2-6" },
      }),
    );

    expect(res.status).toBe(200);
    // Still the known limitation: a pool Range is not forwarded, so the
    // upstream answers the whole object.
    expect(ezfetchMock.mock.calls[0]![1]).toBeUndefined();
  });

  it("does not forward Range for a mutable dists index", async () => {
    const proxy = makeProxy({
      ready: true,
      paths: ["dists/trixie/main/binary-amd64/Packages.gz"],
    });
    const res = await proxy.fetch(
      new Request(
        "https://mirs.uk/dists/trixie/main/binary-amd64/Packages.gz",
        { headers: { range: "bytes=0-3" } },
      ),
    );

    expect(res.status).toBe(200);
    expect(ezfetchMock.mock.calls[0]![1]).toBeUndefined();
  });
});

describe("createAptProxy — snapshot facts", () => {
  const knownSets = (paths: string[], hashes: string[] = []) => async () => ({
    paths: new Set(paths),
    hashes: new Set(hashes),
    suites: new Set(
      paths
        .filter((path) => path.startsWith("dists/"))
        .map((path) => path.slice("dists/".length).split("/")[0]!),
    ),
  });

  it("redirects a pool directory addressed without a trailing slash", async () => {
    const proxy = createAptProxy(makeRepo({ ready: false }), {
      known: knownSets(["pool/main/n/nginx/nginx_1.0_amd64.deb"]),
    });

    const res = await proxy.fetch(get("pool/main/n/nginx"));
    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("/pool/main/n/nginx/");
    expect(ezfetchMock).not.toHaveBeenCalled();
  });

  it("redirects the dists root the same way", async () => {
    const proxy = createAptProxy(makeRepo({ ready: false }), {
      known: knownSets(["dists/trixie/InRelease"]),
    });

    const res = await proxy.fetch(get("dists"));
    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("/dists/");
  });

  it("leaves an unknown pool file to the ordinary file path", async () => {
    const proxy = createAptProxy(makeRepo({ ready: false }), {
      known: knownSets(["pool/main/n/nginx/nginx_1.0_amd64.deb"]),
    });

    const res = await proxy.fetch(get("pool/main/x/other/other_1.0_amd64.deb"));
    expect(res.status).toBe(200);
    expect(ezfetchMock).toHaveBeenCalledTimes(1);
  });

  it("prefers the snapshot hashes over a stale repo index", async () => {
    const fresh = "f".repeat(64);
    const proxy = createAptProxy(makeRepo({ ready: true, hashes: [] }), {
      known: knownSets(["dists/trixie/InRelease"], [fresh]),
    });

    // Without `known` the same request 404s — the repo never saw this hash.
    const stale = await createAptProxy(makeRepo({ ready: true, hashes: [] })).fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA256/${fresh}`),
    );
    expect(stale.status).toBe(404);

    const res = await proxy.fetch(
      get(`dists/trixie/main/binary-amd64/by-hash/SHA256/${fresh}`),
    );
    expect(res.status).toBe(200);
  });

  it("forwards Range for a by-hash object under a snapshot generation", async () => {
    cachedfetchMock.mockImplementation(async () =>
      new Response("CDATA", {
        status: 206,
        headers: { "content-range": "bytes 0-3/8", "content-length": "4" },
      }),
    );
    const proxy = createAptProxy(makeRepo({ ready: false }), {
      known: knownSets(["dists/trixie/InRelease"], [HASH]),
    });

    const res = await proxy.fetch(
      new Request(
        `https://mirs.uk/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
        { headers: { range: "bytes=0-3" } },
      ),
    );

    expect(res.status).toBe(206);
    const [, init] = cachedfetchMock.mock.calls[0]!;
    expect(new Headers((init as RequestInit).headers).get("range")).toBe("bytes=0-3");
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
