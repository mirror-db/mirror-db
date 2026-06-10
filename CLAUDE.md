# CLAUDE.md

Multi-registry OCI mirror on Cloudflare Workers. Presents upstream registries
(Docker Hub, ghcr, gcr, …) as **no-login-required** mirrors: clients pull
anonymously, the Worker handles upstream token auth, and credentials / auth
challenges never reach clients.

## Routing

`BASE_DOMAIN` = `mirs.uk`. Subdomain → host. `dcr.mirs.uk` → Docker Hub.

- [server/index.ts](server/index.ts) — entry. Parses subdomain, dispatches to a
  host from the `hosts` map, passes `ctx` through. 404 otherwise.
- [server/hosts/index.ts](server/hosts/index.ts) — aggregates all hosts into a
  `Map<subdomain, OciHost>`. Simple passthrough mirrors declared inline via
  `createRegistryHost`; hosts needing logic (login, path rewrite) live in their
  own subdir.

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

## Docker Hub host — [server/hosts/docker/](server/hosts/docker/)

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

## Layout

- `server/pkgs/oci/` — reusable OCI components (proxy, creds, www-authenticate,
  host factory). Shared across mirror sources.
- `server/hosts/<name>/` — per-host logic + `index.ts` exporting
  `{ host, fetch }`.
- Path alias: `@server/*` → `./server/*`.

## Commands

```
pnpm test          # unit (vitest, node env, cloudflare:workers stubbed)
pnpm test:e2e      # e2e: builds + spawns `wrangler dev --remote`, real pull
pnpm type-check
pnpm build
pnpm deploy        # build + wrangler deploy
pnpm cf-typegen    # regenerate worker-configuration.d.ts (KV binding `kv`, etc.)
```

E2E ([docker.e2e.test.ts](server/hosts/docker/docker.e2e.test.ts)) is
self-contained: `beforeAll` builds + spawns `wrangler dev --remote --host
dcr.mirs.uk`, `afterAll` kills the process group. Uses plain `fetch`.
**Local Docker Hub is firewalled** → e2e must use `--remote`.

[oci-test.sh](oci-test.sh): one live `docker pull` per mirror (manual,
post-deploy).

## Testing notes

- `cloudflare:workers` virtual module stubbed in
  [server/test-stubs/cloudflare-workers.ts](server/test-stubs/cloudflare-workers.ts).
- `caches` global absent in Node → `defaultCache()` returns `undefined` outside
  the runtime; caching is a no-op in unit tests unless a mock `cache` is passed.
- Node `Response(str)` does **not** auto-set `Content-Length` → cache tests must
  set it explicitly (size guard requires it).
