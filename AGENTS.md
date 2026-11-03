# CLAUDE.md

Multi-registry OCI mirror on Cloudflare Workers. Presents upstream registries
(Docker Hub, ghcr, gcr, …) as **no-login-required** mirrors: clients pull
anonymously, the Worker handles upstream token auth, and credentials / auth
challenges never reach clients.

## Routing

`BASE_DOMAIN` = `mirs.uk`. Every mirror implements the `Mirror` interface
([server/types.ts](server/types.ts)): `name`, optional `host` (subdomain route,
e.g. `dcr.mirs.uk` → Docker Hub) or `path` (single word, matched as the first
URL segment, e.g. `mirs.uk/npm/...`), optional `keepHTTP` (serve over plain HTTP
without redirect), `fetch(request)`, optional `status()`.

- [server/index.ts](server/index.ts) — entry. Parses subdomain from host header.
  Subdomain mirror wins; on bare `@` splits pathname, looks up first segment in
  `mirrorsByPath`, then the mount switches: `/speedtest/`, `/keys/`, `/api/`,
  `/scripts/`. Everything else is a static asset or a **real 404** — the SPA
  shell is only served for `/` and `/about` (the routes `src/router/` owns).
  An unknown first segment must never fall through to the SPA: a 404 page
  ([server/not-found.ts](server/not-found.ts)) answers in two flavours —
  HTML + countdown redirect home for a browser, plain text + the mount list for
  everything else. A typo in a package manager config has to look like a typo.
- [server/mirrors/index.ts](server/mirrors/index.ts) — aggregates every mirror
  into `mirrors` array plus lookups: `mirrorsBySubdomain`, `mirrorsByName`,
  `mirrorsByPath`. Simple passthrough registries declared inline via
  `createRegistryHost`; mirrors needing logic (login, path rewrite, an index)
  live in their own subdir under `mirrors/`.
- [server/api/index.ts](server/api/index.ts) — Hono app at `/api/`. Endpoints:
  - `GET /api/mirrors` — JSON manifest of all mirrors (for relay discovery).
    Includes `keepHTTP` flag for mirrors that allow plain HTTP.
  - `GET /api/status/:name` — per-mirror status snapshot.
