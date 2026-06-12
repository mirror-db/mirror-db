# CLAUDE.md

Multi-registry OCI mirror on Cloudflare Workers. Presents upstream registries
(Docker Hub, ghcr, gcr, …) as **no-login-required** mirrors: clients pull
anonymously, the Worker handles upstream token auth, and credentials / auth
challenges never reach clients.

## Routing

`BASE_DOMAIN` = `mirs.uk`. Every mirror implements the `Mirror` interface
([server/types.ts](server/types.ts)): `name`, optional `host` (subdomain route,
e.g. `dcr.mirs.uk` → Docker Hub) or `path` (single word, matched as the first
URL segment, e.g. `mirs.uk/npm/...`), `fetch(request, ctx?)`, optional
`status()`.

- [server/index.ts](server/index.ts) — entry. Runs `relayMiddleware` first,
  then parses subdomain from host header. Subdomain mirror wins; on bare `@`
  splits pathname, looks up first segment in `mirrorsByPath`, then falls through
  to Hono API (`/api/`), then static SPA.
- [server/mirrors/index.ts](server/mirrors/index.ts) — aggregates every mirror
  into `mirrors` array plus lookups: `mirrorsBySubdomain`, `mirrorsByName`,
  `mirrorsByPath`. Simple passthrough registries declared inline via
  `createRegistryHost`; mirrors needing logic (login, path rewrite, an index)
  live in their own subdir under `mirrors/`.
- [server/api/index.ts](server/api/index.ts) — Hono app at `/api/`. Endpoints:
  - `GET /api/mirrors` — JSON manifest of all mirrors (for relay discovery).
  - `GET /api/status/:name` — per-mirror status snapshot.

### Relay — [server/relay/index.ts](server/relay/index.ts)

`relayMiddleware(request): Request` — pure request rewrite for domestic relay
servers that access all mirrors through a single domain.

Convention:
- `/@relay/@<subdomain>/<path>` → rewrites host to `<subdomain>.<BASE_DOMAIN>`,
  path to `/<path>`. E.g. `/@relay/@dcr/v2/...` → `dcr.mirs.uk/v2/...`.
- `/@relay/<path>` → strips prefix, keeps same host.
  E.g. `/@relay/npm/react` → `mirs.uk/npm/react`.

After rewrite, request is indistinguishable from a direct request — downstream
routing works unchanged. Relay client sends `X-MDB-Relay-Host: <its-domain>` so
mirrors that rewrite absolute URLs (npm, pypi) can emit correct links via
`MirrorContext.relay.host`.

### Mirrors

| sub | upstream |
|-----|----------|
| dcr | registry-1.docker.io |
| ghcr | ghcr.io |
| gcr | gcr.io |
| k8s | registry.k8s.io |
| quay | quay.io |
| mcr | mcr.microsoft.com |
| nvcr | nvcr.io |
| ocr | container-registry.oracle.com |

## Core proxy — [server/pkgs/oci/registry-proxy.ts](server/pkgs/oci/registry-proxy.ts)

`proxyRegistryRequest(request, opts)`. Dependency-injected (`fetchImpl`,
`getCredentials`, `kv`, `cache`, `waitUntil`) so it unit-tests without the
Workers runtime.

Flow:
1. Rewrite path (`opts.rewritePath`), build upstream URL.
2. **Cache lookup** (see below) for digest-addressed GETs.
3. Send with stripped request headers (drop `authorization`, `host`, `cookie`,
   `cf-*`, `x-forwarded-*`).
4. On `401` Bearer challenge → `getToken` (Basic auth if creds, optional KV
   token cache) → retry with `Authorization: Bearer`.
5. **Follow redirects server-side** (max 5). Bearer forwarded only when next hop
   stays on the upstream host; cross-host CDN targets (pre-signed) get no auth.
6. `sanitizeResponse` strips response headers, then cache write.

### Caching (Cache API)

Only **content-addressed** GETs cached: `/blobs/<digest>` and
`/manifests/<digest>`. Tag manifests (`/manifests/latest`) are mutable → never
cached.

