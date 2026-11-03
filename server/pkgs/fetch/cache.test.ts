import { beforeEach, describe, expect, it, vi } from "vitest";

import { MdbCacheManager, cachedfetch, liftCacheControl, mdbCache } from "./cache";
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
  now: Date.now(),
}));

// `cachedfetch` writes through the module-level `mdbCache`; redirect it (and
// the upstream fetch) into the per-test fakes. The clock is virtual so the
// max-age → revalidate schedule can be exercised without waiting.
vi.mock("./cache", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./cache")>();
  Object.defineProperty(mod.mdbCache, "cache", { get: () => state.cache });
  Object.defineProperty(mod.mdbCache, "now", { get: () => () => state.now });
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
    state.now = Date.now();
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

// ── Revalidation ────────────────────────────────────────────────────────────
//
// A stored entry is re-checked with a conditional HEAD at most once per
// `MdbRevalidateIntervalMs` (10 min), counted from when it was stored — not
// from its `max-age`. A 304 re-stamps the entry; a 200 means the object changed
// and the new copy is stored.

describe("cachedfetch — periodic revalidation", () => {
  const url = "https://x.com/rev";
  const STORED_AT = 1_000_000;
  const INTERVAL = 10 * 60 * 1000;

  const stored = (opts: { etag?: string; maxAge?: number; body?: string } = {}) => {
    const headers: Record<string, string> = {
      "content-length": String((opts.body ?? "v1").length),
      "x-mdb-stored-at": String(STORED_AT),
    };
    if (opts.etag) headers.etag = opts.etag;
    if (opts.maxAge !== undefined) headers["cache-control"] = `public, max-age=${opts.maxAge}`;
    // Entries written by `save()` carry the client lifetime here; a hand-made
    // fixture must mirror that shape.
    headers["x-mdb-client-cc"] = "public, max-age=600";
    return new Response(opts.body ?? "v1", { headers });
  };

  let cache: FakeCache;

  beforeEach(() => {
    cache = new FakeCache();
    state.cache = cache;
    state.fetch.mockReset();
    state.now = STORED_AT;
  });

  it("serves the stored copy between revalidation points, whatever max-age says", async () => {
    // max-age is tiny, but the entry is only a minute old → no HEAD yet.
    cache.entries.set(url, stored({ etag: '"a"', maxAge: 60 }));
    state.now = STORED_AT + 60_000;
    expect(await (await cachedfetch(url)).text()).toBe("v1");
    expect(state.fetch).not.toHaveBeenCalled();
  });

  it("revalidates with a conditional HEAD once the interval has elapsed", async () => {
    cache.entries.set(url, stored({ etag: '"a"', maxAge: 31_536_000 }));
    state.now = STORED_AT + INTERVAL;
    state.fetch.mockImplementation(async (req: Request) => {
      expect(req.method).toBe("HEAD");
      expect(req.headers.get("if-none-match")).toBe('"a"');
      return new Response(null, { status: 304 });
    });

    expect(await (await cachedfetch(url)).text()).toBe("v1");
    // Only the HEAD went out: the body was not refetched.
    expect(state.fetch).toHaveBeenCalledTimes(1);
  });

  it("re-stamps the entry after a 304, so the next check waits a full interval", async () => {
    cache.entries.set(url, stored({ etag: '"a"' }));
    state.now = STORED_AT + INTERVAL;
    state.fetch.mockImplementation(async () => new Response(null, { status: 304 }));

    await cachedfetch(url);
    await vi.waitFor(() =>
      expect(Number(cache.entries.get(url)?.headers.get("x-mdb-stored-at"))).toBe(
        STORED_AT + INTERVAL,
      ),
    );

    // One second later it is a plain hit — no second HEAD.
    state.fetch.mockReset();
    state.fetch.mockImplementation(async () => new Response(null, { status: 304 }));
    state.now = STORED_AT + INTERVAL + 1_000;
    expect(await (await cachedfetch(url)).text()).toBe("v1");
    expect(state.fetch).not.toHaveBeenCalled();
  });

  it("re-fetches and stores the new body when the source changed", async () => {
    cache.entries.set(url, stored({ etag: '"a"' }));
    state.now = STORED_AT + INTERVAL;
    state.fetch.mockImplementation(async (req: Request) =>
      req.method === "HEAD"
        ? new Response(null, { status: 200, headers: { etag: '"b"' } })
        : stored({ etag: '"b"', body: "v2" }),
    );

    expect(await (await cachedfetch(url)).text()).toBe("v2");
    await vi.waitFor(() =>
      expect(cache.entries.get(url)?.headers.get("etag")).toBe('"b"'),
    );
  });

  it("drops the entry when the source answers 404", async () => {
    cache.entries.set(url, stored({ etag: '"a"' }));
    state.now = STORED_AT + INTERVAL;
    state.fetch.mockImplementation(async (req: Request) =>
      req.method === "HEAD"
        ? new Response(null, { status: 404 })
        : new Response("gone", { status: 404 }),
    );

    expect((await cachedfetch(url)).status).toBe(404);
    expect(cache.entries.has(url)).toBe(false);
  });

  it("keeps serving the stored copy when the source is unreachable", async () => {
    cache.entries.set(url, stored({ etag: '"a"' }));
    state.now = STORED_AT + INTERVAL;
    state.fetch.mockImplementation(async () => {
      throw new TypeError("network down");
    });

    expect(await (await cachedfetch(url)).text()).toBe("v1");
  });

  it("serves entries written outside the manager without revalidating", async () => {
    cache.entries.set(url, new Response("legacy"));
    state.now = STORED_AT + 365 * 24 * 3600 * 1000;
    expect(await (await cachedfetch(url)).text()).toBe("legacy");
    expect(state.fetch).not.toHaveBeenCalled();
  });

  it("serves the client lifetime, not the store's long one", async () => {
    cache.entries.set(url, stored({ etag: '"a"', maxAge: 60 }));
    state.now = STORED_AT + 1_000;
    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-cache")).toBe("hit");
    expect(res.headers.get("x-mdb-client-cc")).toBeNull();
    expect(res.headers.get("cache-control")).toBe("public, max-age=600");
  });

  it("does not leak the bookkeeping headers to the client", async () => {
    cache.entries.set(url, stored({ etag: '"a"' }));
    state.now = STORED_AT + 1_000;
    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-stored-at")).toBeNull();
    expect(res.headers.get("x-mdb-revalidate-after")).toBeNull();
  });
});

describe("MdbCacheManager.revalidateInterval", () => {
  const at = (now: number) => new MdbCacheManager(new FakeCache(), undefined, () => now);
  const entry = () =>
    new Response("x", { headers: { "x-mdb-stored-at": "400" } });

  it("returns the remaining time inside an interval", () => {
    // Stored at 400 ms, interval is 10 min → first check at 400 + 600 000.
    expect(at(400).revalidateInterval(entry())).toBe(600_000);
    expect(at(400 + 60_000).revalidateInterval(entry())).toBe(540_000);
  });

  it("returns 0 once the interval has elapsed, not only on a boundary", () => {
    expect(at(400 + 600_000).revalidateInterval(entry())).toBe(0);
    // Late by half an interval still revalidates now: aligning to a modulo
    // would leave the entry unchecked for almost two intervals.
    expect(at(400 + 900_000).revalidateInterval(entry())).toBe(0);
    expect(at(400 + 1_200_000).revalidateInterval(entry())).toBe(0);
  });

  it("honours a per-entry interval", () => {
    const r = new Response("x", {
      headers: { "x-mdb-stored-at": "400", "x-mdb-revalidate-after": "1000" },
    });
    expect(at(400).revalidateInterval(r)).toBe(1000);
    expect(at(1400).revalidateInterval(r)).toBe(0);
  });

  it("returns undefined without a store stamp", () => {
    expect(at(1).revalidateInterval(new Response("x"))).toBeUndefined();
  });
});

describe("liftCacheControl", () => {
  it("adds the floor when the source sends no lifetime", () => {
    expect(liftCacheControl(null)).toBe("public, max-age=600");
  });

  it("raises a shorter max-age and keeps the other directives", () => {
    expect(liftCacheControl("public, max-age=60")).toBe("public, max-age=600");
    expect(liftCacheControl("public, s-maxage=60, max-age=5")).toBe(
      "public, s-maxage=60, max-age=600",
    );
    expect(liftCacheControl("max-age=5")).toBe("max-age=600");
  });

  it("leaves a longer lifetime alone", () => {
    expect(liftCacheControl("public, max-age=31536000, immutable")).toBe(
      "public, max-age=31536000, immutable",
    );
  });

  it("does not invent a lifetime for no-store / no-cache", () => {
    // An upstream that forbids storage keeps forbidding it.
    expect(liftCacheControl("no-store")).toBe("no-store");
    expect(liftCacheControl("private, no-cache")).toBe("private, no-cache");
    expect(liftCacheControl("no-store, max-age=0")).toBe("no-store, max-age=0");
  });
});