- [server/keys/index.ts](server/keys/index.ts) — Hono app at `/keys/` on the
  bare domain. Proxies common signing keys (PGP + SSH) so domestic clients fetch
  them through the mirror. See [Key proxy](#key-proxy--serverkeys) below.
  A key entry may list **several URLs**: they are fetched and merged into one
  keyring (`PGPPubKeyHandler.fromUrls`), normalised to binary. Syncthing needs
  this — it signs with two certificates and publishes only one.
- [server/scripts/](server/scripts/) — Hono app at `/scripts/` on the bare
  domain. Generates install scripts for OCI registries that return correct
  mirror URLs based on `X-MDB-Relay-Host` header.
- [server/speedtest.ts](server/speedtest.ts) — `GET /speedtest/{mb}` on bare
  domain. Streams random bytes (1–1024 MB) via `ReadableStream` +
  `crypto.getRandomValues`. Ignores `?passthrough` (Worker is always origin).

### Relay info — [server/relay.ts](server/relay.ts)

`relayInfo(request)` — extracts relay context from `X-MDB-Relay-Host` header.

The relay server (Go binary) sets `X-MDB-Relay-Host` to its own base domain when
proxying requests to the Worker. Mirrors that generate absolute URLs (Helm charts,
npm, pypi, install scripts) use this to emit URLs pointing back through the relay
instead of directly to the upstream Worker.

**Rule: every absolute reference in a response body or `Location` is built from
`relayInfo().origin`, never from `request.url`.** `request.url` is always the
Worker's origin; using it silently sends the client past the relay on its next
request. See [ADR 0002](docs/adr/0002-absolute-references-and-relay.md).

- `normalizeRelayHost()` validates the header as a bare hostname
  (`[a-z0-9-]` labels, no port/path/userinfo, ≤253 chars, lowercase, no trailing
  dot) and **falls back to `BASE_DOMAIN`** on anything else. The project is open,
  so anyone can send the header; it may pick a hostname in a URL, never change
  the URL's structure.
- Directory redirects stay **relative** and use `308` (the APT `dists` → `dists/`
  case). A permanent `301` to an absolute Worker URL would pin clients to the
  Worker forever.

Returns `{ baseDomain, origin }`:
- `baseDomain`: relay's domain if present, else `env.BASE_DOMAIN`
- `origin`: `https://${baseDomain}` or request's own origin

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
| lscr | lscr.io |

Path-routed file trees ([web-mirrors.ts](server/mirrors/web-mirrors.ts)) pair
with the APT mirrors by name: `/debian-cd/` + `/debian-cloud/`,
`/ubuntu-cd/` + `/ubuntu-cloud/` + `/ubuntu-releases/`. A name is a label; the
**base URL must be a listing root** (an `autoindex` page, not a redirect and not
a 404) — `cuda` is `…/compute/cuda/repos/` and only that path lists,
`microsoft` is `packages.microsoft.com/repos/` (the site root also exposes
`config/`, `keys/`, `yumrepos/` as site chrome). See
[ADR 0007](docs/adr/0007-listing-mounts-declare-their-root.md).

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
  for OCI registry-proxy and Docker credential validation. **Gotcha:** it hands a
  `Request` to global `fetch`, not `(url, init)` — a `fetch` stub must read
  `input.url`, not `input.toString()`.
- **`ezfetch(reqInfo, parts?, init?)`** — ergonomic `mdbfetch`. `buildRequest`
  `url-join`s any `parts` onto the URL and defaults `redirect: "follow"`. Use
  when joining path segments to a base; returns the `Response`.
  `cachedfetch` shares the same overloaded signature.
- **`cachedfetch`** — `ezfetch` + read-through Cache API via the module-level
  `mdbCache` (`MdbCacheManager`): cache strategy by method: GET full cache (match
  → fetch → save); HEAD match with `ignoreMethod: true` (reuse GET cache), miss
  fetches directly without save; other methods bypass cache entirely. Guards: only
  caches 2xx GET, skips Range requests (206 partial would poison full-URL key),
  size ≤ `MdbCacheSizeLimit`, no `content-range`, in-flight write dedup. Used by
  the **APT proxy** ([apt/proxy.ts](server/pkgs/apt/proxy.ts)), `AptRepo`'s
  Release fetch, **`upstreamProxy`**, and PyPI. The **OCI proxy** does *not* use
  it: it needs digest-keyed entries + immutable `Cache-Control` rewriting, so it
  calls `mdbfetch` + an injected `cache` directly.

  **What can be stored (learned the hard way):** the Cache API keys by URL
  only and **silently refuses to store a response carrying `Vary`** — verified
  in workerd, `cache.match` returns nothing. `save()` therefore stores the
  sanitised header set (`STRIP_RESPONSE_HEADERS`), not whatever the upstream
  sent. It also materialises document bodies before `put` (≤8 MB, or any
  textual body): a caller that transforms the response consumes the original
  body, and putting the unread branch of that tee never completes. Binary
  bodies keep streaming. Both failure modes are silent — the cache simply stays
  empty — so `cache.workers.test.ts` covers them.

  **Freshness (ADR 0003):** `save()` stamps the entry with `x-mdb-stored-at` and
  `x-mdb-revalidate-after` (default `MdbRevalidateIntervalMs` = 10 min) and lifts
  the advertised `max-age` up to that floor (`liftCacheControl`; `no-store` /
  `no-cache` are never lifted, and `sanitizeResponse({cacheControl})` still wins
  downstream). A hit whose interval has elapsed is re-checked with a conditional
  `HEAD` (`If-None-Match` / `If-Modified-Since`): `304` re-stamps in place, `200`
  refetches + replaces, `404`/`410` drops the entry, an unreachable source keeps
  serving the stored copy. The Cache API has no store-time notion, so this has to
  live in the stored headers — which are then **stripped on the way out**
  (`mark()`), together with setting `x-mdb-cache: hit|revalidated|miss|bypass`.
- **`sanitizeResponse(upstream, opts?)`** — strip dangerous headers
  (`STRIP_RESPONSE_HEADERS`: `www-authenticate`, `set-cookie`, `vary`, `age`,
  hop-by-hop) from an upstream response before serving to clients. Optional
  `cacheControl` override on 200s. Shared by both OCI and APT proxies.
- Constants in [const.ts](server/pkgs/fetch/const.ts): `CFCacheLimit` (512 MB,
  CF Free/Pro per-object cap) = `MdbCacheSizeLimit`, `MdbMaxConcurrentRequests`,
  `MdbCacheName` (`"upstream"`), `MdbUserAgent`, `MdbRevalidateIntervalMs`
  (10 min).

## Layout

- `server/pkgs/oci/` — reusable OCI components (proxy, creds, www-authenticate,
  host factory). `server/pkgs/apt/` — APT repo index + proxy + status.
  `server/pkgs/fetch/` — shared upstream fetch + cache + sanitize helpers.
  `server/pkgs/web-list/` — upstream directory-listing proxy (see below).
- `server/mirrors/proxy/` — composable hook-based HTTP proxy builder (see below).
- `server/relay/` — relay middleware (request rewrite for single-domain access).
- `server/api/` — Hono API routes.
- `server/keys/` — PGP/SSH key proxy at `/keys/` (see above).
- `server/mirrors/<name>/` — per-mirror logic + `index.ts` exporting a `Mirror`
  (`{ name, host? | path?, fetch, status? }`). `server/mirrors/index.ts`
  aggregates them.
- `tools/relay/` — Go relay server (domestic proxy that talks to the Worker via
  `/@relay/`). See [Relay server](#relay-server) below.
- Path alias: `@server/*` → `./server/*`.

## Upstream proxy builder — [server/mirrors/proxy/](server/mirrors/proxy/)

Composable hook chain for non-OCI mirrors that just need prefix-strip + base-URL
+ cache + sanitize. Used by `createPassthroughMirror` (web-mirrors) and npm.

- **`upstreamProxy(upstream)`** — factory returning a `ProxyRequest` pre-wired
  with: `stripPrefix` → `setBaseUrl` → `filterHeaders` (Range + extras) →
  `standardizeUserAgent` → `followRedirects` → `sanitize` (post). Fetches via
  `cachedfetch` internally.
- **`Upstream`** interface: `{ url, prefix?, passthroughHeaders? }`.
- **`ProxyRequest`** class: `.pre(hook)` / `.post(hook)` / `.apply(req)`.
  Pre-hooks transform the outbound Request; post-hooks transform the Response.
  Post-hooks receive the **original** request (pre-rewrite), useful for URL
  rewriting in response bodies.
- Pre-hooks: `rewritePath(fn)`, `stripPrefix`, `setBaseUrl` (preserves query),
  `stripSearch`, `filterHeaders`, `standardizeUserAgent`, `followRedirects`.
- Post-hooks: `sanitize` (strips dangerous headers), `rewriteBody(fn)` (streaming
  text transform via `TransformStream`; skips non-text; deletes `content-length`).

Example — npm mirror in full:
```ts
const proxy = upstreamProxy({ url: UPSTREAM, prefix: PREFIX })
  .post(rewriteBody((body, req) => {
    const mirrorBase = `${new URL(req.url).origin}${PREFIX}/`;
    return body.replaceAll(`${UPSTREAM}/`, mirrorBase);
  }));
export const npm: Mirror = { name: "npm", path: PATH, fetch: (r) => proxy.apply(r) };
```

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

## Helm chart mirror — [server/mirrors/charts/](server/mirrors/charts/)

Single path-routed mirror (`/charts/`) hosting many Helm repos, one per entry in
[repos.ts](server/mirrors/charts/repos.ts). Add a repo with
`helm repo add <name> https://mirs.uk/charts/<name>`.

- **`HelmRepoKeeper`** ([keeper.ts](server/mirrors/charts/keeper.ts)) — one
  isolate-scoped instance per repo. `refresh()` fetches upstream `index.yaml`
  (via `ezfetch`), resolves every chart `urls` entry to an absolute URL, hashes
  it with `uuidv5` into an opaque `res/<uuid>/<filename>` **relative** path, and
  records `uuid → absolute url` in `resMap`. The index is re-dumped with
  `js-yaml` under **`FAILSAFE_SCHEMA`** (`lineWidth: -1`) — critical: the default
  YAML-1.1 schema coerces unquoted scalars and would rewrite a chart
  `version: 1.10` into the float `1.1` (and truncate large integer digests),
  silently corrupting the served index. FAILSAFE keeps every scalar a string so
  the load → mutate-urls → dump roundtrip is lossless. The relative `res/` path
  makes `helm` pull tarballs back through the mirror.
- `res/<uuid>/*` responses pass through `sanitizeResponse` (strip
  `set-cookie`/`www-authenticate`/etc.) before serving — chart tarballs come
  from arbitrary CDNs (S3, GitHub). Upstream index fetch failures surface as 502.
- Routes (under `/charts/<repo>/`): `index.yaml` (rewritten index),
  `generated` (upstream timestamp), `res/<uuid>/info` (original url),
  `res/<uuid>/*` (tarball, streamed + cached via `cachedfetch`). Bare `<repo>`
  returns the repo metadata JSON.
- `resMap` is rebuilt deterministically from the same index, so a `res` request
  landing on a cold isolate just re-derives the map (`waitInitialized`). Same
  `uuid` for the same absolute url across isolates → cache hits are stable.
- Why rewrite to opaque uuids instead of passing urls through: chart urls can be
  cross-host (e.g. gitlab → `gitlab-charts.s3.amazonaws.com`), and only urls
  present in a fetched index ever enter `resMap` → not an open proxy.
- Deps: `js-yaml` (pure-ESM, **named** `load`/`dump` exports — no default),
  `uuid` (`v5` + `NIL`).

## Key proxy — [server/keys/](server/keys/)

Hono app at `/keys/` on the bare domain. Fronts common PGP signing keys and SSH
public-key lists so domestic clients fetch them through the mirror instead of
reaching upstreams directly. Mounted in [server/index.ts](server/index.ts) via
`firstSeg === "keys"`, before the `/api` fallthrough.

- **`SrvPGPKey(urlProvider)`** ([utils/pgp-key.ts](server/keys/utils/pgp-key.ts))
  — sub-app serving one PGP key per `:keyname`, resolved to upstream URLs by
  `urlProvider`. A provider may return **several URLs**; they are fetched and
  merged into one binary keyring, because apt/sqv reject a signature whose
  certificate is unknown — Debian signs its index with three certificates, and
  Ubuntu's keyring holds three of them.

  Routes: `GET /:keyname` (raw bytes; `.pgp`/`.asc` ext switches format),
  `/:keyname/pgp` (binary), `/:keyname/asc` (armored), `/:keyname/in-sources`
  (armored, formatted for a deb822 `Signed-By:` field — the equivalent of
  `sed -e 's/^$/./' -e '1!s/^/ /'`), `/:keyname/info`
  (`{ algorithm, bits, fingerprint, fingerprints, creationTime }`),
  `POST /:keyname` (verify a cleartext-signed message body against any key).

  Parsing is only needed for `/asc`, `/in-sources`, `/info` and `POST`; the
  bytes served for `/` and `/pgp` are the published file, normalised to binary
  when it parses. An unknown name is a **404** (`UnknownKeyError`), any other
  failure a 502.
- **`SrvSSHKeyList(urlProvider)`** ([utils/ssh-key.ts](server/keys/utils/ssh-key.ts))
  — sub-app serving a user's SSH `authorized_keys` list. `GET /:keyname` returns
  raw text; `/:keyname/info` returns parsed keys with md5 (via `spark-md5`) +
  sha256 (`crypto.subtle`) fingerprints.
- Key providers under [pgp/](server/keys/pgp/): `debianKeyName2Url` maps a
  codename/version (`bookworm`, `13`, `-security`, `-release`) to a
  `ftp-master.debian.org` archive key; `repo2KeyUrl` is a static table keyed by
  **APT mount name**, so `/keys/apt/<mirror>` resolves for every mirror. The
  only exception is `debian-ports`: its archive key ships only inside
  `debian-ports-archive-keyring_*.deb`, and the keyserver copy has no
  self-signature (apt answers `ERRSIG`), so it answers 404 on purpose.
- Routes mounted: `/debian/:codename`, `/apt/:repo`, `/keybase/:user`,
  `/gh|github/:user`, `/gitlab/:user`.
- Upstream fetch goes through `ezfetch` (no read-through cache yet — these keys
  are highly cacheable, so `cachedfetch` is a candidate swap).
- Deps: `openpgp` (heaviest single dependency — drives bundle to ~755 KB;
  needed for `/info` + `POST` verify), `spark-md5` (SSH md5 fingerprints).

## Docs

- [CONTEXT.md](CONTEXT.md) — the project's glossary (mirror, mount, front door,
  listing site, res resource, relay, absolute reference, cache key…). Keep it a
  glossary: no implementation detail.
- [docs/adr/](docs/adr/) — decisions, including rejected ones (0001 mirror
  kinds, 0005 registry token realm scope, 0006 external probes). A rejected ADR
  keeps the reasoning so it is not re-proposed from scratch.
- [TODO.md](TODO.md) — reviewed-but-open items (dead upstreams, a passthrough
  that needs a token, the 48 MB PyPI index) with the evidence that produced
  them.

## Known limitations

- `/pypi/simple/` is ~48 MB uncompressed upstream (7.6 MB gzip). The mirror
  requests identity, so a cache miss transfers the full 48 MB once and the
  read-through cache then serves it for the 10-minute revalidation interval.
  Compressing the upstream hop is not implemented.
- `gcr.io` times out from some networks (verified from a CN egress: all of
  `gcr.io`, `eu.gcr.io`, `us.gcr.io`, `mirror.gcr.io` answer `401` + a valid
  token realm over a working path). Not an upstream shutdown — treat as a path
  problem when it shows up.
- `apt.syncthing.net` serves a real archive (`dists/syncthing/…`) but no
  directory listings — `/dists/` is `404` and a wrong component answers `403`.
  The suite list is therefore explicit, and the mount's HTML/JSON listing is
  `404` by design.
- `packages.opentofu.org/opentofu/tofu/any/` is not an APT tree (packagecloud
  web UI; `dists/` is 404 at every path tried).
- `packages.k8s.io` returns `403` for the `isv:/kubernetes:/` prefix without a
  token.
- Syncthing signs `dists/syncthing` with **two** certificates: the current one
  (`FBA2E162…`) that `syncthing.net/release-key.gpg` ships, and the 2014 key
  (`37C84554…`) it no longer publishes. A client with only the published key
  gets `Missing key 37C84554…` from `sqv` and apt skips the repository, so
  `/keys/apt/syncthing` serves both (the second from keyserver.ubuntu.com, which
  keeps its self-signatures). Sequoi rejects that certificate as *bound* because
  its self-signature is SHA-1 — a warning, not an error: `sqv` exits 0.
- APT `pool/` files are proxied with `ezfetch` and **do not forward `Range`**:
  a resuming client gets `200` from byte 0 and apt restarts the download. Not
  fixed yet; `dists/` files and by-hash objects are unaffected.

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

## Relay server — [tools/relay/](tools/relay/)

Go binary (`relay serve`) — domestic reverse proxy sitting between end-users and
the Worker. Discovers mirrors from upstream, terminates TLS via certmagic, proxies
all traffic by replacing relay domain with upstream domain while preserving paths.

### Architecture

```
Client → relay (HTTPS) → Worker (CF edge)
Client → relay A → relay B → Worker   (multi-tier, not yet supported)
```

Relay routing:
1. Client requests `dcr.relay.example.com/v2/library/nginx/manifests/latest`
2. Relay replaces host: `dcr.relay.example.com` → `dcr.mirs.uk`
3. Relay forwards to upstream with Host: `dcr.mirs.uk`, original path, and `X-MDB-Relay-Host: relay.example.com`
4. Worker routes normally based on subdomain, uses `X-MDB-Relay-Host` for script generation

### Config — [tools/relay/config/](tools/relay/config/)

YAML (`/etc/mirror-relay/config.yaml`) + env override (dots → underscores).

```yaml
relay:
  # disguise_port: 344      # SSH-disguised TLS listener
  base_domains:
    - domain: relay.example.com
      cf_api_token: ""

upstream:
  url: "https://mirs.uk"
  # disguise_port: 344       # connect via SSH-disguised TLS
  # proxy: ""                # http(s)/socks5 proxy; bypasses edge selection

cf:                          # edge selection; every key is optional
  # latency_capacity: 64     # entries held in the latency pool
  # speed_capacity: 8
  # top_n: 3                 # transports select randomly within Top-N
  # seed_count: 10           # latency results before Initialize returns
  # speed_seed_count: 3      # bootstrap IPs speed-probed during Initialize
  # latency_url: ""          # default: cloudflare.com/cdn-cgi/trace
  # speed_url: ""            # default: speed.cloudflare.com/__down
  # latency_min_score: 250   # reciprocal score; 250 ≈ "answers within 4s"
  # speed_min_score: 65536   # = below 64KiB/s is discarded
  # penalty: 0.5             # score multiplier for a failed request
  # scan_interval: 1h        # scan address pools for new candidates
  # patrol_latency_interval: 5m
  # patrol_speed_interval: 30m
  # recruit_interval: 10m    # small scan; keeps pools from draining
  # recruit_samples: 2
  # init_timeout: 30s        # startup wait; exceeding it is not fatal
  # patrol_intensity: 1      # scan budget ceiling vs the pre-rewrite Refresh
  #                          # (≈6k candidates); 0.5 = half, capped at 4
  # candidates_file: ""      # default: <config dir>/cf-candidates.csv
  # snapshot_interval: 1h    # also written on shutdown
  # bootstrap_host: ""       # default: hostname of upstream.url

acme:
  source: acme               # "acme" | "upstream" | URL origin
```

`acme.source`:
- `acme` — DNS-01 via Let's Encrypt (needs `cf_api_token` per domain).
- `upstream` — sync cert-pack from `upstream.url`.
- URL (e.g. `https://upper.example.com`) — sync cert-pack from that origin.

### Upstream transport

Selection priority: `proxy` > `disguise` > edge selection.

- **Proxy** (`upstream.proxy`): when set, all upstream traffic routes through an
  `http`/`https`/`socks5` proxy. `buildProxyTransport` uses `http.Transport.Proxy`
  for http(s) and `golang.org/x/net/proxy` for socks5(h).
- **SSH disguise** (`upstream.disguise_port`): `disguiseDialTLS` exchanges SSH
  banner then does TLS handshake. Bypasses HTTPS-targeted DPI throttling.
- **Edge selection** (default): see below.

The first two are explicit operator decisions and skip edge selection entirely —
picking a Cloudflare IP would override their intent.

### Edge selection — [tools/relay/server/edge.go](tools/relay/server/edge.go)

Ranks Cloudflare anycast IPs from this host's vantage point and serves through
the best ones. Two pools, because the winner differs by workload: **latency**
(time to first byte; decides a manifest fetch) and **speed** (bytes/sec; decides
a layer download). Both are maintained; requests currently go through the
latency pool, and per-request routing is the next step.

Startup calls `engine.Initialize` under `cf.init_timeout`. **Failure is logged,
not fatal:** the engine keeps running, patrol and recruit fill the pools in the
background, and requests fall back to unoptimised routing until they do. A
measurement problem must not become an outage.

`GET /.mirror-relay/pool` (JWT-guarded, like `/cert-pack` and `/mirrors`) reports
both pools, the gate verdict and which pool is serving — the answer to "why is
traffic going to that IP".

A **patrol** re-measures every member (bounded by pool capacity, every 5m); a
**scan** explores the address pools for new candidates (hourly, the expensive
one); a **recruit** is a small scan on a short cadence.

Startup order: `Start` runs **one full scan immediately**, then starts the
periodic jobs. Initialization only seeds a handful of addresses, and every timer
is anchored at `Start` — so a relay restarting more often than `scan_interval`
would otherwise never scan at all (ADR-0014 in `cft/docs/adr/`). The scan logs one
line when it finishes: `scan: P probed, N responded, K admitted in Xs`.

Two startup paths must tolerate a transient: the cert-pack fetch retries three
times before giving up (a parent restarting at that exact second used to kill the
consumer), and until `/api/mirrors` has loaded once the watcher retries every
15 s instead of every 10 minutes (a relay with no manifest answers 502 to
everything, so one unlucky edge address at startup was ten minutes of downtime).

`cf.patrol_intensity` sets the scan's candidate budget, measured against what the
pre-rewrite pool manager generated — one per `/24` over the measured usable
ranges, 2334 today — capped at 4. A scan **spends the whole budget**: a full pool
still has a worst member worth replacing, and stopping at the fill level freezes
the ranking at the first sweep's result (ADR-0015 supersedes ADR-0009's early
stop).
At intensity 1 that is ~2300 probes an hour — the knob to lower first on a
metered uplink.

