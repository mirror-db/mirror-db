import { describe, expect, it } from "vitest";

import { AptRepo, type FetchImpl } from "./repo";
import { proxyAptRequest, type CacheLike } from "./proxy";

const HASH = "a".repeat(64);
const BASE = "https://up.example/debian/";

/** A repo with a hand-set index, so we drive enforcement directly. */
function makeRepo(opts: {
  ready: boolean;
  paths?: string[];
  hashes?: string[];
  validUntil?: number;
}): AptRepo {
  const repo = new AptRepo({ base: BASE, fetchImpl: async () => new Response() });
  repo.state = opts.ready ? "ready" : "idle";
  repo.knownPaths = new Set(opts.paths ?? []);
  repo.knownHashes = new Set(opts.hashes ?? []);
  repo.validUntil = opts.validUntil ?? 0;
  return repo;
}

/** Mock fetch returning a 200 with an explicit Content-Length (size guard needs it). */
function mockFetch(body = "data"): { fetchImpl: FetchImpl; urls: string[] } {
  const urls: string[] = [];
  const fetchImpl: FetchImpl = async (input) => {
    urls.push(input.toString());
    return new Response(body, {
      status: 200,
      headers: { "content-length": String(body.length) },
    });
  };
  return { fetchImpl, urls };
}

/** In-memory Cache API mock. */
function mockCache(): { cache: CacheLike; store: Map<string, Response> } {
  const store = new Map<string, Response>();
  const cache: CacheLike = {
    async match(req) {
      return store.get(req.toString());
    },
    async put(req, res) {
      store.set(req.toString(), res);
    },
  };
  return { cache, store };
}

const get = () => new Request("https://mirs.uk/debian/x", { method: "GET" });

describe("proxyAptRequest", () => {
  it("passes through unjudged while the repo is not ready", async () => {
    const { fetchImpl, urls } = mockFetch();
    const repo = makeRepo({ ready: false });

    const res = await proxyAptRequest(get(), {
      rel: "dists/trixie/does-not-exist",
      repo,
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(urls).toEqual([`${BASE}dists/trixie/does-not-exist`]);
  });

  it("404s an unknown by-hash file once ready, without fetching", async () => {
    const { fetchImpl, urls } = mockFetch();
    const repo = makeRepo({ ready: true, hashes: [] });

    const res = await proxyAptRequest(get(), {
      rel: `dists/trixie/main/binary-amd64/by-hash/SHA256/${"b".repeat(64)}`,
      repo,
      fetchImpl,
    });

    expect(res.status).toBe(404);
    expect(urls).toHaveLength(0);
  });

  it("matches a known by-hash file under a non-SHA256 algo (SHA512)", async () => {
    const sha512 = "c".repeat(128);
    const { fetchImpl, urls } = mockFetch();
    const repo = makeRepo({ ready: true, hashes: [sha512] });

    const res = await proxyAptRequest(get(), {
      rel: `dists/trixie/main/binary-amd64/by-hash/SHA512/${sha512}`,
      repo,
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(urls).toHaveLength(1);
  });

  it("404s an unknown dists file once ready", async () => {
    const { fetchImpl } = mockFetch();
    const repo = makeRepo({ ready: true, paths: ["dists/trixie/Release"] });

    const res = await proxyAptRequest(get(), {
      rel: "dists/trixie/ghost",
      repo,
      fetchImpl,
    });

    expect(res.status).toBe(404);
  });

  it("always passes through pool/ paths (not listed in Release)", async () => {
    const { fetchImpl, urls } = mockFetch();
    const repo = makeRepo({ ready: true, paths: [] });

    const res = await proxyAptRequest(get(), {
      rel: "pool/main/n/nginx/nginx_1.0_amd64.deb",
      repo,
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(urls).toHaveLength(1);
  });

  it("caches a known by-hash file as immutable", async () => {
    const { fetchImpl } = mockFetch();
    const { cache, store } = mockCache();
    const rel = `dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`;
    const repo = makeRepo({ ready: true, hashes: [HASH] });

    const res = await proxyAptRequest(get(), { rel, repo, fetchImpl, cache });

    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
    expect(store.has(`${BASE}${rel}`)).toBe(true);
  });

  it("caches a dist file with the source freshness window", async () => {
    const { fetchImpl } = mockFetch();
    const { cache } = mockCache();
    const repo = makeRepo({
      ready: true,
      paths: ["dists/trixie/InRelease"],
      validUntil: Date.now() + 3600_000,
    });

    const res = await proxyAptRequest(get(), {
      rel: "dists/trixie/InRelease",
      repo,
      fetchImpl,
      cache,
    });

    const cc = res.headers.get("cache-control") ?? "";
    const maxAge = Number(cc.match(/max-age=(\d+)/)?.[1]);
    expect(maxAge).toBeGreaterThan(3500);
    expect(maxAge).toBeLessThanOrEqual(3600);
  });

  it("serves a cache hit without fetching upstream", async () => {
    const { fetchImpl, urls } = mockFetch();
    const { cache } = mockCache();
    const rel = "dists/trixie/InRelease";
    await cache.put(`${BASE}${rel}`, new Response("cached", { status: 200 }));
    const repo = makeRepo({ ready: true, paths: [rel] });

    const res = await proxyAptRequest(get(), { rel, repo, fetchImpl, cache });

    expect(await res.text()).toBe("cached");
    expect(urls).toHaveLength(0);
  });
});
