import { MdbCacheName, MdbCacheSizeLimit, MdbRevalidateIntervalMs } from "./const";
import { buildRequest, ezfetch, mdbfetch } from "./fetch";
import { parseContentLength, parseRange, STRIP_RESPONSE_HEADERS } from "./http";
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
 * The lifetime to advertise to clients, kept beside the stored copy.
 *
 * The store's own `Cache-Control` cannot express this: `cache.match()` honours
 * `max-age`, so an entry stored with the client-facing 10 minutes stopped
 * matching exactly when its revalidation was due — the entry expired instead of
 * being re-checked, and the revalidation branch never ran in production. The
 * stored copy therefore gets a long lifetime and the client value travels in
 * this header, applied when the entry is served.
 */
export const MDB_CLIENT_CACHE_CONTROL = "x-mdb-client-cc";

/** Lifetime given to the stored copy, so it survives until revalidation. */
const STORE_CACHE_CONTROL = "public, max-age=31536000";

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

/** Max body size that is materialised before storing (documents only). */
const MATERIALISE_LIMIT = 8 * 1024 * 1024;

/** Content types whose bodies are cheap to materialise and worth caching. */
const TEXTUAL = /^(?:text\/|application\/(?:json|xml|xhtml|javascript|yaml|toml|x-yaml))/i;

/**
 * Whether `save` should read the body into memory instead of storing a stream.
 *
 * True for documents with a known, modest length, and for textual bodies
 * without a length (autoindex pages are chunked). Binary bodies stay streamed
 * so a large layer or ISO never sits in the Worker's heap twice.
 */
const shouldMaterialise = (resp: Response): boolean => {
  const len = parseContentLength(resp.headers);
  if (len !== undefined) return len <= MATERIALISE_LIMIT;
  return TEXTUAL.test(resp.headers.get("content-type") ?? "");
};

