import { MdbCacheName, MdbCacheSizeLimit, MdbRevalidateIntervalMs } from "./const";
import { buildRequest, ezfetch, mdbfetch } from "./fetch";
import { parseContentLength, parseRange } from "./http";
import { waitUntil } from "cloudflare:workers";

export type MdbCache = string | Cache | undefined | Promise<Cache>;

/** True when `c` looks like a Cache instance (no globals touched). */
const isCache = (c: unknown): c is Cache =>
  typeof (c as Cache | undefined)?.match === "function" &&
  typeof (c as Cache | undefined)?.put === "function";

export const ResolveCache = (cache: MdbCache): Promise<Cache> => {
  if (cache instanceof Promise) return cache;
  // Duck-typed on purpose: a `Cache` instance created by the runtime does not
  // always satisfy `instanceof Cache` across realm/global boundaries, and
  // `instanceof` throws outright where the global is absent.
  if (isCache(cache)) return Promise.resolve(cache);
  if (typeof cache === "string") return caches.open(cache);
  return caches.open(MdbCacheName);
};

/**
 * Bookkeeping header written into every stored entry. The Cache API does not
 * expose when an entry was stored, and upstream `Age`/`Date` are stripped on
 * the way in, so the store time has to travel with the object.
 */
export const MDB_STORED_AT = "x-mdb-stored-at";

/** How long an entry may be served before its source is re-checked. */
export const MDB_REVALIDATE_AFTER = "x-mdb-revalidate-after";

/**
 * Lift the advertised lifetime up to the revalidation floor, leaving every
 * other directive alone.
 *
 * Deliberately does **nothing** for `no-store` / `no-cache`: an upstream that
 * forbade caching stays forbidden. A response with no lifetime at all gets the
 * floor as a whole `Cache-Control` value.
 */
