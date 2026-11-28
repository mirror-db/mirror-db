import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";

/**
 * Revalidation inside the real runtime: the Cache API is the runtime's, the
 * clock is controlled, and `waitUntil` is turned into a plain await so a
 * background write is observable in the test.
 *
 * This is the case that cannot be asserted from outside: the Worker must notice
 * an entry is older than `MdbRevalidateIntervalMs`, ask the source whether it
 * still matches, and serve the stored copy without transferring the body.
 */

vi.mock("cloudflare:workers", () => ({
  waitUntil: (p: Promise<unknown>) => p,
  env: {},
}));

const upstream = vi.fn();

vi.mock("./fetch", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./fetch")>();
  return { ...mod, mdbfetch: (...args: unknown[]) => upstream(...args) };
});

const { createCachedfetch, MdbCacheManager } = await import("./cache");

const MINUTE = 60_000;

describe("cachedfetch — revalidation in workerd", () => {
  let now = 1_000_000;
  let manager: InstanceType<typeof MdbCacheManager>;
  let cachedfetch: ReturnType<typeof createCachedfetch>;

  beforeEach(() => {
    now = 1_000_000;
    upstream.mockReset();
    manager = new MdbCacheManager(
      caches.open(`rev-${Math.random()}`),
      undefined,
      () => now,
    );
    cachedfetch = createCachedfetch(manager);
  });

  afterEach(() => {
    upstream.mockReset();
  });

  const store = async (url: string, body: string, etag = '"v1"') => {
    await manager.save(
      new Request(url),
      new Response(body, {
        headers: { etag, "content-type": "application/json" },
      }),
    );
  };

  it("revalidates with a conditional HEAD after the interval, and serves the stored body", async () => {
    const url = `https://reval.invalid/${Math.random()}`;
    await store(url, '{"v":1}');

    now += 11 * MINUTE;
    upstream.mockImplementation(async (req: Request) => {
      expect(req.method).toBe("HEAD");
      expect(req.headers.get("if-none-match")).toBe('"v1"');
      return new Response(null, { status: 304 });
    });

    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-cache")).toBe("revalidated");
    expect(await res.text()).toBe('{"v":1}');
    // Exactly one request: the HEAD. The body was never re-fetched.
    expect(upstream).toHaveBeenCalledTimes(1);
  });

  it("does not revalidate before the interval", async () => {
    const url = `https://reval.invalid/${Math.random()}`;
    await store(url, '{"v":1}');

    now += 5 * MINUTE;
    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-cache")).toBe("hit");
    expect(upstream).not.toHaveBeenCalled();
  });

  it("refetches and replaces the entry when the source changed", async () => {
    const url = `https://reval.invalid/${Math.random()}`;
    await store(url, '{"v":1}');

    now += 11 * MINUTE;
    upstream.mockImplementation(async (req: Request) =>
      req.method === "HEAD"
        ? new Response(null, { status: 200, headers: { etag: '"v2"' } })
        : new Response('{"v":2}', { headers: { etag: '"v2"' } }),
    );

    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-cache")).toBe("miss");
    expect(await res.text()).toBe('{"v":2}');

    // The replacement is stored in the background, so the next request becomes
    // a hit once that write lands.
    await vi.waitFor(async () => {
      const again = await cachedfetch(url);
      expect(again.headers.get("x-mdb-cache")).toBe("hit");
      expect(await again.text()).toBe('{"v":2}');
    });
  });

  it("drops the entry when the source is gone", async () => {
    const url = `https://reval.invalid/${Math.random()}`;
    await store(url, '{"v":1}');

    now += 11 * MINUTE;
    upstream.mockImplementation(async (req: Request) =>
      req.method === "HEAD"
        ? new Response(null, { status: 404 })
        : new Response("gone", { status: 404 }),
    );

    const res = await cachedfetch(url);
    expect(res.status).toBe(404);
    expect(await cachedfetch(url).then((r) => r.headers.get("x-mdb-cache"))).not.toBe("hit");
  });

  it("keeps serving the stored copy when the source is unreachable", async () => {
    const url = `https://reval.invalid/${Math.random()}`;
    await store(url, '{"v":1}');

    now += 11 * MINUTE;
    upstream.mockImplementation(async () => {
      throw new TypeError("network down");
    });

    const res = await cachedfetch(url);
    expect(res.headers.get("x-mdb-cache")).toBe("hit");
    expect(await res.text()).toBe('{"v":1}');
  });
});