export class MdbCacheManager {
  private dedupSet: Set<string> = new Set();
  /** URLs with a revalidation HEAD in flight, so a burst sends one. */
  private revalidating: Set<string> = new Set();
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
   *
   * The body is materialised for documents before `cache.put`. A caller that
   * transforms the response (npm metadata URL rewriting, PyPI's HTMLRewriter)
   * consumes the original body, leaving the cloned stream unread — and a `put`
   * of an unread stream never completes, which silently drops the entry.
   * Large binary bodies keep streaming.
   */
  async save(req: Request, resp: Response) {
    // Only cache GET requests (Cache API behavior for other methods is undefined)
    if (req.method !== "GET") return;
    if (!this.isCacheable(req)) return;
    if (!this.isCacheable(resp)) return;

    // An upstream that forbade caching stays uncached: its `Cache-Control` is
    // neither lifted nor ignored.
    if (isUncacheable(resp.headers.get("cache-control"))) return;

    // A response carrying `Vary` cannot be stored by the Cache API (it keys by
    // URL only and silently refuses one). Do not strip `Vary` and store the
    // rest: the next hit would then be delivered without the representation
    // headers the miss had. Let every such request reach the upstream and leave
    // variant handling to the relay.
    if (resp.headers.has("vary")) return;

    await this.dedupExec(req, async () => {
      const cache = await ResolveCache(this.cache);
      const headers = new Headers(resp.headers);
      // The Cache API must not hold auth challenges or cookies either. Store the
      // sanitised header set instead of whatever the upstream sent.
      for (const name of STRIP_RESPONSE_HEADERS) headers.delete(name);
      headers.set(MDB_STORED_AT, String(this.now()));
      if (!headers.has(MDB_REVALIDATE_AFTER)) {
        headers.set(MDB_REVALIDATE_AFTER, String(MdbRevalidateIntervalMs));
      }
      // The client-facing lifetime travels beside the entry; the stored copy
      // itself keeps a long one. `cache.match` honours `max-age`, so storing
      // the 10-minute client value made the entry expire exactly when its
      // revalidation came due — the re-check never ran and every hit became a
      // miss.
      headers.set(
        MDB_CLIENT_CACHE_CONTROL,
        liftCacheControl(headers.get("cache-control")),
      );
      headers.set("cache-control", STORE_CACHE_CONTROL);

      let body: ReadableStream<Uint8Array> | ArrayBuffer | null = resp.body;
      if (shouldMaterialise(resp)) {
        const buf = await resp.arrayBuffer();
        if (buf.byteLength > this.sizeLimit) return;
        body = buf;
      }

      const stamped = new Response(body, {
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

  /** Mark `key` as being revalidated; returns false when one is already running. */
  claimRevalidation(key: string): boolean {
    if (this.revalidating.has(key)) return false;
    this.revalidating.add(key);
    return true;
  }

  releaseRevalidation(key: string): void {
    this.revalidating.delete(key);
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
    // Past the interval → check on the next request (the check re-stamps the
    // entry, so the following one is a full interval away). Before that → serve
    // with the remaining time. Aligning to a modulo would let an entry whose
    // first check lands just after a boundary go unchecked for almost two
    // intervals.
    return age < interval ? interval - age : 0;
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
  // Restore the lifetime this mirror chose for clients; the stored copy carries
  // a long one so the entry survives until its revalidation is due.
  const clientCC = headers.get(MDB_CLIENT_CACHE_CONTROL);
  if (clientCC) {
    headers.set("cache-control", clientCC);
    headers.delete(MDB_CLIENT_CACHE_CONTROL);
  }
  headers.set(MDB_CACHE_HEADER, outcome);
  return new Response(resp.body, {
    status: resp.status,
    statusText: resp.statusText,
    headers,
  });
};

/**
 * Revalidate `cached` against the source with a conditional HEAD, and return the
 * response to serve.
 *
 * One HEAD per entry per interval: the claim set keeps a burst of concurrent
 * requests from all asking at once, and a `304` re-stamps the entry so the next
 * check is a full interval away.
 */
async function revalidate(
  req: Request,
  cached: Response,
  ctx: MdbCacheManager,
): Promise<Response> {
  if (!ctx.claimRevalidation(req.url)) {
    // Another request is already asking the source; serve what we have.
    return mark(cached, "hit");
  }

  try {
    const probe = new Request(req.url, { method: "HEAD", headers: req.headers });
    probe.headers.delete("range");
    // Conditional on the stored validators: an unchanged object answers 304 and
    // transfers nothing; the entry is simply re-stamped as fresh.
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

    // The source answers HEAD inconsistently (405/501): treat the entry as
    // stale and fetch again through the normal path.
    if (res.status === 405 || res.status === 501) {
      await ctx.delete(req);
      return mark(await mdbfetch(req), "miss");
    }

    if (res.status === 200) {
      const fresh = await mdbfetch(req);
      if (fresh.ok) waitUntil(ctx.save(req, fresh.clone()));
      return mark(fresh, "miss");
    }

    // Gone upstream (404/410) or an error: drop the entry, serve the source.
    await ctx.delete(req);
    if (res.status === 404 || res.status === 410) {
      return mark(await mdbfetch(req), "miss");
    }
    return mark(cached, "hit");
  } finally {
    ctx.releaseRevalidation(req.url);
  }
}

/** True when the upstream forbade storing this response. */
export function isUncacheable(cacheControl: string | null): boolean {
  return !!cacheControl && /(?:^|,)\s*(?:no-store|no-cache)\s*(?:,|$)/i.test(cacheControl);
}

/** `resp` with its `Cache-Control` lifted to the revalidation floor. */
const liftable = (resp: Response): Response => {
  const lifted = liftCacheControl(resp.headers.get("cache-control"));
  if (lifted === resp.headers.get("cache-control")) return resp;
  const headers = new Headers(resp.headers);
  headers.set("cache-control", lifted);
  return new Response(resp.body, {
    status: resp.status,
    statusText: resp.statusText,
    headers,
  });
};

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
      // Advertise the same lifetime the stored copy carries, so a client and a
      // later cache hit see the same value instead of an upstream figure that
      // disappears after the first request.
      return mark(liftable(resp), "miss");
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
