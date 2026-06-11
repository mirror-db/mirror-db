/**
 * Generic OCI distribution (registry) reverse proxy.
 *
 * Presents an upstream registry as a "no login required" mirror: clients hit
 * this proxy with no credentials, the proxy authenticates to the upstream on
 * their behalf (token auth, RFC 7235 Bearer challenge), and any
 * `WWW-Authenticate` challenge is stripped from responses so clients never see
 * a login prompt.
 *
 * The module is dependency-injected (`fetchImpl`, `getCredentials`, `kv`) so it
 * can be unit-tested without the Workers runtime.
 */

import { CFCacheLimit, mdbfetch, parseContentLength, sanitizeResponse, STRIP_RESPONSE_HEADERS } from "@server/pkgs/fetch";

import {
  basicAuthHeader,
  type Credentials,
  type KvLike,
} from "./creds";
import { parseWwwAuthenticate, type AuthChallenge } from "./www-authenticate";

export type FetchImpl = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;

/** Minimal Cache API surface used here — keeps the module testable with a mock. */
export interface CacheLike {
  match(request: string | URL | Request): Promise<Response | undefined>;
  put(request: string | URL | Request, response: Response): Promise<void>;
}

/**
 * The Workers default Cache, or `undefined` outside the runtime (e.g. unit
 * tests in Node) where the `caches` global does not exist.
 */
export function defaultCache(): CacheLike | undefined {
  return typeof caches !== "undefined"
    ? (caches.default as unknown as CacheLike)
    : undefined;
}

export interface RegistryProxyOptions {
  /** Upstream registry host, e.g. `"registry-1.docker.io"`. */
  upstream: string;
  /** Resolve stored credentials (or null for anonymous access). */
  getCredentials?: () => Promise<Credentials | null>;
  /** Rewrite the incoming pathname before forwarding (e.g. `library/` fixup). */
  rewritePath?: (path: string) => string;
  /** Injected fetch (defaults to global). */
  fetchImpl?: FetchImpl;
  /** Optional KV for caching bearer tokens by scope. */
  kv?: KvLike;
  /** KV key prefix for cached tokens. */
  tokenCachePrefix?: string;
  /** Optional Cache API store for content-addressed (digest) responses. */
  cache?: CacheLike;
  /** Max object size to cache, in bytes (default 512 MB — the Free/Pro limit). */
  maxCacheBytes?: number;
  /** Schedule a background promise (e.g. `ctx.waitUntil`) for async cache writes. */
  waitUntil?: (promise: Promise<unknown>) => void;
}

/** Request headers that must not be forwarded upstream. */
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

/** Max server-side redirect hops to follow before giving up. */
const MAX_REDIRECTS = 5;

const isRedirect = (status: number): boolean =>
  status === 301 || status === 302 || status === 303 || status === 307 || status === 308;

/**
 * Content-addressed registry paths: blobs and manifests fetched *by digest*
 * (`<algo>:<hex>`). These are immutable, so they are safe to cache forever.
 * Tag manifests (`/manifests/latest`) are mutable and deliberately excluded.
 * The captured group is the bare digest.
 */
const DIGEST_PATH =
  /\/(?:blobs|manifests)\/([a-z0-9]+(?:[+._-][a-z0-9]+)*:[0-9a-f]{32,})$/;

/** Extract the digest from a by-digest path, or null if it isn't one. */
const extractDigest = (path: string): string | null => {
  const m = path.match(DIGEST_PATH);
  return m ? m[1] : null;
};

/**
 * Build the cache key for a content-addressed object. Keyed by digest (not the
 * full path) so the same layer shared across repositories — e.g. a common
 * `debian` base layer under `library/nginx` and `library/node` — hits one cache
 * entry. Scoped to the upstream host to keep registries isolated.
 */
const digestCacheKey = (upstream: string, digest: string): string =>
  `https://${upstream}/__cache__/${encodeURIComponent(digest)}`;

/** One year — content-addressed data never changes under its digest. */
const IMMUTABLE_CACHE_CONTROL = "public, max-age=31536000, immutable";

/**
 * Default cap on what we store in the Cache API. Cloudflare rejects objects over
 * a per-plan limit (512 MB Free/Pro, larger on Business/Enterprise); a blob
 * above this is streamed straight through and never cached. 512 MB is the safe
 * floor — override via `maxCacheBytes` on higher plans.
 */
const DEFAULT_MAX_CACHE_BYTES = CFCacheLimit;

/**
 * Decide whether a content-addressed 200 is small enough to cache. Requires a
 * known `Content-Length` — without it we can't size the object, so skip rather
 * than risk a mid-stream `cache.put` rejection.
 */
function withinCacheLimit(response: Response, limit: number): boolean {
  const bytes = parseContentLength(response.headers);
  return bytes !== undefined && bytes <= limit;
}

function buildUpstreamHeaders(request: Request): Headers {
  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!STRIP_REQUEST_HEADERS.has(key.toLowerCase())) {
      headers.set(key, value);
    }
  });
  return headers;
}

interface TokenResponse {
  token?: string;
  access_token?: string;
  expires_in?: number;
}

/**
 * Exchange a Bearer challenge for an access token at the challenge's `realm`.
 * Uses Basic auth when credentials are supplied (anonymous otherwise). Optional
 * KV caching keyed by scope avoids a token round-trip on every request.
 */