A **recruit waits for a patrol in progress** — both are bulk measurements of the
same links, and a speed probe saturates the link, so concurrent runs measure
each other.

Probing is not free, and the two probe kinds differ by ~1000x in bytes: a latency
probe moves ~1.1 KB, a speed probe reads for a three-second window and moves
hundreds of KB. Both pools are maintained anyway — a cold pool cannot be switched
to on the first request that needs it — so speed probes are ~350 MB/day against
~10 MB/day for latency at default cadences. Accepted deliberately; see ADR-0012.

Candidate addresses survive a restart: the top of both pools is written to
`cf-candidates.csv` every hour and on shutdown, and a restart measures those
instead of exploring the published ranges. Without a snapshot the engine resolves
`cf.bootstrap_host` (default: the upstream hostname) and uses what DNS says.
**Scores in the file are never trusted** — every candidate is measured again,
because a score describes a moment. This replaces `cf-nodes.csv`, which nothing
reads any more.

The engine lives in `tools/relay/cft/`, a **separate module** mounted here for
development and referenced by a `replace` in `go.mod`. It is not committed to
this repository: CI skips the relay build while it is absent, and the Docker job
is disabled. Publishing it is what unblocks both — see TODO.md.

**No `http.Client.Timeout`.** It caps the *whole* request including body read,
so any download longer than the timeout is cut mid-stream (e.g. a 755MB ISO at
1MB/s dies at ~30s). Streaming proxies must bound only the setup phases:
`TLSHandshakeTimeout` + `ResponseHeaderTimeout` (30s) on the proxy/disguise
transports, `DialTimeout` (5s) inside the edge engine. Once response headers
arrive, transfer runs untimed.

