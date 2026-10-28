import { beforeEach, describe, expect, it, vi } from "vitest";

import { MdbCacheManager, cachedfetch, mdbCache } from "./cache";
import { mdbfetch } from "./fetch";

// `ResolveCache` does `instanceof Cache`; Node has no such global, so stub it
// with an empty class that our fake is an instance of.
class Cache {}
vi.stubGlobal("Cache", Cache);


// A minimal in-memory stand-in for the Workers `Cache` interface. The real
// global lives in workerd; Node only needs put/match with the semantics that
// matter here (URL-keyed, method-sensitive unless `ignoreMethod` is set).
class FakeCache extends Cache {
  entries = new Map<string, Response>();

  async put(req: Request, resp: Response) {
    this.entries.set(req.url, resp);
  }

  async match(req: Request | string, opts?: CacheQueryOptions) {
    const key = typeof req === "string" ? new Request(req).url : req.url;
    if (req instanceof Request && req.method !== "GET" && !opts?.ignoreMethod) {
      return undefined;
    }
    return this.entries.get(key);
  }

  async delete(req: Request | string) {
    const key = typeof req === "string" ? new Request(req).url : req.url;
    return this.entries.delete(key);
  }
}

const state = vi.hoisted(() => ({
  cache: undefined as FakeCache | undefined,
  fetch: vi.fn(),
}));

// `cachedfetch` writes through the module-level `mdbCache`; redirect it (and
// the upstream fetch) into the per-test fakes.
vi.mock("./cache", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./cache")>();
  Object.defineProperty(mod.mdbCache, "cache", { get: () => state.cache });
  return mod;
});

vi.mock("./fetch", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./fetch")>();
  return { ...mod, mdbfetch: state.fetch };
});

const okResponse = () =>
  new Response("body", { status: 200, headers: { "content-length": "4" } });

describe("MdbCacheManager", () => {
  let cache: FakeCache;
  let mgr: MdbCacheManager;

  beforeEach(() => {
    cache = new FakeCache();
    mgr = new MdbCacheManager(cache);
  });

  it("save stores and match retrieves", async () => {
    const req = new Request("https://x.com/item");
    await mgr.save(req, okResponse());
    expect(await (await mgr.match(req))?.text()).toBe("body");
  });

  it("save skips non-GET requests", async () => {
    const putSpy = vi.spyOn(cache, "put");
    await mgr.save(new Request("https://x.com/post", { method: "POST" }), okResponse());
    expect(putSpy).not.toHaveBeenCalled();
  });

  it("save skips responses over the size limit", async () => {
    const small = new MdbCacheManager(cache, 100);
    const req = new Request("https://x.com/big");
    await small.save(
      req,
      new Response("x", { status: 200, headers: { "content-length": "200" } }),
    );
    expect(await small.match(req)).toBeUndefined();
  });
});

describe("cachedfetch", () => {
  let cache: FakeCache;

  beforeEach(() => {
    cache = state.cache = new FakeCache();
    state.fetch.mockReset();
    state.fetch.mockImplementation(async () => okResponse());
  });

  it("caches 2xx GET responses", async () => {
    const url = "https://x.com/get";
    await cachedfetch(url);
    await vi.waitFor(() => expect(cache.entries.has(url)).toBe(true));
  });

  it("serves a cache hit without hitting upstream", async () => {
    const url = "https://x.com/hit";
    cache.entries.set(url, new Response("cached"));
    expect(await (await cachedfetch(url)).text()).toBe("cached");
    expect(state.fetch).not.toHaveBeenCalled();
  });

  it("skips cache lookup and save for range requests", async () => {
    const url = "https://x.com/range";
    await cachedfetch(new Request(url, { headers: { range: "bytes=0-10" } }));
    expect(state.fetch).toHaveBeenCalledTimes(1);
    expect(state.fetch.mock.calls[0][0].headers.get("range")).toBe("bytes=0-10");
    expect(cache.entries.has(url)).toBe(false);
  });

  it("HEAD matches the GET cache entry", async () => {
    const url = "https://x.com/head";
    cache.entries.set(url, new Response("cached"));
    expect(await cachedfetch(new Request(url, { method: "HEAD" }))).toBeDefined();
    expect(state.fetch).not.toHaveBeenCalled();
  });

  it("HEAD forwards the method on a cache miss and does not save", async () => {
    const url = "https://x.com/head-miss";
    await cachedfetch(new Request(url, { method: "HEAD" }));
    expect(state.fetch.mock.calls[0][0].method).toBe("HEAD");
    expect(cache.entries.has(url)).toBe(false);
  });

  it("bypasses cache entirely for POST", async () => {
    const url = "https://x.com/post";
    await cachedfetch(new Request(url, { method: "POST", body: "x" }));
    expect(state.fetch.mock.calls[0][0].method).toBe("POST");
    expect(cache.entries.has(url)).toBe(false);
  });
});

// Guard: the mocked module still exports the real primitives.
expect(typeof mdbfetch).toBe("function");
expect(mdbCache).toBeInstanceOf(MdbCacheManager);
