/**
 * APT repository reverse proxy with an index-aware cache.
 *
 * Given a repo-relative path and a resolved {@link AptRepo} index, this:
 *   - 404s requests for files the index says don't exist (once the repo is
 *     `ready`; before that everything passes through unjudged);
 *   - serves + stores content-addressed `by-hash/SHA256/<hash>` files with an
 *     immutable one-year cache (the hash pins the bytes — clients self-verify);
 *   - caches everything else until the source's `Valid-Until`.
 *
 * Dependency-injected (`fetchImpl`, `cache`, `waitUntil`) so it unit-tests
 * without the Workers runtime, mirroring `pkgs/oci/registry-proxy.ts`.
 */

import type { AptRepo, FetchImpl } from "./repo";

/** Minimal Cache API surface — keeps the module testable with a mock. */
export interface CacheLike {
  match(request: string | URL | Request): Promise<Response | undefined>;
  put(request: string | URL | Request, response: Response): Promise<void>;
}

/** The Workers default Cache, or `undefined` outside the runtime (Node tests). */
export function defaultCache(): CacheLike | undefined {
  return typeof caches !== "undefined"
    ? (caches.default as unknown as CacheLike)
    : undefined;
}

export interface AptProxyOptions {
  /** Repo-relative path, e.g. `dists/trixie/InRelease` (no leading slash). */
  rel: string;
  /** The in-memory index that decides existence + freshness. */
  repo: AptRepo;
  /** Injected fetch (defaults to global). */
  fetchImpl?: FetchImpl;
  /** Optional Cache API store. */
  cache?: CacheLike;
  /** Max object size to cache, in bytes (default 512 MB — the Free/Pro limit). */
  maxCacheBytes?: number;
  /** Schedule a background promise (e.g. `ctx.waitUntil`) for async cache writes. */
  waitUntil?: (promise: Promise<unknown>) => void;
}

/** Request headers that must not be forwarded upstream (`range` is kept). */
const STRIP_REQUEST_HEADERS = new Set([
  "authorization",
  "host",
  "cookie",
  "x-forwarded-for",
  "x-forwarded-host",
  "x-forwarded-proto",
  "cf-connecting-ip",
  "cf-ray",
  "cf-visitor",
  "cf-ipcountry",
]);

/** Response headers stripped before returning to the client (and before cache write). */
const STRIP_RESPONSE_HEADERS = new Set([
  "set-cookie",
  "set-cookie2",
  "vary",
  "age",
  "connection",
  "keep-alive",
  "transfer-encoding",
]);

/** Matches a content-addressed `by-hash/<algo>/<hash>` path; group 1 is the hash. */
const BY_HASH = /\/by-hash\/(?:MD5Sum|SHA1|SHA256|SHA512)\/([0-9a-f]{32,128})$/i;

const IMMUTABLE_CACHE_CONTROL = "public, max-age=31536000, immutable";
const DEFAULT_MAX_CACHE_BYTES = 512 * 1024 * 1024;

const notFound = (): Response => new Response(null, { status: 404 });

function buildUpstreamHeaders(request: Request): Headers {
  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!STRIP_REQUEST_HEADERS.has(key.toLowerCase())) headers.set(key, value);
  });
  return headers;
}

function sanitizeHeaders(response: Response): Headers {
  const headers = new Headers();
  response.headers.forEach((value, key) => {
    if (!STRIP_RESPONSE_HEADERS.has(key.toLowerCase())) headers.set(key, value);
  });
  return headers;
}

/** Cacheable only with a known `Content-Length` within the size cap. */
function withinCacheLimit(response: Response, limit: number): boolean {
  const len = response.headers.get("content-length");
  if (!len) return false;
  const bytes = Number(len);
  return Number.isFinite(bytes) && bytes <= limit;
}

export async function proxyAptRequest(
  request: Request,
  opts: AptProxyOptions,
): Promise<Response> {
  const fetchImpl = opts.fetchImpl ?? (globalThis.fetch as FetchImpl);
  const { rel, repo } = opts;

  const hash = rel.match(BY_HASH)?.[1]?.toLowerCase() ?? null;

  // Enforcement only kicks in once the index is populated; until then we proxy
  // everything so the mirror works during the first-request resolve window.
  if (repo.ready) {
    if (hash !== null) {
      if (!repo.knownHashes.has(hash)) return notFound();
    } else if (rel.startsWith("dists/")) {
      if (!repo.knownPaths.has(rel)) return notFound();
    }
    // `pool/` and other namespaces aren't listed in Release → never 404.
  }

  const upstreamUrl = repo.url(rel);
  const cacheable = request.method === "GET" && !!opts.cache;
  if (cacheable) {
    const hit = await opts.cache!.match(upstreamUrl);
    if (hit) return hit;
  }

  const upstream = await fetchImpl(upstreamUrl, {
    method: request.method,
    headers: buildUpstreamHeaders(request),
    redirect: "follow",
  });

  const headers = sanitizeHeaders(upstream);

  // Store cacheable 200s. Immutable content-addressed files cache forever; the
  // rest inherit the source's freshness window (`Valid-Until - now`).
  if (
    cacheable &&
    upstream.status === 200 &&
    withinCacheLimit(upstream, opts.maxCacheBytes ?? DEFAULT_MAX_CACHE_BYTES)
  ) {
    headers.set(
      "cache-control",
      hash !== null ? IMMUTABLE_CACHE_CONTROL : sourceCacheControl(repo),
    );
    const stored = new Response(upstream.body, {
      status: 200,
      statusText: upstream.statusText,
      headers,
    });
    const write = opts.cache!.put(upstreamUrl, stored.clone()).catch(() => {});
    if (opts.waitUntil) opts.waitUntil(write);
    else await write;
    return stored;
  }

  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers,
  });
}

/** `Cache-Control` derived from how long the source index stays valid. */
function sourceCacheControl(repo: AptRepo): string {
  const seconds = Math.max(0, Math.floor((repo.validUntil - Date.now()) / 1000));
  return `public, max-age=${seconds}`;
}