### Speedtest

`GET /speedtest/{mb}?passthrough=n` — when `passthrough > 0`, proxies to upstream
with `passthrough - 1`; when 0 or absent, serves random bytes locally.

### Key files

- `tools/relay/server/server.go` — Server, routers, `httputil.ReverseProxy`
  rewrites, `X-MDB-Relay-Host` handling (`RelayHostHeader`), manifest watcher.
  Internal endpoints: `/.mirror-relay/cert-pack`, `/.mirror-relay/mirrors`,
  `/.mirror-relay/pool` (all JWT-guarded).
- `tools/relay/server/edge.go` — edge-selection wiring: engine construction,
  `cf.*` → `engine.Config` mapping, `/.mirror-relay/pool` snapshot, shutdown.
- `tools/relay/server/certmagic.go` — `CertMagicStorage()` is a `sync.OnceValue`:
  file storage in ACME mode, the shared `certclient.CertClient` in cert-pack
  mode. ACME issuance is skipped entirely in cert-pack mode.
- `packCache` (same file) — certmagic only reads Storage for names it manages or
  when on-demand TLS is on, so a consumer's pack would never be looked at and
  every handshake answered `no certificate available`. The pack is loaded into
  the cache as **unmanaged** certificates at startup and re-loaded every 30
  minutes, dropping hashes that left the pack so a renewal replaces its
  predecessor. See [ADR 0008](docs/adr/0008-cert-pack-consumer-caches-the-pack.md).