export async function getToken(
  challenge: AuthChallenge,
  creds: Credentials | null,
  fetchImpl: FetchImpl,
  opts?: { kv?: KvLike; cachePrefix?: string },
): Promise<string | null> {
  const realm = challenge.params.realm;
  if (!realm) return null;

  const url = new URL(realm);
  if (challenge.params.service) url.searchParams.set("service", challenge.params.service);
  if (challenge.params.scope) url.searchParams.set("scope", challenge.params.scope);

  const cacheKey =
    opts?.kv && opts.cachePrefix
      ? `${opts.cachePrefix}${challenge.params.service ?? ""}|${challenge.params.scope ?? ""}|${creds?.username ?? "anon"}`
      : null;

  if (cacheKey && opts?.kv) {
    const cached = await opts.kv.get(cacheKey);
    if (cached) return cached;
  }

  const headers = new Headers();
  if (creds) headers.set("Authorization", basicAuthHeader(creds));

  const res = await fetchImpl(url.toString(), { method: "GET", headers });
  if (!res.ok) return null;

  const body = (await res.json()) as TokenResponse;
  const token = body.token ?? body.access_token ?? null;

  if (token && cacheKey && opts?.kv) {
    // Expire a little early to avoid using a token past its lifetime upstream.
    const ttl = Math.max(60, (body.expires_in ?? 300) - 30);
    await opts.kv.put(cacheKey, token, { expirationTtl: ttl });
  }

  return token;
}

/**
 * Proxy a single registry request to the upstream, transparently handling the
 * token-auth handshake and stripping auth challenges from the response.
 */
export async function proxyRegistryRequest(
  request: Request,
  opts: RegistryProxyOptions,
): Promise<Response> {
  const fetchImpl = opts.fetchImpl ?? mdbfetch;

  const incoming = new URL(request.url);
  const path = opts.rewritePath ? opts.rewritePath(incoming.pathname) : incoming.pathname;
  const upstreamUrl = `https://${opts.upstream}${path}${incoming.search}`;

  // Serve immutable digest-addressed content from the Cache API when possible.
  // The key is the digest, so identical layers shared across repositories hit
  // the same entry.
  const digest = extractDigest(path);
  const cacheable = request.method === "GET" && !!opts.cache && digest !== null;
  const cacheKey = digest ? digestCacheKey(opts.upstream, digest) : upstreamUrl;
  if (cacheable) {
    const hit = await opts.cache!.match(cacheKey);
    if (hit) return hit;
  }

  const baseHeaders = buildUpstreamHeaders(request);
  // Buffer the body once so the request can be retried after authentication.
  const body =
    request.method === "GET" || request.method === "HEAD"
      ? undefined
      : await request.arrayBuffer();

  const send = (url: string, auth?: string): Promise<Response> => {
    const headers = new Headers(baseHeaders);
    if (auth) headers.set("Authorization", auth);
    return fetchImpl(url, {
      method: request.method,
      headers,
      body: body as BodyInit | undefined,
      redirect: "manual",
    });
  };

  let url = upstreamUrl;
  let auth: string | undefined;
  let response = await send(url);

  if (response.status === 401) {
    const challenge = parseWwwAuthenticate(response.headers.get("www-authenticate"));
    if (challenge && challenge.scheme.toLowerCase() === "bearer") {
      const creds = opts.getCredentials ? await opts.getCredentials() : null;
      const token = await getToken(challenge, creds, fetchImpl, {
        kv: opts.kv,
        cachePrefix: opts.tokenCachePrefix,
      });
      if (token) {
        auth = `Bearer ${token}`;
        response = await send(url, auth);
      }
    }
  }

  // Follow redirects inside the worker rather than returning them to the
  // client. Registries (e.g. gcr.io) may answer with a *relative* redirect to
  // an on-host download path that still needs auth — a client following it
  // would land back on this mirror or drop the bearer. Only forward auth when
  // the next hop stays on the upstream host; CDN targets are pre-signed.
  let hops = 0;
  while (isRedirect(response.status) && hops < MAX_REDIRECTS) {
    const location = response.headers.get("location");
    if (!location) break;
    const next = new URL(location, url);
    const sameHost = next.host === new URL(url).host;
    response = await send(next.toString(), sameHost ? auth : undefined);
    url = next.toString();
    hops += 1;
  }

  const final = sanitizeResponse(response);

  // Persist content-addressed 200s. Force a long immutable Cache-Control so the
  // store accepts it regardless of upstream headers. The write runs in the
  // background (waitUntil) so the client streams without waiting on the cache.
  if (
    cacheable &&
    final.status === 200 &&
    withinCacheLimit(final, opts.maxCacheBytes ?? DEFAULT_MAX_CACHE_BYTES)
  ) {
    const headers = new Headers(final.headers);
    headers.set("cache-control", IMMUTABLE_CACHE_CONTROL);
    const cached = new Response(final.body, {
      status: 200,
      statusText: final.statusText,
      headers,
    });
    const write = opts.cache!.put(cacheKey, cached.clone()).catch(() => {});
    if (opts.waitUntil) opts.waitUntil(write);
    else await write;
    return cached;
  }

  return final;
}
