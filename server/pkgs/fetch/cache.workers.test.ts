import { describe, expect, it, vi, beforeEach } from "vitest";

import { MdbCacheManager } from "./cache";

/**
 * The digest of the cache behaviour is only observable inside the runtime: the
 * Cache API exists under Node, but nothing is stored. These tests use the real
 * `caches` global from workerd.
 */

let counter = 0;
const uniqueUrl = (tag: string) =>
  `https://cache-test.invalid/${tag}-${counter++}`;

describe("MdbCacheManager (workerd)", () => {
  let cache: Cache;
  let mgr: MdbCacheManager;

  beforeEach(async () => {
    cache = await caches.open(`mdb-test-${Math.random()}`);
    mgr = new MdbCacheManager(cache);
  });

  it("stores and retrieves a body", async () => {
    const request = new Request(uniqueUrl("plain"));
    await mgr.save(
      request,
      new Response("cached-data", {
        headers: {
          "content-length": "11",
          "content-type": "text/plain",
          "cache-control": "public, max-age=3600",
        },
      }),
    );

    const hit = await mgr.match(request);
    expect(await hit!.text()).toBe("cached-data");
  });

  it("stores a textual body whose length is unknown", async () => {
    // Autoindex pages and npm metadata arrive chunked; without materialising
    // them the `put` of the unread cloned stream never completes.
    const request = new Request(uniqueUrl("chunked"));
    await mgr.save(
      request,
      new Response('{"ok":true}', {
        headers: { "content-type": "application/json" },
      }),
    );

    const hit = await mgr.match(request);
    expect(await hit!.text()).toBe('{"ok":true}');
  });

  it("survives the caller consuming the original body", async () => {
    // This is the npm/PyPI shape: the caller transforms the body (URL rewrite,
    // HTMLRewriter) while the cache writes the clone.
    const request = new Request(uniqueUrl("consumed"));
    const upstream = new Response('{"name":"pkg"}', {
      headers: { "content-type": "application/json" },
    });
    const clone = upstream.clone();
    const saving = mgr.save(request, clone);
    expect(await upstream.text()).toBe('{"name":"pkg"}');
    await saving;

    const hit = await mgr.match(request);
    expect(await hit!.text()).toBe('{"name":"pkg"}');
  });

  it("stores a response that carries Vary", async () => {
    // The Cache API refuses to store a response with `Vary` (it keys by URL
    // only), which silently disabled caching for every upstream that sends one
    // — npm metadata, PyPI Simple pages. The stored copy must be sanitised.
    const request = new Request(uniqueUrl("vary"));
    await mgr.save(
      request,
      new Response('{"ok":1}', {
        headers: {
          "content-type": "application/json",
          vary: "accept-encoding, accept",
          "set-cookie": "a=b",
        },
      }),
    );

    const hit = await mgr.match(request);
    expect(hit).toBeDefined();
    expect(await hit!.text()).toBe('{"ok":1}');
    expect(hit!.headers.get("vary")).toBeNull();
    expect(hit!.headers.get("set-cookie")).toBeNull();
  });

  it("stamps the store time and lifts the advertised lifetime", async () => {
    const request = new Request(uniqueUrl("stamped"));
    await mgr.save(
      request,
      new Response("x", {
        headers: { "content-type": "text/plain", "cache-control": "public, max-age=60" },
      }),
    );

    const hit = await mgr.match(request);
    expect(Number(hit!.headers.get("x-mdb-stored-at"))).toBeGreaterThan(0);
    expect(hit!.headers.get("cache-control")).toBe("public, max-age=600");
  });

  it("skips responses over the size limit", async () => {
    const small = new MdbCacheManager(cache, 100);
    const request = new Request(uniqueUrl("big"));
    await small.save(
      request,
      new Response("x", { headers: { "content-length": "200" } }),
    );
    expect(await small.match(request)).toBeUndefined();
  });

  it("skips requests carrying content-range", async () => {
    const request = new Request(uniqueUrl("partial"), {
      headers: { "content-range": "bytes 0-99/1000" },
    });
    await mgr.save(request, new Response("x", { headers: { "content-length": "1" } }));
    expect(await mgr.match(request)).toBeUndefined();
  });

  it("deduplicates concurrent saves for the same URL", async () => {
    const putSpy = vi.spyOn(cache, "put");
    const request = new Request(uniqueUrl("dedup"));
    const response = new Response("data", { headers: { "content-length": "4" } });

    await Promise.all([
      mgr.save(request, response.clone()),
      mgr.save(request, response.clone()),
    ]);
    expect(putSpy).toHaveBeenCalledTimes(1);
  });

  it("deletes an entry on request", async () => {
    const request = new Request(uniqueUrl("del"));
    await mgr.save(request, new Response("x", { headers: { "content-length": "1" } }));
    await mgr.delete(request);
    expect(await mgr.match(request)).toBeUndefined();
  });
});
