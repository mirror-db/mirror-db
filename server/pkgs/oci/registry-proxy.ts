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

/** Response headers stripped before returning to the client. */
const STRIP_RESPONSE_HEADERS = new Set([
  // Never leak an auth challenge — that is what makes clients prompt for login.
  "www-authenticate",
  // Hop-by-hop / connection-scoped headers.
  "connection",
  "keep-alive",
  "transfer-encoding",
]);

function buildUpstreamHeaders(request: Request): Headers {
  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!STRIP_REQUEST_HEADERS.has(key.toLowerCase())) {
      headers.set(key, value);
    }
  });
  return headers;
}

function sanitizeResponse(response: Response): Response {
  const headers = new Headers();
  response.headers.forEach((value, key) => {
    if (!STRIP_RESPONSE_HEADERS.has(key.toLowerCase())) {
      headers.set(key, value);
    }
  });
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
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
  const fetchImpl = opts.fetchImpl ?? (globalThis.fetch as FetchImpl);

  const incoming = new URL(request.url);
  const path = opts.rewritePath ? opts.rewritePath(incoming.pathname) : incoming.pathname;
  const upstreamUrl = `https://${opts.upstream}${path}${incoming.search}`;

  const baseHeaders = buildUpstreamHeaders(request);
  // Buffer the body once so the request can be retried after authentication.
  const body =
    request.method === "GET" || request.method === "HEAD"
      ? undefined
      : await request.arrayBuffer();

  const send = (auth?: string): Promise<Response> => {
    const headers = new Headers(baseHeaders);
    if (auth) headers.set("Authorization", auth);
    return fetchImpl(upstreamUrl, {
      method: request.method,
      headers,
      body: body as BodyInit | undefined,
      redirect: "manual",
    });
  };

  let response = await send();

  if (response.status === 401) {
    const challenge = parseWwwAuthenticate(response.headers.get("www-authenticate"));
    if (challenge && challenge.scheme.toLowerCase() === "bearer") {
      const creds = opts.getCredentials ? await opts.getCredentials() : null;
      const token = await getToken(challenge, creds, fetchImpl, {
        kv: opts.kv,
        cachePrefix: opts.tokenCachePrefix,
      });
      if (token) {
        response = await send(`Bearer ${token}`);
      }
    }
  }

  return sanitizeResponse(response);
}