- `tools/relay/server/disguise.go` — SSH banner exchange
  (`ClientBannerExchange` is exported for `forward`).
- `tools/relay/server/speedtest.go` — `/speedtest/{mb}` handler with passthrough.
- `tools/relay/certclient/` — read-only `certmagic.Storage` that snapshots the
  parent's cert-pack hourly. Mutations return `ErrReadOnly`.
- `tools/relay/certsrv/` — streams the cert-pack tar; skips lock files.
- `tools/relay/guard/` — HS256 JWT (exp/nbf/iat enforced, alg pinned).
- `tools/relay/config/` — viper + `RELAY_CONFIG_DIR` / `RELAY_BASE_DOMAINS` env.
- `tools/relay/forward/forward.go` — `relay forward` L4 forwarder.
- `tools/relay/install/install.go` — `relay install` systemd setup.
- `tools/relay/uninstall.sh` — clean removal (handles old + new installs).

### Config / secrets

`RELAY_CONFIG_DIR` (default `/etc/mirror-relay`) is the single root for
`config.yaml`, `secret.key` and `certmagic/` — no CWD fallback, so the env var
can never split config from keys. `secret.key` is `0640`, the dir `0750`.
systemd unit pins `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` and enables the
usual hardening (`ProtectHome`, `ProtectKernelTunables`, `RestrictNamespaces`,
`MemoryDenyWriteExecute`, …).

### Tests

`go test -race ./...` in `tools/relay` — covers proxy rewrites (raw path,
hop-by-hop/`Connection` tokens, `X-MDB-Relay-Host` passthrough, upgrade,
base path), host matching, `keepHTTP`, cert-name→token mapping, cert-pack
tar streaming/untar, guard accept/reject matrix, config precedence, and the
`forward` half-close regression. CI runs vet + test before building.

### Build

```bash
cd tools/relay && go build -o relay
```