- **Key = digest, not path** (`digestCacheKey`): same layer across repos
  (`library/nginx` + `library/node`) shares one entry. Scoped per upstream host
  to contain cache-poisoning blast radius (a bad upstream can't poison another
  registry's digest space; client self-verifies sha256 anyway → DoS-only).
- Forced `Cache-Control: public, max-age=31536000, immutable` on the stored copy.
- **Size guard** `maxCacheBytes` (default 512MB = CF Free/Pro per-object limit).
  Skip when `Content-Length` unknown or over limit (avoids mid-stream
  `cache.put` reject). Override on Business/Enterprise.
- Write runs via `ctx.waitUntil` → client streams immediately, cache fills in
  background.

### Header hygiene (anti-leak)

`STRIP_RESPONSE_HEADERS`: `www-authenticate` (no login prompt), `set-cookie` /
`set-cookie2` (digest-shared cache would replay one client's cookie to all),
`vary` (digest content is request-header-independent → avoid spurious misses),
`age`, hop-by-hop (`connection`/`keep-alive`/`transfer-encoding`). Stripping
happens **before** cache write, so neither cache nor client sees them.

## Docker Hub mirror — [server/mirrors/docker/](server/mirrors/docker/)

- `normalizeDockerPath`: `/v2/<single>/...` → `/v2/library/<single>/...`.
- `resolveCredentials`: KV `creds:docker-hub` → fallback env
  `DOCKER_HUB_USER`/`DOCKER_HUB_PASSWORD`.
- `POST /auth/login`: admin-guarded (`x-admin-token` == `DOCKER_HUB_PASSWORD`),
  validates creds via token fetch, persists to KV. Returns `{ ok, username }`
  only — **never the password**.
- Handler exported as `handle` (NOT `fetch` — avoids shadowing global `fetch`).

## Credential safety (hard rule)

Password lives only in: KV value + outbound `Authorization` to the token
endpoint. **Never** in responses, logs, or error messages. `.env` holds a real
Docker Hub PAT and is gitignored — never echo it.

## Upstream fetch — [server/pkgs/fetch/](server/pkgs/fetch/)

Shared helpers for talking to upstreams. All outbound proxy traffic should go
through these, not bare `fetch`, so user-agent + rate-limiting are uniform.

- **`mdbfetch(input, init)`** — `typeof fetch`. The base primitive: wraps args in
  a `Request`, sets `user-agent: mirror-db/0.0.0-dev` ([`MdbUserAgent`](server/pkgs/fetch/const.ts))
  if absent, and runs every call through a shared `p-limit` gate
  (`MdbMaxConcurrentRequests` = 64 concurrent). It is the default `fetchImpl`
  for both proxies ([oci/registry-proxy.ts](server/pkgs/oci/registry-proxy.ts),
  [apt/proxy.ts](server/pkgs/apt/proxy.ts)) and `AptRepo`. **Gotcha:** it hands a
  `Request` to global `fetch`, not `(url, init)` — a `fetch` stub must read
  `input.url`, not `input.toString()`.
- **`ezfetch(reqInfo, parts?, init?)`** — ergonomic `mdbfetch`. `buildRequest`
  `url-join`s any `parts` onto the URL and defaults `redirect: "follow"`. Use
  when joining path segments to a base; returns the `Response`.
- **`cachedfetch`** — `ezfetch` + read-through Cache API via the module-level
  `mdbCache` (`MdbCacheManager`): `match` first, on miss `mdbfetch` then `save` a
  clone. Keyed by **`req.url`** (not digest), guarded by `isCacheable`
  (size ≤ `MdbCacheSizeLimit`, no `content-range`, bounded `range`), with
  in-flight write dedup. Used by the **APT proxy** ([apt/proxy.ts](server/pkgs/apt/proxy.ts))
  and `AptRepo`'s Release fetch — both key on the bare upstream URL. The **OCI
  proxy** does *not* use it: it needs digest-keyed entries + immutable
  `Cache-Control` rewriting, so it calls `mdbfetch` + an injected `cache` directly.
- **`sanitizeResponse(upstream, opts?)`** — strip dangerous headers
  (`STRIP_RESPONSE_HEADERS`: `www-authenticate`, `set-cookie`, `vary`, `age`,
  hop-by-hop) from an upstream response before serving to clients. Optional
  `cacheControl` override on 200s. Shared by both OCI and APT proxies.
- Constants in [const.ts](server/pkgs/fetch/const.ts): `CFCacheLimit` (512 MB,
  CF Free/Pro per-object cap) = `MdbCacheSizeLimit`, `MdbMaxConcurrentRequests`,
  `MdbCacheName` (`"upstream"`), `MdbUserAgent`.

## Layout

- `server/pkgs/oci/` — reusable OCI components (proxy, creds, www-authenticate,
  host factory). `server/pkgs/apt/` — APT repo index + proxy + status.
  `server/pkgs/fetch/` — shared upstream fetch + cache + sanitize helpers.
  `server/pkgs/web-list/` — upstream directory-listing proxy (see below).
- `server/relay/` — relay middleware (request rewrite for single-domain access).
- `server/api/` — Hono API routes.
- `server/mirrors/<name>/` — per-mirror logic + `index.ts` exporting a `Mirror`
  (`{ name, host? | path?, fetch, status? }`). `server/mirrors/index.ts`
  aggregates them.
- `tools/relay/` — Go relay server (domestic proxy that talks to the Worker via
  `/@relay/`).
- Path alias: `@server/*` → `./server/*`.

## Web listing proxy — [server/pkgs/web-list/](server/pkgs/web-list/)

Generic proxy for upstream sites that serve Apache/nginx autoindex HTML. Used by
the APT mirror but reusable for any directory-listing upstream.

- **`WebListFs`** — read-only FS abstraction over an upstream listing site. Uses
  `cachedfetch` by default. `readdir(rel)` streams HTML through `HTMLRewriter`,
  extracting `<tr>/<th>/<td>/<a>` into a 2D cell array, guesses column semantics
  (Name/Last modified/Size) via regex, and cleans values into typed
  `WebListEntry` objects.
- **`WebListProxy`** — unified request handler exposing three access modes:
  1. **JSON API**: `?format=json` on any directory → `{ path, entries }`.
  2. **Web listing**: directory GET → injected `render(entries, path)` callback
     (or use `defaultRender` for a minimal `<ul>` page).
  3. **WebDAV (read-only, anonymous)**: `PROPFIND` Depth 0/1 → 207 multistatus,
     `OPTIONS` → DAV:1. Supports Windows/macOS/Linux native mount.
  - `fetchHook(path, request)` — optional hook to intercept requests before
    default handling (APT proxy uses this for existence enforcement + caching).
  - `baseHref` — prefix for `<D:href>` in PROPFIND responses (must match the
    mount point for Windows WebClient compatibility).
- **`parseListing`** — two-stage: `collectRows` (HTMLRewriter, Workers-only) →
  `rowsToEntries` (pure, testable in Node). Handles text-chunk deduplication
  between `<td>` and nested `<a>` via `inAnchor` depth tracking.
- **`webdav.ts`** — hand-crafted RFC 4918 class 1 responses (no XML library
  needed). `propfindResponse`, `optionsResponse`, `methodNotAllowed`.

## Commands

```
pnpm test          # unit: both "node" + "workers" vitest projects
pnpm test:e2e      # e2e: builds + spawns `wrangler dev --remote`, real pull
pnpm type-check
pnpm build
pnpm deploy        # build + wrangler deploy
pnpm cf-typegen    # regenerate worker-configuration.d.ts (KV binding `kv`, etc.)
```

E2E ([docker.e2e.test.ts](server/mirrors/docker/docker.e2e.test.ts)) is
self-contained: `beforeAll` builds + spawns `wrangler dev --remote --host
dcr.mirs.uk`, `afterAll` kills the process group. Uses plain `fetch`.
**Local Docker Hub is firewalled** → e2e must use `--remote`.

[oci-test.sh](oci-test.sh): one live `docker pull` per mirror (manual,
post-deploy).

## Testing notes

- Two vitest projects ([vitest.config.ts](vitest.config.ts)):
  - **`node`** — `*.test.ts`. Plain Node env, `cloudflare:workers` stubbed
    ([server/test-stubs/cloudflare-workers.ts](server/test-stubs/cloudflare-workers.ts)).
    Pure logic, mocked fetch.
  - **`workers`** — `*.workers.test.ts`. Runs inside workerd via
    `@cloudflare/vitest-pool-workers` ([vitest.workers.config.ts](vitest.workers.config.ts)).
    Real `HTMLRewriter`, `caches`, etc. Used by `web-list` parsing tests.
- `caches` global absent in Node → `defaultCache()` returns `undefined` outside
  the runtime; caching is a no-op in unit tests unless a mock `cache` is passed.
- Node `Response(str)` does **not** auto-set `Content-Length` → cache tests must
  set it explicitly (size guard requires it).
