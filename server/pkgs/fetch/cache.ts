import { MdbCacheName, MdbCacheSizeLimit } from "./const";
import { buildRequest, ezfetch, mdbfetch } from "./fetch";
import { parseContentLength, parseRange } from "./http";

export type MdbCache = string | Cache | undefined | Promise<Cache>;

export const ResolveCache = (cache: MdbCache): Promise<Cache> => {
  if (cache instanceof Promise) return cache;
  if (cache instanceof Cache) return Promise.resolve(cache);
  if (typeof cache === "string") return caches.open(cache);
  return caches.open(MdbCacheName);
};

export class MdbCacheManager {
  private dedupSet: Set<string> = new Set();

  constructor(
    private cache: MdbCache = MdbCacheName,
    private sizeLimit: number = MdbCacheSizeLimit,
  ) {}

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

  async save(req: Request, resp: Response) {
    if (!this.isCacheable(req)) return;
    if (!this.isCacheable(resp)) return;

    await this.dedupExec(req, async () => {
      const cache = await ResolveCache(this.cache);
      await cache.put(req, resp);
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

  match(req: Request, init?: CacheQueryOptions) {
    return ResolveCache(this.cache).then((cache) => cache.match(req, init));
  }
}

export const mdbCache = new MdbCacheManager();

export const cachedfetch = (async (reqInfo, arg2, arg3) => {
  const req = buildRequest(reqInfo, arg2, arg3);

  let resp = await mdbCache.match(req);
  if (resp) return resp;

  resp = await mdbfetch(req);
  if (resp) await mdbCache.save(req, resp.clone());
  return resp;
}) as typeof ezfetch;