export function liftCacheControl(cacheControl: string | null): string {
  const floor = Math.floor(MdbRevalidateIntervalMs / 1000);
  if (!cacheControl) return `public, max-age=${floor}`;
  if (/(?:^|,)\s*(?:no-store|no-cache)\s*(?:,|$)/i.test(cacheControl)) {
    return cacheControl;
  }

  const match = cacheControl.match(/(?:^|,)\s*max-age\s*=\s*"?(\d+)"?/i);
  if (!match) return `${cacheControl}, max-age=${floor}`;

  const current = Number(match[1]);
  if (!Number.isFinite(current) || current >= floor) return cacheControl;

  // Replace only the number so the separator/quoting survive intact.
  return cacheControl.replace(
    /((?:^|,)\s*max-age\s*=\s*"?)(\d+)/i,
    `$1${floor}`,
  );
}

export class MdbCacheManager {
  private dedupSet: Set<string> = new Set();
  private readonly cache: MdbCache;
  private readonly sizeLimit: number;

  constructor(
    cache: MdbCache = MdbCacheName,
    sizeLimit: number = MdbCacheSizeLimit,
    /** Clock override for tests. */
    public now: () => number = Date.now,
  ) {
    this.cache = cache;
    this.sizeLimit = sizeLimit;
  }

  dedupExec<T>(
    req: Request,
    fn: () => Promise<T | undefined>,
  ): Promise<T | undefined> {
    if (this.dedupSet.has(req.url)) return Promise.resolve(undefined);
    this.dedupSet.add(req.url);
    return fn().finally(() => this.dedupSet.delete(req.url));
  }

  isCacheable(r: Request | Response): boolean {
    if (r instanceof Response) {
      // check if the content length is too large
      const size = parseContentLength(r.headers);
      if (size && size > this.sizeLimit) return false;
    }

    if (r instanceof Request) {
      // if "Content-Range" is specified, it means the response is not cacheable
      if (r.headers.get("content-range")) return false;

      // if range is specified, check if the range is too large
      const range = parseRange(r.headers);
      if (range?.length && range.length > this.sizeLimit) return false;
    }

    return true;
  }

  /**
   * Store `resp` under `req`, stamping the store time and lifting the advertised
   * lifetime to the revalidation floor.
   *
   * The advertised lifetime and the revalidation cadence are deliberately
   * independent: clients (and the edge) may hold a copy for a long time, while
   * the mirror re-checks the source every {@link revalidateInterval}. Lifting
   * the floor only makes the copy *usable longer*, never staler than the
   * revalidation the mirror performs anyway.
   */
  async save(req: Request, resp: Response) {
    // Only cache GET requests (Cache API behavior for other methods is undefined)
    if (req.method !== "GET") return;
    if (!this.isCacheable(req)) return;
    if (!this.isCacheable(resp)) return;

    await this.dedupExec(req, async () => {
      const cache = await ResolveCache(this.cache);
      const headers = new Headers(resp.headers);
      headers.set(MDB_STORED_AT, String(this.now()));
      if (!headers.has(MDB_REVALIDATE_AFTER)) {
        headers.set(MDB_REVALIDATE_AFTER, String(MdbRevalidateIntervalMs));
      }
      headers.set("cache-control", liftCacheControl(headers.get("cache-control")));
      const stamped = new Response(resp.body, {
        status: resp.status,
        statusText: resp.statusText,
        headers,
      });
      await cache.put(req, stamped);
    });
  }

  async fetchAndSave(req: Request, init?: RequestInit) {
    const newReq = new Request(req.url, init);
    if (!this.isCacheable(newReq)) return;

    // Drop `range` so we fetch + store the full object, not a partial slice.
    newReq.headers.delete("range");

    // `save()` does its own `dedupExec` + cacheability checks; wrapping this in
    // another `dedupExec(newReq)` would collide on the same URL key and make
    // the inner `put` short-circuit, so call it directly.
    const resp = await mdbfetch(newReq);
    await this.save(newReq, resp);
  }

  async match(req: Request, init?: CacheQueryOptions) {
    return ResolveCache(this.cache).then((cache) => cache.match(req, init));
  }

  async delete(req: Request) {
    await ResolveCache(this.cache).then((cache) => cache.delete(req));
  }

  /**
   * How long a stored response may be re-served before its source is re-checked.
   *
   * A hit is always checked at most once per `MDB_REVALIDATE_AFTER` (10 min by
   * default), measured from when the entry was stored — **not** from its
   * `max-age`. That decouples the lifetime advertised to clients (which is
   * deliberately long) from how quickly the mirror notices an upstream change:
   * the entry is re-stamped on every 304, so an unchanged object costs one
   * conditional HEAD per interval and transfer nothing.
   *
   * Returns `undefined` for entries without a store stamp (written outside this
   * manager, or predating it): those are served until evicted.
   */
  revalidateInterval(resp: Response): number | undefined {
    const storedAt = Number(resp.headers.get(MDB_STORED_AT));
    if (!Number.isFinite(storedAt) || storedAt <= 0) return undefined;

    const perEntry = Number(resp.headers.get(MDB_REVALIDATE_AFTER));
    const interval = Number.isFinite(perEntry) && perEntry > 0
      ? perEntry
      : MdbRevalidateIntervalMs;

    const age = Math.max(0, this.now() - storedAt);
    // Freshly stored (and every interval boundary after that) → check now.
    // In between → serve, so one interval costs at most one HEAD.
    const phase = age % interval;
    return phase === 0 && age > 0 ? 0 : interval - phase;
  }
}

export const mdbCache = new MdbCacheManager();

/**
 * What a cached fetch did, reported back to the caller so it can surface the
 * outcome on the response (`x-mdb-cache`). Without this, a stale or bypassed
 * entry is indistinguishable from a fresh one outside the Worker.
 */
export type CacheOutcome = "hit" | "revalidated" | "miss" | "bypass";

/** Response header reporting what the cache did for this request. */
export const MDB_CACHE_HEADER = "x-mdb-cache";

/**
 * Strip the manager's bookkeeping headers and record the outcome on the way
 * out — both describe the cache, not the object, so the stored headers never
 * travel to clients while `x-mdb-cache` (which clients and probes *do* want)
 * is added.
 */
const mark = (
  resp: Response,
  outcome: CacheOutcome,
): Response => {
  const headers = new Headers(resp.headers);
  headers.delete(MDB_STORED_AT);
  headers.delete(MDB_REVALIDATE_AFTER);
  headers.set(MDB_CACHE_HEADER, outcome);
  return new Response(resp.body, {
    status: resp.status,
    statusText: resp.statusText,
    headers,
  });
};

/** Revalidate `cached` against the source with a HEAD; resolves to the response to serve. */
async function revalidate(
  req: Request,
  cached: Response,
  ctx: MdbCacheManager,
): Promise<Response> {
  const probe = new Request(req.url, { method: "HEAD", headers: req.headers });
  probe.headers.delete("range");
  // Conditional on the stored validators: an unchanged object answers 304 and
  // transfer nothing; the entry is simply re-stamped as fresh.
  const etag = cached.headers.get("etag");
  const lastModified = cached.headers.get("last-modified");
  if (etag) probe.headers.set("if-none-match", etag);
  if (lastModified) probe.headers.set("if-modified-since", lastModified);

  let res: Response;
  try {
    res = await mdbfetch(probe);
  } catch {
    // Source unreachable: keep serving the stored copy rather than failing.
    return mark(cached, "hit");
  }

  if (res.status === 304) {
    const restamped = new Response(cached.body, {
      status: cached.status,
      statusText: cached.statusText,
      headers: cached.headers,
    });
    restamped.headers.set(MDB_STORED_AT, String(ctx.now()));
    waitUntil(ctx.save(req, restamped.clone()));
    return mark(restamped, "revalidated");
  }

  // The source answers HEAD inconsistently (405/501): treat the entry as stale
  // and fetch again through the normal path.
  if (res.status === 405 || res.status === 501) {
    await ctx.delete(req);
    return mark(await mdbfetch(req), "miss");
  }

  if (res.status === 200) {
    const fresh = await mdbfetch(req);
    if (fresh.ok) waitUntil(ctx.save(req, fresh.clone()));
    return mark(fresh, "miss");
  }

  // Gone upstream (404/410) or an error: drop the entry, serve the source as-is.
  await ctx.delete(req);
  if (res.status === 404 || res.status === 410) {
    return mark(await mdbfetch(req), "miss");
  }
  return mark(cached, "hit");
}

export const cachedfetch = (async (reqInfo, arg2, arg3) => {
  const req = buildRequest(reqInfo, arg2, arg3);

  // Cache strategy by method:
  // - GET: full cache (match → fetch → save), with periodic revalidation
  // - HEAD: match with ignoreMethod (reuse GET cache), no save on miss
  // - Other methods: bypass cache entirely

  if (req.method === "GET") {
    // Skip cache for range requests — a 206 partial keyed by full URL would
    // serve incomplete content to subsequent non-range requests.
    const hasRange = req.headers.has("range");

    if (!hasRange) {
      const cached = await mdbCache.match(req);
      if (cached) {
        const due = mdbCache.revalidateInterval(cached);
        if (due === undefined || due > 0) return mark(cached, "hit");
        return revalidate(req, cached, mdbCache);
      }
    }

    const resp = await mdbfetch(req);

    // Only cache successful, complete responses.
    if (resp.ok && !hasRange) {
      waitUntil(mdbCache.save(req, resp.clone()));
    }
    return mark(resp, hasRange ? "bypass" : "miss");
  }

  if (req.method === "HEAD") {
    // Try to reuse GET cache (ignoreMethod allows HEAD to match GET entries)
    const cached = await mdbCache.match(req, { ignoreMethod: true });
    if (cached) return mark(cached, "hit");

    // Miss: fetch directly but don't cache (HEAD responses have no body)
    return mark(await mdbfetch(req), "bypass");
  }

  // Other methods (POST, PUT, etc.): bypass cache entirely
  return mark(await mdbfetch(req), "bypass");
}) as typeof ezfetch;
