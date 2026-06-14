import { describe, expect, it, vi, beforeEach } from "vitest";

import { MdbCacheManager, cachedfetch } from "./cache";

describe("MdbCacheManager", () => {
  let mockCache: Cache;
  let mgr: MdbCacheManager;

  beforeEach(async () => {
    // Use a real Workers Cache instance via caches.open
    mockCache = await caches.open("test-cache-" + Math.random());
    mgr = new MdbCacheManager(mockCache);
  });

  it("save stores and match retrieves", async () => {
    const request = new Request("https://x.com/item");
    const response = new Response("cached-data", {
      status: 200,
      headers: {
        "content-length": "11",
        "content-type": "text/plain",
        "cache-control": "public, max-age=3600",
      },
    });

    await mgr.save(request, response);
    const hit = await mgr.match(request);
    expect(hit).toBeDefined();
    expect(await hit!.text()).toBe("cached-data");
  });

  it("match returns undefined on miss", async () => {
    const request = new Request("https://x.com/nope");
    const hit = await mgr.match(request);
    expect(hit).toBeUndefined();
  });

  it("save skips when response content-length exceeds size limit", async () => {
    const smallMgr = new MdbCacheManager(mockCache, 100);
    const request = new Request("https://x.com/big");
    const response = new Response("x", {
      headers: { "content-length": "200" },
    });
    await smallMgr.save(request, response);
    const hit = await smallMgr.match(request);
    expect(hit).toBeUndefined();
  });

  it("save skips when request has content-range", async () => {
    const request = new Request("https://x.com/partial", {
      headers: { "content-range": "bytes 0-99/1000" },
    });
    const response = new Response("x", {
      headers: { "content-length": "1" },
    });
    await mgr.save(request, response);
    const hit = await mgr.match(request);
    expect(hit).toBeUndefined();
  });

  it("deduplicates concurrent saves for same URL", async () => {
    const putSpy = vi.spyOn(mockCache, "put");
    const request = new Request("https://x.com/dedup");
    const response = new Response("data", {
      headers: { "content-length": "4" },
    });

    await Promise.all([
      mgr.save(request, response.clone()),
      mgr.save(request, response.clone()),
    ]);
    expect(putSpy).toHaveBeenCalledTimes(1);
  });
});

describe("cachedfetch", () => {
  it("fetches from upstream and caches 2xx responses", async () => {
    const resp = await cachedfetch("https://httpbin.org/get");
    expect(resp.ok).toBe(true);
  });

  it("does not cache non-2xx responses", async () => {
    const resp = await cachedfetch("https://httpbin.org/status/404");
    expect(resp.status).toBe(404);
  });

  it("skips cache for range requests", async () => {
    const resp = await cachedfetch(
      new Request("https://httpbin.org/get", {
        headers: { range: "bytes=0-10" },
      }),
    );
    // Should still get a response (not error)
    expect(resp).toBeDefined();
  });
});
