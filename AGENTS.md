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
without redirect), optional `relayDirectives` (response-side `X-MDB-Relay`
keywords; never part of the distributed manifest), `fetch(request, context)`,
optional `status()`. `MountContext.relativePath` excludes the mount name and
query, adds no leading slash, and preserves pathname case, escaping and empty
segments. The router computes it once; path mounts use the existing pathname
split's remaining segments, while a subdomain mount receives its full path minus
the URL's structural leading slash. Mirrors consume the original request plus
this context instead of stripping their own prefix again.

- [server/index.ts](server/index.ts) — entry. Parses subdomain from host header.
  Subdomain mirror wins; on bare `@` splits pathname, looks up first segment in
  `mirrorsByPath`, then the mount switches: `/speedtest/`, `/relaytest/`,
  `/keys/`, `/api/`, `/scripts/`, `/conf/`. `serveMirror` passes the original
  request and resolved mount context to the mirror; every response then goes
  through the shared relay-control middleware
  ([server/relay-control.ts](server/relay-control.ts)). Inbound requests are
  normalised first: a request without a valid `X-MDB-Relay-Host` loses any
  `X-MDB-Relay` request header, so a direct client cannot inject response
  control. Everything else is a static asset or a **real 404** — the SPA
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
  - `GET|HEAD /api/mirrors` — JSON manifest of all mirrors plus ordered relay
    policies. Mirrors include `keepHTTP` and opaque `policy_tags`; each policy
    carries `match` unchanged and `rules` with `traffic_policy` (`latency` or
    `speed`), `passthrough`, the evaluation control `exit`, and `immutable` — an
    RE2-compatible pattern over the mount-relative path declaring those bytes
    immutable; it may carry a named group `sha256` or `sha1` that captures the
    content digest. Matching rules merge field by field in order (later wins,
    omitted inherits). `immutable` records a pre-request content-addressing fact,
    never response freshness, and does not change transport. Both methods emit the
    same deterministic ETag and honour `If-None-Match`. See
    [ADR 0009](docs/adr/0009-policy-driven-relay-transport.md) for distribution
    and [ADR 0012](docs/adr/0012-policy-field-merge-and-passthrough.md) for the
    merge semantics, plus
    [ADR 0015](docs/adr/0015-immutable-policy-rule.md) for `immutable`.
  - `GET /api/status/:name` — per-mirror status snapshot.
- [server/conf/apt/](server/conf/apt/) — deb822 source presets at
  `/conf/apt/<name>.sources`. Each preset returns `Stanza[]`; explicit metadata,
  not directory-name inference, handles multi-root, flat and architecture-mapped
  repositories. Query aliases override suites/components/architectures;
  `protocol` / `proto=http|https` selects every URI's scheme (default HTTPS).
  Versioned Debian presets include a fixed security stanza whose suite is not
  changed by a main-source `suites` override. Debian and Ubuntu use their installed
  archive keyrings; other presets inline the same armored block as
  `/keys/apt/<name>/in-sources`. All URIs use `relayInfo().origin`.
  `/conf/apt/` lists registered paths as JSON, or a linked `<pre>` list for browser
  user agents. See [ADR 0010](docs/adr/0010-explicit-apt-source-presets.md).
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
- [server/relaytest.ts](server/relaytest.ts) — `/relaytest/<case>` on the bare
  domain: a deterministic upstream for exercising the relay cache from outside.
  Eight fixed cases (`fresh`, `revalidate`, `ttl`, `vary`, `encoding`, `range`,
  `no-store`, `digest`) answer a fixed status, header set and body; `GET` and
  `HEAD` share status and headers, `HEAD` has no body and never evaluates Range.
  Not a mirror: absent from the manifest, never cached by the Worker, and the
  relay bypasses `/relaytest` itself (it neither reads nor writes cache there).
  Production mirrors carry no test configuration; tests point a temporary mirror
  at a case URL.

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

### Relay directive — [server/relay-control.ts](server/relay-control.ts)

`X-MDB-Relay` is the response-side control header, Cache-Control-shaped:
comma-separated `keyword` or `keyword=value`. The Worker parses/merges it, and
the relay consumes it (see the Relay server section below).
Keywords: `no-store`, `no-vary`, `prefetch`, `ttl=<seconds>`, `immutable`,
`sha256=<64 hex>`, `sha1=<40 hex>`; unknown keywords are ignored.
[server/relay-path.ts](server/relay-path.ts) owns runtime `PathFact` digest
validation, `200`/`206`/`304` gating and declaration merging. The OCI proxy,
verified APT `by-hash` branch and stable `debian-snapshot` branch construct those
facts while retaining their own path grammars; policy patterns stay independent.

- **Response-declared immutability.** `immutable` states that this response's
  bytes cannot change; `sha256`/`sha1` name its body digest. A digest never
  implies `immutable`, is only an Adopt expectation (never a cache key), and is
  accepted only at its exact hex length; two valid values for one algorithm
  cancel it. See [ADR 0017](docs/adr/0017-response-declared-immutability.md).

- **Injection is per mirror.** `serveMirror` merges `mirror.relayDirectives`
  into whatever the upstream already sent; an existing directive always wins and
  is never overridden. Merging is keyword-deduplicated and deterministic.
- **First batch:** the eleven `createAptMirror` mounts (`debian`,
  `debian-ports`, `debian-security`, `ubuntu`, `ubuntu-ports`, `mise`,
  `syncthing-pkgs`, `hashicorp`, `opentofu`, `github-cli`,
  `gitlab-runner-deb`) add `no-vary`. That states an upstream fact (a path names
  its bytes regardless of request headers), not a cache policy; other mirrors
  keep ordinary `Vary` handling. The APT proxy also adds `no-store` to mutable
  `dists/` indexes (excluding `by-hash/`), so independently cached Release and
  Packages generations cannot cause a hash/size mismatch.
- **Content-addressed paths declare themselves.** Only a request path can
  establish immutability; a response header (`ETag`,
  `Docker-Content-Digest`, `Content-Digest`, `Cache-Control`) never can. Three
  mirrors act on that fact, and only on a `200`, `206` or `304`:
  - the OCI proxy adds `immutable` to a `blobs/`/`manifests/` path that carries
    a digest, plus `sha256=`/`sha1=` when the path names that algorithm at its
    full (64/40 hex) length; another algorithm declares `immutable` alone, and a
    tag manifest declares nothing even when the response carries a digest;
  - the APT proxy adds the same pair, but only for a `by-hash/SHA256|SHA1/<hex>`
    path whose hash the resolved index or snapshot generation verified
    (`MD5Sum`/`SHA512` declare `immutable` alone). Mutable `dists/` indexes and
    `pool/` payloads never qualify, and an unjudged pass-through (no index yet)
    declares nothing. By-hash upstream requests send
    `Accept-Encoding: identity` for `GET` and `HEAD`, so the relay's active cache
    can admit them; a Range request keeps bypassing the Worker's read-through
    cache;
  - `debian-snapshot` adds `immutable` to a pinned
    `archive/<known>/<timestamp ≤ now>/…` object. A future timestamp (newest,
    still moving), `mr/` and a `404` declare nothing, and no digest is derived
    from a redirect.

  The directive is added to the delivered response, never to the Worker's own
  cached copy, and is merged through `mergeRelayDirectives`, so an existing
  `no-store`/`prefetch`/`ttl` is kept.
- **`prefetch` is opt-in per response.** The five snapshot APT mounts add it to
  `Release`/`InRelease` only (status `200` or `304`), and PyPI adds it to the
  `/pypi/simple/` root index only (status `200` or `304`). A `Packages` index,
  a package body and a per-package PyPI page never carry it. PyPI sends a
  conditional request (either validator) straight upstream instead of answering
  it from its own read-through cache, so a relay rebuilding a lease after a
  restart gets a real `304`; an unconditional request stays on the Worker cache.
  The relay learns eligibility only by serving the response; nothing about it is
  distributed.
- **Inbound copies are meaningless.** `normalizeRelayRequest` removes an
  `X-MDB-Relay` request header unless a valid `X-MDB-Relay-Host` marks the
  request as relay-originated. Only request headers are touched.
- **Directives never enter the manifest.** `no-store`, `no-vary`, `prefetch`,
  `no-transform`, freshness, coding and digests are post-response facts; policies
  only carry pre-request fields (see
  [ADR 0012](docs/adr/0012-policy-field-merge-and-passthrough.md)). Prefetch is
  no exception: it declares eligibility from a response and creates local state,
  not a distributed rule ([ADR 0014](docs/adr/0014-demand-driven-prefetch-leases.md)).
  `sanitizeResponse` strips any third-party
  `x-mdb-relay`, so only the front-door middleware can add one.

### APT metadata snapshot — [server/mirrors/apt-mirror.ts](server/mirrors/apt-mirror.ts)

Five APT mounts point at upstreams with a real archive but no autoindex
(`syncthing-pkgs`, `hashicorp`, `mise`, `opentofu`, `github-cli`). They set
`snapshot: true` and must declare a non-empty `suites` list — there is no
listing to discover suites from, so construction fails without one. Every other
APT mount keeps the plain proxy path. Each mount constructs either `AptRepo`
(directory discovery, `metadata-only` coverage) or `AptSnapshotIndex` (snapshot
build, `tree` coverage) as its single `AptRepository`. `createAptProxy` calls
`facts()` once per file request and uses that generation for existence,
`by-hash` and `Cache-Control`. Without facts the request stays unjudged: an
ordinary non-`by-hash` file gets `max-age=0`, while a `by-hash` response keeps
its one-year immutable `Cache-Control` but makes no relay immutable or digest
declaration. `Mirror.status()` reads the same facts and returns their
precomputed health summary, or `initializing` when no generation has ever
published.

- **One builder, one contract.** [snapshot.ts](server/pkgs/apt/snapshot.ts)
  pins each suite's `InRelease` (else `Release`), registers every file the
  release names, then downloads and parses only `Packages`/`Sources`. It
  prefers `by-hash` over the mutable path and verifies every fetched index
  against the release's size and SHA-256/SHA-1. No package body is ever
  fetched. Its health summary is computed from the same pinned releases and
  retains no parsed release or checksum tree. The full generation is published
  by one reference swap; any failure, limit overflow, path escape or conflicting
  digest rejects it and the previous generation keeps serving.
- **Bound 8 / 64 / 50 000.** One `Packages`/`Sources` may expand to 8 MiB
  decompressed, one mirror generation may parse 64 MiB (counted once per
  identical content — the Release digest is the identity, so a file repeated
  under several suites costs one download and one parse) and retain 50 000
  de-duplicated files. Index groups run at most four at a time
  (`GROUP_CONCURRENCY`), because each buffers its compressed body while
  decompressing. Only gzip and plain indexes are supported; an xz/bz2/zstd-only
  repository has no schema-1 index.
- **Three directory modes, one generation.** HTML, `?format=json` and read-only
  WebDAV (`PROPFIND` Depth 0/1) all read the current snapshot through a
  `WebListSource`; file bodies still stream from upstream through `WebListFs`.
  An unknown directory is `404`/`502`, never an empty listing. The mount root
  without a trailing slash is a relative `308`.
- **Fixed endpoint.** `/<mount>/.mdb/apt-index.json` serves one `schema: 1`
  JSON document (mirror, generation/expiry, each suite's release identity, the
  release's metadata files and the de-duplicated package files with path, size,
  SHA-256 and SHA-1). Paths are mount-relative; there is no absolute upstream
  URL and no unrelated package field. Other paths under `.mdb/` are `404`.
  `GET`/`HEAD` both answer; a matching `If-None-Match` is `304`.
- **Stable content ETag.** The validator hashes the document minus
  `generated_at`/`expires_at`, so a rebuild of unchanged upstream metadata keeps
  the same ETag while the times still appear in the body.
- **Same generation for every consumer.** Proxy facts, synthetic listings, the
  fixed endpoint and status all come from the published snapshot; status never
  resolves another repository or re-parses a release. The generation's own expiry
  drives both the endpoint's `Cache-Control` and the relay's index refresh (see
  prefetch below).
- **Release siblings stay reachable.** An `InRelease` generation lists only the
  signed name it actually verified; `Release` and `Release.gpg` in the same
  known suite are still passed through, because apt chooses between them. An
  unknown path inside a known suite is `404`; a path the generation does not
  cover is otherwise unjudged, so a real package body is never denied for being
  absent from a metadata-only table.
- **Method, not Range.** A HEAD reaches upstream as a HEAD (a body read from the
  read-through cache is dropped before delivery). `Range` is forwarded **only**
  for a known `by-hash/` object: the request then bypasses `cachedfetch`'s
  read-through cache, upstream answers the real `206`, and nothing partial is
  stored under the full-URL key. APT `pool/` files still do not forward `Range`,
  and a mutable `dists/` index is never ranged either.

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

- **Range never hits the digest cache.** The stored copy is a complete `200` and
  cannot answer a range, so a ranged digest GET (`Range` on the inbound request)
  skips the lookup and reaches upstream for a real `206`. The write path is
  unchanged: only a complete `200` is stored, and a `206` never is. The Worker
  does not slice bodies itself.
- **Digest upstreams are fetched as `identity`.** One digest names one byte
  sequence, and the cache entry is shared by every client of it, so a digest
  request replaces the client's `Accept-Encoding` with `identity` (redirect hops
  included). Tag manifests keep the client's negotiation.

- **Key = digest, not path** (`digestCacheKey`): same layer across repos
  (`library/nginx` + `library/node`) shares one entry. Scoped per upstream host
  to contain cache-poisoning blast radius (a bad upstream can't poison another
  registry's digest space; client self-verifies sha256 anyway → DoS-only).
- Forced `Cache-Control: public, max-age=31536000, immutable` on **every**
  digest-addressed 200, delivered and stored. Delivery gets it even when the body
  is over `maxCacheBytes` (or has no `Content-Length`) and so never enters the
  Cache API — otherwise the relay would receive a large layer with no freshness
  and revalidate it on every pull. `sanitizeResponse` merges this value, so an
  orthogonal directive such as `no-transform` survives. A tag manifest and any
  non-200 keep their upstream headers.
- The stored copy also drops `vary` via `STRIP_STORED_HEADERS`: a digest-addressed
  blob must not vary, and the Cache API refuses to store a `Vary` response. Only
  the cached copy is affected; the client still receives upstream's `Vary`.
- **Relay declaration from the path.** A digest-addressed path adds
  `X-MDB-Relay: immutable` — plus `sha256=`/`sha1=` when the path names that
  algorithm at full length — to the delivered `200`/`206`/`304`, never to the
  Worker's stored copy. This is also the only delivery path that carries no
  cache: an object over `maxCacheBytes` and a cache hit both still declare. A tag
  manifest declares nothing even when the response carries a
  `Docker-Content-Digest` (see the relay-directive section).
- **Size guard** `maxCacheBytes` (default 512MB = CF Free/Pro per-object limit).
  Skip when `Content-Length` unknown or over limit (avoids mid-stream
  `cache.put` reject). Override on Business/Enterprise.
- Write runs via `ctx.waitUntil` → client streams immediately, cache fills in
  background.

### Header hygiene (anti-leak)

`STRIP_RESPONSE_HEADERS`: `www-authenticate` (no login prompt), `set-cookie` /
`set-cookie2` (digest-shared cache would replay one client's cookie to all),
`x-mdb-relay` (a third-party upstream must not inject a relay directive; the
front-door middleware adds the mirror's own after sanitising), `age`, hop-by-hop
(`connection`/`keep-alive`/`transfer-encoding`). Stripping happens **before**
cache write, so neither cache nor client sees them.

`vary` is **not** stripped. The relay needs it to build cache variants, and a
direct client must see the representation negotiation the upstream asked for.
Responses carrying `Vary` bypass the Worker read-through cache instead of being
stored without it (see below).

`STRIP_STORED_HEADERS` adds `vary` for the one place a response is stored
regardless: the OCI digest cache, where a digest-addressed blob must not vary. It
applies to the stored copy only, so the client still receives upstream's `Vary`.

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
  `mdbCache` (`MdbCacheManager`); `createCachedfetch(manager)` binds another
  manager through the public seam. Cache strategy by method: GET full cache (match
  → fetch → save); HEAD match with `ignoreMethod: true` (reuse GET cache), miss
  fetches directly without save; other methods bypass cache entirely. Every hit,
  miss, revalidation and bypass goes through the same `sanitizeResponse` delivery
  step, and every delivered HEAD has a null body with its metadata intact. Guards:
  only caches 2xx GET, skips Range requests (206 partial would poison full-URL key),
  size ≤ `MdbCacheSizeLimit`, no `content-range`, in-flight write dedup. Used by
  the **APT proxy** ([apt/proxy.ts](server/pkgs/apt/proxy.ts)), `AptRepo`'s
  Release fetch, **`upstreamProxy`**, and PyPI. The **OCI proxy** does *not* use
  it: it needs digest-keyed entries + immutable `Cache-Control` rewriting, so it
  calls `mdbfetch` + an injected `cache` directly.

  **What can be stored (learned the hard way):** the Cache API keys by URL
  only and **silently refuses to store a response carrying `Vary`** — verified
  in workerd, `cache.match` returns nothing. `save()` therefore **skips any
  response that carries `Vary`**: stripping it to force a store would make the
  next hit deliver a different header set than the miss. Those requests always
  reach the upstream, and variant handling belongs to the relay. For the rest it
  stores the sanitised header set (`STRIP_RESPONSE_HEADERS`), not whatever the
  upstream sent. It also materialises document bodies before `put` (≤8 MB, or
  any textual body): a caller that transforms the response consumes the original
  body, and putting the unread branch of that tee never completes. Binary
  bodies keep streaming. Both failure modes are silent — the cache simply stays
  empty — so `cache.workers.test.ts` covers them. The snapshot mirror applies the
  same `Vary` skip to its own store.

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
  (`STRIP_RESPONSE_HEADERS`: `www-authenticate`, `set-cookie`, `x-mdb-relay`,
  `age`, hop-by-hop; **not** `vary`) from an upstream response before serving to
  clients. `cacheControl` overrides only the cacheability/lifetime directives on
  a 200; `mergeCacheControl` keeps orthogonal ones such as `no-transform`, which
  the relay must still see. `cachedfetch` applies the base cleaning on delivery;
  mirror layers keep it only for a direct `mdbfetch`/`ezfetch` or a specific
  override such as APT/OCI cache control.
- Constants in [const.ts](server/pkgs/fetch/const.ts): `CFCacheLimit` (512 MB,
  CF Free/Pro per-object cap) = `MdbCacheSizeLimit`, `MdbMaxConcurrentRequests`,
  `MdbCacheName` (`"upstream"`), `MdbUserAgent`, `MdbRevalidateIntervalMs`
  (10 min).

## Layout

- `server/pkgs/oci/` — reusable OCI components (proxy, creds, www-authenticate,
  host factory). `server/pkgs/apt/` — APT repo index, proxy, status, metadata
  snapshot (`metadata.ts`, `snapshot.ts`, `stanza.ts`).
  `server/pkgs/fetch/` — shared upstream fetch + cache + sanitize helpers.
  `server/pkgs/web-list/` — upstream directory-listing proxy (see below).
- `server/mirrors/proxy/` — composable hook-based HTTP proxy builder (see below).
- `server/relay.ts` — relay host parsing; `server/relay-control.ts` —
  `X-MDB-Relay` parsing/merging and inbound request normalisation;
  `server/relaytest.ts` — `/relaytest/<case>` deterministic upstream for relay
  cache tests.
- `server/api/` — Hono API routes.
- `server/keys/` — PGP/SSH key proxy at `/keys/` (see above).
- `server/mirrors/<name>/` — per-mirror logic + `index.ts` exporting a `Mirror`
  (`{ name, host? | path?, fetch, status? }`). `server/mirrors/index.ts`
  aggregates them.
- `tools/relay/` — Go relay server (domestic proxy that talks to the Worker via
  `/@relay/`). See [Relay server](#relay-server) below. `tools/relay/activecache/`
  — in-process assembly segment for immutable objects (one `PassiveCache` per
  key); `tools/relay/diskspace/` — free-space watermark guard shared by every
  local cache write.
- Path alias: `@server/*` → `./server/*`.

## Upstream proxy builder — [server/mirrors/proxy/](server/mirrors/proxy/)

Composable hook chain for non-OCI mirrors that need mount-relative upstream URL
mapping + cache + sanitize. Used by `createPassthroughMirror` (web-mirrors) and
npm.

- **`upstreamProxy(upstream)`** — factory returning a `ProxyRequest` pre-wired
  with: `setBaseUrl` → `filterHeaders` (Range + extras) →
  `standardizeUserAgent` → `followRedirects`. `setBaseUrl` appends the resolved
  `MountContext.relativePath` verbatim to the upstream base and preserves the
  original query. Fetches via `cachedfetch`, which owns response sanitising, so
  the factory adds no duplicate post-hook.
- **`Upstream`** interface: `{ url, passthroughHeaders? }`.
- **`ProxyRequest`** class: `.pre(hook)` / `.post(hook)` /
  `.apply(req, context)`. Pre-hooks receive the outbound Request and mount
  context; post-hooks transform the Response. Post-hooks receive the **original**
  client request (pre-rewrite), useful for relay-aware URL rewriting in response
  bodies. Within this chain, only the URL adapter rewrites the upstream URL;
  header and redirect hooks may still construct a Request. Neither the mirror nor
  the front door creates an inner Request just to strip a mount.
- Pre-hooks: `setBaseUrl` (preserves query), `stripSearch`, `filterHeaders`,
  `standardizeUserAgent`, `followRedirects`.
- Post-hooks: `sanitize` (strips dangerous headers), `rewriteBody(fn)` (streaming
  text transform via `TransformStream`; skips non-text; deletes `content-length`).

Example — npm mirror in full:
```ts
const proxy = upstreamProxy({ url: UPSTREAM })
  .post(rewriteBody((body, req) => {
    const mirrorBase = `${relayInfo(req).origin}/${PATH}/`;
    return body.replaceAll(`${UPSTREAM}/`, mirrorBase);
  }));
export const npm: Mirror = {
  name: "npm",
  path: PATH,
  fetch: (request, context) => proxy.apply(request, context),
};
```

## Web listing proxy — [server/pkgs/web-list/](server/pkgs/web-list/)

Generic proxy for upstream sites that serve Apache/nginx autoindex HTML. Used by
the APT mirror but reusable for any directory-listing upstream.

- **`WebListFs`** — read-only FS abstraction over an upstream listing site. Uses
  `cachedfetch` by default. `readdir(rel)` streams HTML through `HTMLRewriter`,
  extracting `<tr>/<th>/<td>/<a>` into a 2D cell array, guesses column semantics
  (Name/Last modified/Size) via regex, and cleans values into typed
  `WebListEntry` objects.
- **`WebListProxy`** — unified request handler exposing three access modes.
  `fetch(request, rel)` receives the original request and an explicit,
  already-resolved relative path; it never derives that path by stripping a
  mount from `request.url` or rebuilds an inner Request:
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

## Pigsty APT mirror — [server/mirrors/web-mirrors.ts](server/mirrors/web-mirrors.ts)

`/pigsty/` is a `passthroughConfigs` entry for `https://repo.pigsty.io/apt/`.
The `.io` origin is on Cloudflare and answers quickly from global PoPs; `.cc` is
optimised for mainland clients but slow outside China, which does not help a
Worker fetching from a CF edge. Both serve the same bytes.

The upstream has two repository layouts below one tree:

- `infra/dists/generic` + `infra/pool/main` — suite `generic`;
- `pgsql/<codename>/dists/<codename>` + `pgsql/<codename>/pool/main` — each
  codename is its own repository root.

That second layout does not fit `AptRepo`'s single `base/dists/<suite>` model, so
this is an `upstreamProxy` passthrough rather than `createAptMirror`. It still
carries the `apt` policy tag and `keepHTTP`, and Range passes through. Upstream
has no directory listings or by-hash indexes and advertises `Cache-Control:
no-store` on every object, so the read-through cache deliberately stores nothing.

`/keys/apt/pigsty` serves `https://repo.pigsty.io/key` (fingerprint
`9592A7BC7A682E7333376E09E7935D8DB9BD8B20`), which signs both layouts.

```bash
deb [signed-by=/etc/apt/keyrings/pigsty.gpg] https://mirs.uk/pigsty/infra generic main
deb [signed-by=/etc/apt/keyrings/pigsty.gpg] https://mirs.uk/pigsty/pgsql/bookworm bookworm main
```

## Snapshot mirror — [server/mirrors/debian-snapshot/](server/mirrors/debian-snapshot/)

Read-through access to `snapshot.debian.org` at `/debian-snapshot/`. The upstream
is the Debian archive's time machine and is **PB-scale**, so this is a cache, not
a mirror: nothing is pre-fetched, and only what a client asks for is stored.

Upstream shape, and what the mirror does with it:

- `/archive/<archive>/<YYYYMMDDTHHMMSSZ>/…` is one pinned tree. Eight archives
  exist (`debian`, `debian-security`, `debian-ports`, `debian-backports`,
  `debian-debug`, `debian-security-debug`, `debian-volatile`, `debian-archive`).
- Every file answers `302 → /file/<sha1>/<name>`, which is immutable. Redirects
  are followed **inside** the Worker, so clients never see an upstream origin.
- A timestamp between two snapshots resolves to the nearest **earlier** one, and
  one in the **future** resolves to the newest snapshot. The first is stable
  forever; the second keeps moving, so a future timestamp is never stored.
- `/mr/…` is the upstream's JSON API. Served, never stored.

Policy:

| path | cached | advertised | relay |
|------|--------|------------|-------|
| `archive/<known>/<timestamp ≤ now>/…` (GET, no Range) | yes, keyed by the request URL | `immutable, max-age=31536000` | `immutable` |
| the same with a future timestamp | no | `max-age=600` | — |
| `mr/…` | no | `max-age=600` | — |
| `Range` requests | no (206 would poison the key) | upstream | `immutable` on a pinned path |

Every delivered `200`/`206`/`304` for a pinned object also carries
`X-MDB-Relay: immutable` (never the stored copy), so the relay may keep it
without a revalidation; the `206` leaves the active-cache decision to the relay.
The declaration is a path fact only: no digest is derived from the `302` or from
a response header, and the Worker's own cache is neither cleared nor bypassed.

Everything else — the calendar, `/package/`, `/binary/` — is **404**: those are
browsing UI for the upstream site, and proxying them would turn this into a
crawler of a donation-funded service. Object size is capped by `CFCacheLimit`
(512 MB); a larger `.deb` streams through uncached.

Client prerequisites, which the mirror cannot fix:

- snapshots carry a long-expired `Valid-Until`, so apt needs
  `check-valid-until=no`;
- old snapshots are signed by archive keys of their era — install
  `debian-archive-keyring` (our `/keys/debian/<version>` covers 8 and later).

```bash
deb [signed-by=… check-valid-until=no] \
  https://mirs.uk/debian-snapshot/archive/debian/20240101T000000Z/ bookworm main
```

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
- `res/<uuid>/*` responses are sanitised by `cachedfetch` (strip
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

  `/in-sources` is designed for the **inline** form — first line unindented, all
  others indented by one space, blank lines as `.`, exactly one trailing newline:

  ```bash
  install -m 0644 /dev/stdin /etc/apt/sources.list.d/<repo>.sources <<EOF
  Types: deb
  URIs: https://mirs.uk/<repo>/
  Suites: <suite>
  Components: <component>
  Signed-By: $(curl -fsSL https://mirs.uk/keys/apt/<repo>/in-sources)
  EOF
  ```

  A keyring with several certificates is emitted as **one** armored block
  (`armorBinary`, CRC-24 included) so the field stays a single BEGIN/END pair; a
  single armored source is passed through untouched, which keeps the bytes
  identical to the `curl … | sed` pipeline.
- **`SrvSSHKeyList(urlProvider)`** ([utils/ssh-key.ts](server/keys/utils/ssh-key.ts))
  — sub-app serving a user's SSH `authorized_keys` list. `GET /:keyname` returns
  raw text; `/:keyname/info` returns parsed keys with md5 (via `spark-md5`) +
  sha256 (`crypto.subtle`) fingerprints.
- Key providers under [pgp/](server/keys/pgp/): `debianKeyName2Url` maps a
  codename/version (`bookworm`, `13`, `-security`, `-release`) to a
  `ftp-master.debian.org` archive key; `repo2KeyUrl` is a static table keyed by
  **APT mount name** plus documented aliases. Multi-URL entries merge current
  signers: Debian Ports uses the published yearly keys (add the new year before
  the old one expires), OpenTofu needs both its provider and repository keys, and
  Microsoft needs its rolling and legacy certificates.
- Routes mounted: `/debian/:codename`, `/apt/:repo`, `/keybase/:user`,
  `/gh|github/:user`, `/gitlab/:user`.
- Upstream fetch goes through `ezfetch` (no read-through cache yet — these keys
  are highly cacheable, so `cachedfetch` is a candidate swap).
- Deps: `openpgp` (heaviest single dependency — drives bundle to ~755 KB;
  needed for `/info` + `POST` verify), `spark-md5` (SSH md5 fingerprints).

## Docs

- [CONTEXT.md](CONTEXT.md) — the project's glossary (mirror, mount, front door,
  listing site, res resource, relay, relay directive, prefetch lease, metadata
  index, absolute reference, cache key…). Keep it a glossary: no implementation
  detail.
- [docs/adr/](docs/adr/) — decisions, including rejected ones (0001 mirror
  kinds, 0005 registry token realm scope, 0006 external probes, 0013 one
  Worker-owned APT index, 0014 demand-driven prefetch, 0015 the `immutable`
  policy field, 0016 the immutable active cache). A rejected ADR keeps the
  reasoning so it is not re-proposed from scratch.
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
  The suite list is therefore explicit. `syncthing-pkgs` is a snapshot mount, so
  its directories are synthesised from metadata instead of upstream listings.
- The bare Kubernetes repository prefix returns `403`, but concrete
  `core:/stable:/v<minor>/deb/` trees are anonymous and work. Its signing key URL
  is versioned; the key table follows the latest stable minor.
- Syncthing signs `dists/syncthing` with **two** certificates: the current one
  (`FBA2E162…`) that `syncthing.net/release-key.gpg` ships, and the 2014 key
  (`37C84554…`) it no longer publishes. A client with only the published key
  gets `Missing key 37C84554…` from `sqv` and apt skips the repository, so
  `/keys/apt/syncthing` serves both (the second from keyserver.ubuntu.com, which
  keeps its self-signatures). Sequoi rejects that certificate as *bound* because
  its self-signature is SHA-1 — a warning, not an error: `sqv` exits 0.
- APT `pool/` files are proxied with `ezfetch` and **do not forward `Range`**:
  a resuming client gets `200` from byte 0 and apt restarts the download. The
  request method itself is preserved (a `HEAD` reaches upstream as `HEAD`). Not
  fixed yet; mutable `dists/` files do not forward `Range` either, only a known
  `by-hash/` object does.
- A schema-1 APT index only parses gzip and plain `Packages`/`Sources`. A
  repository that publishes only xz, bz2 or zstd has no synthetic listing and no
  hash prediction, by design (ADR 0013). The same generation is also bounded to
  8 MiB per index, 64 MiB parsed per generation and 50 000 retained files, so an
  upstream that exceeds those limits simply keeps the previous generation.
- The relay's APT index, prefetch leases and their scheduling state are **memory
  only**. A restart serves no prediction and refreshes nothing until real
  requests teach it again; two relays learn independently.
- Relay request-cache metadata is **local to one relay** (bbolt). A shared
  `S3_BUCKET` carries bodies and digest pointers only, so two relays never share
  variant metadata; each rebuilds it from its own upstream traffic.
- Concurrent cache misses on the ordinary variant path are deliberately not
  merged (no singleflight), so a cold popular mutable object is fetched once per
  concurrent request. An immutable `GET` does coalesce: concurrent readers of one
  digest or URL share one assembly in the active segment (ADR 0016).
- Response-declared immutability can only act after the headers arrive: a
  request that finds an assembly already running for the URL is delivered
  ordinarily rather than joined, a partial `206` seed caches only its range and
  publishes nothing, and the entry is URL-keyed so it never deduplicates the same
  bytes across two URLs (ADR 0017).
- The active segment is **per process and per relay**. Its key map and fragments
  are memory/`CACHE_DIR` state, so a restart deletes incomplete assemblies and two
  relays never share one. A published body's local bytes also stay reserved in the
  segment until its last reader releases, which is why
  `CACHE_ACTIVE_CAPACITY`/`CACHE_ACTIVE_MAX_ENTRIES` bound it separately from the
  store's LRU.
- Shared-storage unit tests use a fake object store. A manual one-shot Docker
  check against RustFS 1.0.6 passed Put, dual-digest Get, Range, metadata restart,
  cross-instance adoption, Delete and Prune on 2026-11-23. No deployment has been
  verified, and that manual check is not part of CI.

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
    Real `HTMLRewriter`, `caches`, etc. Used by `web-list` parsing tests, the
    APT metadata snapshot's compression path, and the APT proxy's HEAD method
    tests.
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
  # manifest_sync_interval: 10m  # conditional HEAD; positive, jittered
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
  # intensity: 0.13          # sweep patience as a fraction of the block list
  # interval: 10s            # steady state: one probe, then rest
  # patrol_latency_interval: 5m
  # patrol_speed_interval: 30m
  # init_timeout: 30s        # startup wait; exceeding it is not fatal
  # candidates_file: ""      # default: <config dir>/cf-candidates.csv
  # snapshot_interval: 1h    # also written on shutdown
  # bootstrap_host: ""       # default: hostname of upstream.url

cache:                       # content store + request cache; disabled while dir is empty
  # dir: /var/cache/mirror-relay  # CACHE_DIR; enables the store, holds .tmp + index
  # capacity: 128GiB       # CACHE_CAPACITY; space the store may use, required
  # max_size: 12GiB        # CACHE_MAX_SIZE; LRU budget + per-body cap,
  #                        # default 10% of capacity
  # disk_water_mark: 4GiB  # CACHE_DISK_WATER_MARK; min free local bytes,
  #                        # 0/absent disables the guard
  # active_capacity: 2GiB  # CACHE_ACTIVE_CAPACITY; in-process segment budget,
  #                        # default cache.max_size, never above it
  # active_max_entries: 64 # CACHE_ACTIVE_MAX_ENTRIES; live assemblies
  # active_idle_timeout: 10m  # CACHE_ACTIVE_IDLE_TIMEOUT; idle entry lifetime
  # s3:                    # optional shared RustFS bucket; bodies leave local disk
  #   bucket: mirror-cache       # S3_BUCKET
  #   endpoint: https://rustfs.internal:9000  # S3_ENDPOINT (required with bucket)
  #   prefix: relay              # S3_PREFIX
  #   region: us-east-1          # S3_REGION

prefetch:                    # background refresh; disabled while CACHE_DIR is empty
  # concurrency: 4         # PREFETCH_CONCURRENCY; parallel background refreshes
  # max_tasks: 4096        # PREFETCH_MAX_TASKS; in-memory leases, not evicted

acme:
  source: acme               # "acme" | "upstream" | URL origin
```

`acme.source`:
- `acme` — DNS-01 via Let's Encrypt (needs `cf_api_token` per domain).
- `upstream` — sync cert-pack from `upstream.url`.
- URL (e.g. `https://upper.example.com`) — sync cert-pack from that origin.

Cache keys have their own env names, unlike the dotted-key default:
`CACHE_DIR`, `CACHE_CAPACITY`, `CACHE_MAX_SIZE`, `CACHE_DISK_WATER_MARK`,
`CACHE_ACTIVE_CAPACITY`, `CACHE_ACTIVE_MAX_ENTRIES`,
`CACHE_ACTIVE_IDLE_TIMEOUT`, `S3_BUCKET`, `S3_ENDPOINT`, `S3_PREFIX`,
`S3_REGION`. `CACHE_LOG_IMMUTABLE_MISS` (`cache.log_immutable_miss`, default
`false`) is the opt-in diagnostic described under the relay request cache; it is
read at startup, so it needs a restart, and no path is scanned while it is off.
`CACHE_DIR` enables **both** the content store and request caching:
empty means no store and every request goes upstream; non-empty means an
already-resolved mirror `GET`/`HEAD` tries the cache by default (ADR 0011). There
is no separate cache switch and no manifest field for it. The active segment
shares that switch: its own settings are validated only when a manager is built,
so a bad value logs `active cache disabled` and leaves the ordinary chain
working. Its defaults are `active_capacity` = `cache.max_size` (a larger value is
rejected, because a body above the store's per-body cap could never be adopted),
`active_max_entries` = 64, `active_idle_timeout` = 10m, and
`disk_water_mark` = 0 (disabled). An explicit `0` for
`active_capacity` or `active_max_entries` disables the active segment while
leaving the passive store enabled; those zeros are only an explicit switch, not
a default. Prefetch is created only when the store
exists, and reads `prefetch.concurrency` / `prefetch.max_tasks`
(`PREFETCH_CONCURRENCY`, `PREFETCH_MAX_TASKS`); a non-positive value falls back
to the default 4 / 4096.

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

### Policy sync and matching

The initial manifest load is a GET. Later checks send HEAD with the saved strong
ETag every `relay.manifest_sync_interval` (default 10m, jittered); only a changed
or missing ETag causes another GET. Mirrors, compiled policies, routing maps and
ETag are one atomic snapshot. Network errors, invalid JSON, duplicate policy
names, invalid RE2 expressions, an unknown `traffic_policy`, a non-boolean
`passthrough` / `exit`, `null`, and an unknown key in `match` or `rules` all
preserve the last known good snapshot — validation is all-or-nothing.

`rules.immutable` is consumed: the relay compiles the merged pattern with RE2 and
runs it once against the mount-relative path, so an invalid expression rejects the
whole manifest at load time. The Worker may publish the field only after a relay
that accepts it is deployed — an older strict relay sees an unknown rule key and
rejects the whole manifest, losing every policy until it is upgraded — so the
deployment order is relay first, Worker second. ADR 0015 records the field and
that order; ADR 0016 records what the relay does with a match.

Evaluation is **field merge, not first-match** (ADR 0012 supersedes the
first-match rule of ADR 0009; distribution, matching, ETag, atomic replacement
and transport fallback stand). Every matching rule applies the fields it
declares to an accumulated decision: later matches override, omitted fields
inherit, an explicit `false` overrides, and `exit: true` stops evaluation after
its own merge (`exit` is never part of the result). Defaults for an unmatched
resolved request are `traffic_policy: latency`, `passthrough: false`. There is no
catch-all policy: nothing matches before mirror resolution, and a trailing
latency rule would wrongly override an earlier speed rule under merge semantics.
Control-plane endpoints and `/relaytest` are bypassed before matching. Paths
exclude the query and keep their case and escaping. For a path mount,
`/debian/pool/x.deb` is matched as `/pool/x.deb`; for a subdomain mount, the path
is unchanged. Generic artifact suffixes only apply after mirror resolution.

`rules.immutable` merges like every other rule field: a later rule can restate or
omit it, and omission inherits. Its value is an RE2-compatible pattern matched
against the mount-relative path. It may carry a named group `sha256` or `sha1`
(`(?<sha256>…)`); when present, the captured hex is the object's content digest,
and without one the match only declares that the path's bytes are immutable. A
match is a pre-request statement; it says nothing about response freshness and
never selects a transport pool.

The relay never reads a cache field from the manifest: whether to cache is
`CACHE_DIR` plus the response (ADR 0011). `passthrough` is the only
policy-level cache decision, and it is knowable before the request.

Initial rules place two `immutable` patterns **before** the transport rules: OCI
digest `blobs/` and `manifests/`, and APT `by-hash/` SHA256/SHA1 objects. Both
carry the matching named digest group. The specific immutable rule matches first
and the broader rules after it still merge, so a digest blob gains `speed`
through `oci-blobs` while a digest manifest inherits the default latency.
`oci-blobs`, `apt-packages` and the generic suffix rule are unchanged; there is
no explicit default rule because an unmatched request already falls back to
`latency`. Bare `.gz` is deliberately absent so `Packages.gz` stays on latency.
See [ADR 0012](docs/adr/0012-policy-field-merge-and-passthrough.md) and
[ADR 0015](docs/adr/0015-immutable-policy-rule.md).

### Edge selection — [tools/relay/server/edge.go](tools/relay/server/edge.go)

Ranks Cloudflare anycast IPs from this host's vantage point and serves through
the best ones. Two pools, because the winner differs by workload: **latency**
(time to first byte; decides a manifest fetch) and **speed** (bytes/sec; decides
a layer download). The distributed policy list chooses a pool per request; an
unavailable speed pool falls back to latency, while explicit proxy/disguise
configuration bypasses both.

Startup calls `engine.Initialize` under `cf.init_timeout`. **Failure is logged,
not fatal:** the engine keeps running, patrol and recruit fill the pools in the
background, and requests fall back to unoptimised routing until they do. A
measurement problem must not become an outage.

`GET /.mirror-relay/pool` (JWT-guarded, like `/cert-pack` and `/mirrors`) reports
both pools, the gate verdict and which pool is serving — the answer to "why is
traffic going to that IP".

One **sweep** at startup, then one probe per interval. A **patrol** re-measures
every member on its own timers and preempts both (reader/writer gate: probes
share, a patrol takes the whole thing).

Startup order: `Start` runs **one sweep immediately**, then drops into one probe
per interval. Initialization only seeds a handful of addresses, so waiting a
schedule would leave the relay on that seed pool — which is why the sweep is
first, not scheduled (ADR-0018). The sweep logs one line when it finishes:
`sweep: P probed, K admitted, stopped after N without improvement in Xs`.

Two startup paths must tolerate a transient: the cert-pack fetch retries three
times before giving up (a parent restarting at that exact second used to kill the
consumer), and until `/api/mirrors` has loaded once the watcher retries every
15 s instead of every 10 minutes (a relay with no manifest answers 502 to
everything, so one unlucky edge address at startup was ten minutes of downtime).

`cf.intensity` (default `0.13`) is the sweep's patience: it stops after that
fraction of the block list in consecutive probes without an improvement — ~303
against 2334 blocks. A fraction rather than a count because the component is
reusable: the same value means "a large slice of the space" for Cloudflare and "a
few addresses" for a three-subnet list. ADR-0018 supersedes the budget model of
ADR-0009/0015.

A **patrol blocks every probe, including the steady state's** — it takes the
whole gate. A speed probe saturates the link, so a patrol and a probe running
together would measure each other.

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

The engine is `github.com/geektr-cloud/go-cloudflare-fastest-transport` (**v0.3.0**),
a separate published module. It used to be mounted at `tools/relay/cft/` and
referenced by a `replace` while it was validated end to end; the mount is gone,
CI builds the relay normally, and the image no longer copies a local module.

**No `http.Client.Timeout`.** It caps the *whole* request including body read,
so any download longer than the timeout is cut mid-stream (e.g. a 755MB ISO at
1MB/s dies at ~30s). Streaming proxies must bound only the setup phases:
`TLSHandshakeTimeout` + `ResponseHeaderTimeout` (30s) on the proxy/disguise
transports, `DialTimeout` (5s) inside the edge engine. Once response headers
arrive, transfer runs untimed.

### Speedtest

`GET /speedtest/{mb}?passthrough=n` — when `passthrough > 0`, proxies to upstream
with `passthrough - 1`; when 0 or absent, serves random bytes locally.

### Hash content store — [tools/relay/hashstore/](tools/relay/hashstore/)

`CACHE_DIR` is both the on/off switch and the local working directory: `.tmp/`
for bodies still being written, `.index.db` (bbolt) for sizes, SHA-1 aliases and
LRU order, and — without S3 — `data/<ab>/<cd>/<sha256>` for the bodies
themselves. There is no separate TMP_DIR.

- One body is stored once under its real SHA-256 and read through either digest;
  SHA-1 is an unweighted alias in the same index record.
- Two ways in: `Put` copies a reader into `.tmp` and hashes while it copies;
  `Adopt` publishes a body another component already wrote in full into `.tmp`.
  `Adopt(path, size, algo, digest)` accepts only a completed regular file at the
  declared size inside that directory, reads it once to recompute SHA-1 and
  SHA-256, and treats `algo`/`digest` as an expectation — a mismatch is
  `ErrDigestMismatch` and publishes nothing. A local backend renames the file into
  the data tree (the source path disappears); a shared backend uploads it and
  leaves the removal to the caller. Both paths share the publish window that keeps
  a concurrent reconcile or LRU eviction from deleting bytes whose index record is
  not committed yet, and a failed commit on a shared backend rolls the upload back
  only if no peer publisher or index record owns it.
- `CACHE_MAX_SIZE` is the LRU budget and the largest single body: a strict byte
  LRU over logical body bytes (`google/go-lru`, runtime-pressure eviction
  disabled by a constant-zero pressure func). `CACHE_CAPACITY` is the space the
  store may use and is required when the store is enabled; the budget defaults to
  10% of it, and a body above the budget is skipped without evicting anything.
- `CACHE_DISK_WATER_MARK` is the minimum free space on the filesystem holding
  `CACHE_DIR`; `0` disables the guard. It gates `Put`, `Adopt` and active
  admission before a temp file exists, and a large `Put` re-probes mid-write
  (16 MiB or 1 s). Reads, deletes, reconcile and prune never consult it. It
  applies to local `.tmp` staging even when `S3_BUCKET` holds the bodies. A failed
  filesystem probe degrades to no watermark with one warning: explicit capacity
  limits still bound the store, so a measurement problem must not become an
  outage.
- `S3_BUCKET` moves bodies to RustFS (AWS SDK for Go v2, path-style, checksums
  only when required); the local disk then keeps only `.tmp` and `.index.db`.
  The bucket holds `blobs/sha256/<ab>/<cd>/<sha256>` plus two tiny pointer
  objects (`aliases/sha1/<ab>/<cd>/<sha1>` → SHA-256,
  `aliases/sha256/<ab>/<cd>/<sha256>` → SHA-1) so any relay can resolve either
  address. Every object is sharded by its first two and next two hex chars, the
  same as the local layout. Pointers never count toward capacity.
- Shared storage is opportunistic: each instance keeps its own LRU and may delete
  any object, so losing that race is a MISS (`ErrNotFound`). Only a definite
  absence maps to a MISS — S3 `NoSuchKey`/`NotFound` and local `ENOENT`; a
  permission, timeout or I/O error is preserved, keeps the index entry, and
  surfaces as 502. Objects another instance wrote are adopted into the local index
  after a successful read. The bucket is never LISTed; each index entry is verified
  on read (local: open, S3: the GET/HEAD itself).
- Local startup reconciles in the background: an index entry without a file is
  dropped, a file without an index entry is removed. A failed eviction delete is
  written to the index as pending and retried by the same pass. Startup also
  empties `.tmp` (`cleanTempDir`), so an interrupted active assembly is deleted
  rather than resumed — a fragment is not an upstream fact.
- Public read-only endpoints, registered only when the store is enabled and
  independent of the manifest snapshot: `GET|HEAD /hash-content/sha1/<40 hex>`
  and `GET|HEAD /hash-content/sha256/<64 hex>`. Bad address → 400, absent → 404,
  storage error → 502, other methods → 405. Bodies stream, so a body may be
  evicted mid-transfer.
- `Store.Range` opens a body at an offset and returns the total size, the
  building block for `Content-Range` and multipart ranges. An offset outside
  `[0, size]` is `ErrInvalidRange`; a hit promotes recency like `Get`.
- **Variant metadata lives in the same local index** (`variantsBucket`,
  `variantRefsBucket`, `variantMetaBucket`). It is local bbolt even with
  `S3_BUCKET`: the shared bucket carries bodies and digest pointers only.
  `variantRefsBucket` maps body SHA-256 → variant key so eviction, `Delete`, and
  `Prune` remove the metadata in the same transaction as the body. Hard bounds:
  16 variants per URL, 65536 rows globally, 32 KiB per encoded row; an
  over-limit write returns `ErrVariantLimit` and is ignored, never truncating.
- `relay prune` is the offline cleanup (see below), not part of serving.
  Request caching, prefetch and the immutable active segment all sit beside this
  store; telemetry is still a later milestone. `CACHE_DIR` must be writable by the
  service user; the unit's `ReadWritePaths` covers `/var/cache/mirror-relay`, so a
  custom directory needs a unit change.

### Relay request cache — [tools/relay/server/cache.go](tools/relay/server/cache.go)

A hand-written exchange, not `httputil.ReverseProxy`: a hit must serve without
touching upstream, and a miss must tee the body into the store while the client
receives the same bytes. Trigger: `CACHE_DIR` set, method `GET`/`HEAD`, request
resolves to a manifest mirror, and the merged policy did not say `passthrough`.
Internal endpoints never resolve to a mirror; `/relaytest` is also skipped
explicitly. `passthrough` takes the ordinary proxy path before the cache chain:
no lookup, no write, no *response* `X-MDB-Relay` parsing or removal, no
transcode. (Inbound request sanitising still happens first for every request.)

Key: `mirror://<mirror name><mount-relative escaped path>[?<raw query>]`. Scheme,
relay domain and method deliberately stay out, so a `GET` and a `HEAD` share one
entry.

Decision order (`serveCache`):

1. Client `Cache-Control: no-store` runs a **miss that never writes**.
   `no-cache`, `max-age=0`, or `Pragma: no-cache` without `Cache-Control` force
   revalidation of a fresh entry.
2. Zero matching variants → MISS, unless a fresh APT metadata index covers the
   path as an immutable package file: hash prediction then looks the declared
   SHA-256 (SHA-1 fallback) up in the store and, when the body and size match,
   delivers it locally (`GET`, `HEAD` and Range) without building a URL variant.
   A client that asked for revalidation is never answered this way. A single
   stored variant whose body contradicts the fresh index is dropped before
   matching. One remaining match → fresh local hit, or stale revalidation; more
   than one (records with different `Vary` sets all match) → ambiguous,
   re-fetch and afterwards delete the identities that no longer apply. On an
   immutable path the zero-variant branch first tries a digest hit in the store,
   then the active segment for a GET (below), before the ordinary MISS.
3. A fresh hit writes the stored status/headers/body locally, rebuilds
   `Content-Length` from the body size and `Age` from `StoredAt`; conditional
   requests are evaluated first (`If-None-Match` uses weak comparison, `If-Match`
   strong; date conditions need `Last-Modified`) → `304` / `412`, then Range.
4. A stale entry is revalidated with `If-None-Match` / `If-Modified-Since`
   only. `304` folds new freshness/headers into metadata (never `Vary`,
   `Content-Encoding` or `Content-Length`) and leaves the body; a `304` that
   newly declares `no-store`/`private`/`Vary: *` drops the entry. `200` replaces
   the variant, `404`/`410` delete it. A `200` that the client's own condition
   short-circuits to `304`/`412` is still observed once, but stores no new body;
   if it carried `X-MDB-Relay: prefetch`, that observation creates or renews the
   lease, so a later background refresh can populate the cache. A stale ranged
   request is forwarded
   instead of revalidated. Before that, an OCI digest address — or, while the
   active segment is enabled, any path matched by the merged immutable pattern —
   is served locally when the store still holds the recorded body at the recorded
   size and any captured digest names it (ADR 0016); a client-requested
   revalidation still reaches upstream.
5. MISS streams the upstream response into `<CACHE_DIR>/.tmp` via an
   `io.Pipe` while writing the client, computes SHA-1 and SHA-256, and commits
   metadata under a timeout independent of the client connection. Only a
   complete `200` GET whose body reached the client becomes an entry. A MISS
   whose response carries `X-MDB-Relay: immutable` may instead seed the active
   segment before the client is written (see the response-declared seed below);
   every refusal delivers that same response through this ordinary tee.

**Content-addressed digests ignore stale metadata.** Before rule 4, a request
whose path addresses an OCI object by digest (`…/blobs/sha256:<64 hex>` or
`…/manifests/sha256:<64 hex>`) is served locally when the path digest equals the
variant's recorded body SHA-256 (case-insensitive hex) and the store still has
that body at the recorded size. Freshness cannot change the bytes under such a
URL, so GET, HEAD, Range, conditionals and coding negotiation reuse the ordinary
hit path; the metadata and its expiry are left untouched. A tag, another
algorithm, a digest mismatch, a missing or resized body or a store error falls
back to the rules above. `no-cache` / `max-age=0` still forces revalidation, and
a client `no-store` still bypasses the cache entirely. The same rescue also
applies to any path the merged `immutable` pattern matched, where a captured
digest additionally has to name the stored body.

**Immutable active path** ([cache_active.go](tools/relay/server/cache_active.go),
[ADR 0016](docs/adr/0016-tiered-active-cache-for-immutable-objects.md)). Only
this path diverges from the ordinary chain; a request that does not match
`immutable` keeps `streamAndStore` unchanged.

- **Eligibility.** Active manager present, merged policy matched `immutable`, no
  client-forced revalidation, client accepts `identity`, and the method is `GET`.
  A `HEAD` may reuse a digest-addressed body but never creates an entry. A
  request with its own `If-*` validators never seeds, because the upstream's
  answer, not a shared body, decides it.
- **Response-declared seed.** A response that carries `X-MDB-Relay: immutable`
  may seed the same path on its first MISS, before the client is written: the
  response disposition module ([disposition.go](tools/relay/server/disposition.go))
  calls `responseImmutableEligible` and then `seedResponseActive`. Admission
  requires `GET`, a `200`/`206` with a
  concrete size in `(0, CACHE_MAX_SIZE]`, an `identity` coding, no dangerous
  `Vary`, no `no-store`/`private`, a writable request that is neither
  client-forced-revalidated nor conditional, no multipart `Range`, no bypassed
  old variant, no ambiguity cleanup, client-accepted `identity`, a `206` whose
  `Content-Length` matches its `Content-Range` (the adapter closes the body when
  it rejects that mismatch, so the check must precede the handoff), and a merged
  policy that did **not** match `immutable`. The key is the ordinary URL cache
  key; a response digest is only an `Adopt` expectation via
  `activecache.Entry.SetExpectation`, never a key. A policy digest wins. The
  committed variant stores the persistent header set and `Persistent: true`
  while the client response keeps the upstream `Cache-Control`. Every
  refusal before the adapter takes the body delivers that same response through
  the ordinary chain (no second upstream request, and the fallback sits below the
  active decision, so it cannot recurse); a request that finds an entry
  already assembling the URL delivers its own response ordinarily instead of
  joining it, so a partial `206` seed publishes only when its range covers the
  whole object. A response whose body reached an adapter that could not start is
  never replayed: a policy seed starts a fresh exchange, a response seed answers
  `502`.
- **Key and coalescing.** `activecache.Manager` maps `sha256:<hex>` /
  `sha1:<hex>` (captured digest) or the URL key to one entry. OCI manifests are
  always URL-keyed: their media type and headers belong to the repository, so
  they never take a cross-URL synthetic digest hit and a joiner on another URL
  never commits the creator's headers as its own variant. Blobs and APT by-hash
  paths stay digest-keyed. The first `Acquire` becomes the creator and is the
  only one that contacts upstream; later acquirers wait for `Init` and fall back
  to the ordinary chain if it fails. This is the singleflight for immutable
  objects; the ordinary variant path still has none.
- **Seed.** The seed request sends `Accept-Encoding: identity` and, for a
  multipart `Range`, only its first interval. A `206` with a concrete total size
  may start an assembly; a `200` answering a ranged seed, a `206` without a total
  size, an unknown or zero length, a body above `CACHE_MAX_SIZE`, or any write
  prohibition (the ordinary rules plus `Content-Range`/`Vary`/non-identity
  coding) delivers the already-fetched response through the ordinary path. The
  seed detaches the client's cancellation: the body belongs to the shared entry.
- **Cold Range.** Missing intervals are fetched on demand by the tiered cache, so
  a cold `Range` read does not download the whole object first. Delivery covers a
  complete body, single and multipart ranges (the 32-range cap), `416`,
  `If-Range` and conditional evaluation from the shared fragment; a partial
  entry is never presented as complete.
- **Hole fill without byte ranges.** When a missing interval cannot be fetched by
  a range request — the origin ignores `Range` and answers `200`, or the range
  request fails before it yields a reader (for example `500`) — the active source
  repeats the object as one complete `GET`, streams past the prefix and hands the
  tiered cache exactly the requested section. The prefix is discarded, never
  buffered and never stored; a body that ends before the section is complete is
  `io.ErrUnexpectedEOF`. Only a failed complete `GET` fails the entry, so a
  range-refusing origin still completes a partial assembly. A failure after bytes
  were already delivered to the section (including a continuation fragment
  answered `200`) keeps the ordinary terminal semantics: it is not replayed from
  byte zero.
- **Publication.** A complete fragment is published through
  `hashstore.Adopt`, which validates that the source is a completed regular file
  inside `<CACHE_DIR>/.tmp` at the declared size and recomputes **both** digests;
  a captured digest is an expectation, and a mismatch publishes nothing and
  leaves an existing body untouched. `entryStore` implements both finalize paths:
  `FinalizeFromCache` adopts the scratch file in place (normal), `Finalize`
  adopts a copy (fallback when handoff is refused). After publication the entry
  leaves the active map immediately, the scratch name is reclaimed, and readers
  that already hold a local descriptor drain on it; the bytes stay accounted
  until the last reference retires. LRU, capacity and eviction remain the hash
  store's, unchanged: the active segment only limits how many assemblies run.
- **Variant.** After a delivery that completed the object the handler waits
  (bounded at 60 s) for `Adopt` and writes the ordinary URL variant, so a later
  request without a digest capture is still local. Nothing is written when
  publication failed or the size disagrees. The committed variant uses the
  stored header set, not the delivered one: a response that declared
  `immutable` persists the 365-day lifetime and is marked `Persistent`, while the
  first client already received the upstream `Cache-Control`.
- **Limits.** `CACHE_ACTIVE_MAX_ENTRIES`, `CACHE_ACTIVE_CAPACITY` (known sizes
  reserved at `Init`, one unknown-size entry admitted alone) and
  `CACHE_ACTIVE_IDLE_TIMEOUT` (reaper on `StalledTime`) refuse new entries; an
  admitted entry is never truncated. All three, plus the disk watermark, turn
  into the ordinary upstream path.
- **Close order.** `Server.Close` stops the active manager before the hash store:
  entries publish through `Adopt` and own descriptors the store manages, so the
  reverse order could adopt into a closed store.

Freshness (`freshnessExpiry`): `no-cache` stores but is immediately stale;
otherwise shared-cache preference `s-maxage` → `max-age` → `Expires` → the
`X-MDB-Relay: ttl` fallback (absent by default, `0` is not "forever"). `ttl`
never overrides upstream freshness. A response that declares
`X-MDB-Relay: immutable` is the exception: its **stored** header set is rewritten
to `max-age=31536000` (`responseVariantHeaders` →
`withPersistentLifetime`, orthogonal directives such as `no-transform` kept,
`Expires` dropped) and the variant carries `Persistent: true`, so it does not
come back stale on the next request even when the active segment was unavailable.
The delivered header set is untouched: the first client sees the upstream
`Cache-Control`, on the active and ordinary paths alike. Persistence is variant
metadata: a `304` keeps the 365-day lifetime even when it states `max-age=0` and
omits the directive, while a `304` that newly declares `no-store`/`private`
still invalidates the entry; non-persistent variants keep the old `304` rule.
`Persistent` is a spare bit in the existing variant flag byte, so pre-existing
bbolt records decode with it clear. Prefetch uses `prefetchExpiry`, the same
persistent lifetime, to schedule such a response; storage prohibitions are
unchanged. This is a response-declared fact, never derived from
`Cache-Control: immutable`.

Store rules (`storable` / `storableMetadata`): refused by `X-MDB-Relay:
no-store`, `Cache-Control: no-store` or `private`, non-`200`, `Vary: *` unless
`no-vary` was declared, `Content-Range`, an undecodable `Content-Encoding`, or no
body. HEAD never writes; a cold HEAD goes upstream and leaves no body-less entry.
A stale HEAD is revalidated like GET: `304` updates metadata, `200`/`404`/`410`
invalidates the variant, other statuses write nothing.

`X-MDB-Relay` keywords consumed: `no-store`, `no-vary`, `prefetch`,
`ttl=<seconds>`, `immutable`, `sha256=<64 hex>`, `sha1=<40 hex>`. `prefetch` only
feeds the lease table below; it never changes how the current response is stored,
transcoded or delivered. `immutable` plus its optional digest is the only
response-side way to enter the active segment (ADR 0017); the digest is an Adopt
expectation and never a cache key. The header is stripped before delivery on the
cache and normal proxy paths; a `passthrough` response is delivered with it
intact. `no-transform` is honoured from standard `Cache-Control`.

Encoding: stored codings are `identity`, `gzip`, `deflate`, `br`, `zstd`;
negotiation follows full `Accept-Encoding` qvalues/`*`/`identity` with
`stored → zstd → br → gzip → deflate` tie-break (explicit coding beats implicit
`identity`). Transcoding always decodes to `identity` then re-encodes; the
result is never stored and never creates a variant. A transcoded delivery drops
`Content-Length`, `Content-MD5`, `Digest`, `Docker-Content-Digest` and demotes a
strong `ETag` to weak. An OCI digest path or any entry carrying a digest header
is never transcoded; `no-transform`, no acceptable coding, or a range on a
to-be-transcoded hit falls back upstream. zstd decoding caps the frame window at
8 MiB (`encoding_stream.go`), so one request cannot allocate an arbitrary
decoder window.

Range: on the ordinary path, answered locally only for a fresh complete entry
whose negotiated coding equals the stored coding. Single and multipart ranges,
`416`, and `If-Range` (strong `ETag` or exact date; weak never matches) are built
from the local body via `Store.Range`. Multipart responses are capped at 32
ranges; a larger list is ignored in favour of a complete `200`, preventing
read/bandwidth amplification. A Range MISS is forwarded and its `206` never
becomes a stored variant; if upstream ignores Range and returns a complete `200`,
the entry may be stored and one warning line records the full-body transfer. The
active path is the exception: a cold ranged `GET` may seed an assembly, and a
range whose union covers the whole object lets the resulting complete body be
published.

Failure and concurrency: a pre-response cache failure logs and falls back to the
ordinary proxy; a transient body error does not delete metadata, a definite
absence drops it and becomes a MISS. Write failures only disable the tee — the
client still gets the complete response. Nothing retries after response headers
are sent. **No singleflight on the ordinary path**: concurrent misses on one
variant each go upstream and stream independently, while an immutable `GET`
coalesces in the active segment above; `Put` is idempotent by real hash and
metadata is committed by atomic replacement, so the final state is one body and
one self-consistent variant.

### Metadata prefetch — [tools/relay/server/prefetch.go](tools/relay/server/prefetch.go)

Demand-driven and relay-local (ADR 0014). A lease is created only by serving a
response that carried `X-MDB-Relay: prefetch`; nothing is persisted or
discovered by scanning, so a restarted relay starts empty and waits for real
traffic. `GET`/`HEAD` for the same resource renews the lease.

- **Three-day lease, no persisted job list.** `prefetchRetention` = 72 h;
  `dispatchDue` drops an expired lease (and any pending run) before dispatching.
  The scheduler owns the table behind one mutex and runs at most
  `prefetch.concurrency` refreshes (default 4) out of at most
  `prefetch.max_tasks` leases (default 4096). At the limit a new lease is
  refused with a warning; no active lease is evicted.
- **Freshness, not a clock.** A normal resource is refreshed with a conditional
  `GET` (`If-None-Match` / `If-Modified-Since`) at `freshnessExpiry`. `304`
  folds new freshness/headers into metadata, `200` replaces the variant
  atomically through the store, `404`/`410` drops it; other statuses only back
  off. A resource with no freshness is never queued automatically — a live
  request still revalidates synchronously. A failure backs off 1, 5, 15, then
  30 minutes (cap) and never deletes the last good data. Each expiry gets up to
  10% (max 30 s) deterministic jitter keyed by the task.
- **Variant identity is preserved.** A normal task stores the URL key plus the
  variant's `Vary`-matched request headers, replays them on the background
  request, and pins `Accept-Encoding: identity`. The task key is an in-memory
  codec over the same identity value the cache matches and files, so a header
  that was absent can never share a lease with one that was present and empty.
  A background run is always
  `GET`; `HEAD` may create or renew a lease but never becomes the refresh
  method. An ambiguous URL (several matching variants) is left to a real
  request.
- **`no-store` wins, once per state change.** A prefetch lease never makes a
  response storable. An ordinary `no-store` plus `prefetch` response schedules
  nothing and logs one warning per resource when the conflict state changes, not
  per request. The APT exception below schedules only the independent metadata
  index, never a client-cacheable Release/Packages variant.
- **APT is one task per mirror.** Every `Release`/`InRelease` on an apt-tagged
  mirror registers or renews a single task targeting
  `/<mount>/.mdb/apt-index.json` (`aptTaskKey`). A `304` extends the current
  generation's effective deadline when it states later freshness; a `200` is
  validated (`schema: 1`, mirror match, safe paths, package needs a digest) and
  published as a new generation atomically. A document with an `expires_at`
  already in the past retries after `defaultAPTIndexTTL` (1 h), not in a loop.
  A mirror whose mount moved is retargeted and fetched immediately.
- **Shutdown is bounded.** `Close` stops dispatch, cancels the scheduler
  context (so an in-flight refresh aborts), waits for running work, and is safe
  if `Start` never ran. The whole scheduler is absent when the store is
  disabled.

**Hash prediction** ([prefetch_apt.go](tools/relay/server/prefetch_apt.go)) uses
the same generation for delivery, not just refresh. `serveCache` asks
`freshAPTFile`: while the generation is fresh, a package path's declared
SHA-256 is tried first and SHA-1 second, with the size checked through
`Store.Stat`; `Store.Resolve` gives the canonical SHA-256 digest of the body.
The delivery
reuses the M2 hit path (`servePredicted` → `serveHit`) with synthetic headers
(content type from the extension, a digest-based `ETag`, `Accept-Ranges`,
`Digest`, and `Cache-Control` capped by `gen.until`), so `GET`, `HEAD` and Range
work without a URL variant. Metadata files are never delivered this way. A
stored variant whose body disagrees with the fresh index is dropped before it
can hit; a missing body, a size mismatch, a stale index or an unacceptable
`identity` coding falls back to the ordinary upstream path. Nothing here writes
a long-lived variant: the cache key is not populated by the predicted delivery.

### `relay prune` — [tools/relay/prune.go](tools/relay/prune.go)

Offline command: it loads only the `CACHE_*`/`S3_*` configuration, opens
`CACHE_DIR` with `hashstore.OpenOffline` (no startup reconcile, so the scan sees
the raw state) and runs `Store.Prune`. No server, edge engine, certificates or
manifest watcher is built. The bbolt lock refuses a second instance on the same
directory while the relay runs. A disabled cache (empty `CACHE_DIR`) is a clear
non-zero error.

`Store.Prune` completes queued (`pending`) deletes, then repairs the index for
**every** backend: each body record is checked with `backend.stat` (local `stat`,
S3 HEAD) and dropped — LRU, object record and its alias — only on a definite
absence (`stale`); a transient or permission error keeps the row and counts as
`failed` (fail-closed, never fail open). Dangling alias rows whose primary record
is missing are then removed in one bbolt transaction (also `stale`), and variant
metadata whose body record is gone is dropped from the local index (normal
eviction already removes it in the same transaction as the body; this repairs
older drift). Only after
that does it list the backend and delete every body and digest pointer the index
does not contain, so pointers of an already-vanished body go in the same pass.
Shared storage is paged with `ListObjectsV2` under the configured prefix — the
**only** listing in the store; normal reads and the startup reconcile still never
list. Unknown keys and other prefixes are counted as `skipped`/never touched. It
never adopts objects, recomputes digests or reads body contents. Failures are
counted, the pass keeps going, and the command exits non-zero with
`scanned/kept/deleted/pending/stale/skipped/failed` printed to stdout.

**Destructive by design on a shared bucket:** objects written by other relays,
including online ones, are deleted if this index lacks them. Only run it on a
bucket no peer depends on, or when dropping everyone's cache is intended. The
content is still fetchable upstream.

### Key files

- `tools/relay/server/server.go` — Server, routers, `httputil.ReverseProxy`
  rewrites, `X-MDB-Relay-Host` handling (`RelayHostHeader`), manifest watcher,
  and the `handleRequest` order: policy decision → inbound directive sanitising →
  cache chain (unless passthrough) → proxy. It also owns the cache component
  lifecycle: hash store and prefetch first, then the active manager, and `Close`
  stops the active manager before the store. Internal endpoints:
  `/.mirror-relay/cert-pack`, `/.mirror-relay/mirrors`, `/.mirror-relay/pool`
  (all JWT-guarded).
- `tools/relay/server/cache.go` — request-cache chain: lookup, hits,
  revalidation, MISS tee-and-store, metadata commit, ambiguity cleanup, index
  hash-prediction delivery, and stale digest hits. A hit only negotiates coding
  (with transcoding kept here) and hands a stored representation to
  `representation.go`.
  `disposition.go` — the response disposition module: one already-fetched
  response, one disposition (raw / stored / assembled), the pure decision table,
  exactly-once observation, body ownership and the pre-handoff fallback.
  `cache_headers.go` — stored-header selection, freshness, conditional
  evaluation, request cache directives. `representation.go` — the shared
  representation-delivery module: conditional `304`/`412`, `If-Range`, the Range
  grammar (`200`/`206` single/multipart/`416`, the 32-range cap) and the headers,
  framing and `Age` of every shape, over a `representationSource`. The hash store
  and an active entry differ only in that source; neither caller decides a
  status. `cache_active.go` — the immutable active path: eligibility, seeding
  (fetch/first range/metadata), the response-declared first-MISS seed
  (eligibility, URL-keyed `SetExpectation`, no-recursion fallback), the active
  byte source, reader-failure classification, the bounded wait for publication
  and the URL-variant commit, plus digest-hit and immutable stale-hit delivery.
  `immutable_miss.go` — the `CACHE_LOG_IMMUTABLE_MISS` diagnostic: the operator's
  unmodified expression, candidate extraction and the switch-first log gate.
  `variant_identity.go` — the relay's adapter over `internal/variant`: build,
  lookup and cleanup identities, with `Accept-Encoding` excluded from every
  relay-visible dimension but never from the stored row.
- `tools/relay/activecache/` — the in-process segment behind that path:
  `manager.go` (key map, acquire/init/fail, entry and byte accounting, idle
  reaper, close/retire), `entry.go` (one assembly, metadata, readers, publish
  settlement, `SetExpectation` for a URL-keyed entry with a response-declared
  digest), `store.go` (scratch file cache and the `tiered.Store` +
  `CacheHandoff` that publishes through `hashstore.Adopt`).
- `tools/relay/diskspace/` — `Checker` for one directory: watermark probe,
  throttled mid-write recheck, `ErrLowDiskSpace`; platform probe in
  `probe_linux.go` / `probe_other.go`, degrading to "no watermark" when the probe
  is unavailable.
- `tools/relay/server/encoding.go` — `Accept-Encoding` negotiation and coding
  parsing; `encoding_stream.go` — decode-to-identity/re-encode streams and the
  8 MiB zstd window cap.
- `tools/relay/server/prefetch.go` — in-memory lease scheduler: lazy
  registration, renewal, retention expiry, freshness-driven conditional refresh,
  backoff, jitter, concurrency/task bounds, shutdown; `prefetch_apt.go` — the
  schema-1 index parser, per-mirror generation table with a 304-extendable
  deadline, the APT index task, and hash-predicted delivery.
- `tools/relay/server/relay_directive.go` — `X-MDB-Relay` parsing and inbound
  direct-request sanitising; `policy.go` — manifest policy compilation,
  field-merge evaluation, passthrough decision and transport selection.
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
- `tools/relay/server/hashcontent.go` — `/hash-content/{sha1|sha256}/<hex>`
  read-only handler (400/404/405 matrix), registered before the proxy catch-all.
- `tools/relay/prune.go` — `relay prune` cobra command: offline cache cleanup,
  prints counts to stdout, non-zero on any failure.
- `tools/relay/hashstore/` — content store: `store.go` (Put/Adopt/Get/Range/Stat/
  Resolve/Delete, capacity + LRU + reconcile + publish windows + disk watermark),
  `index.go` + `index_variant.go` (bbolt:
  sizes, aliases, LRU order, pending deletes, variant rows and their reverse
  body references, the global variant counter), `variant.go` (the bbolt identity
  codec over the shared identity value, per-URL/global/row limits, and the
  `Persistent` flag bit that survives a 304), `backend.go` (local bodies
  and offsets), `s3.go` (RustFS bodies + digest pointers), `prune.go` (dangling
  variant repair).
- `tools/relay/internal/variant/` — the variant identity domain value: Vary
  name normalisation, one merged request value per varied header, matching and
  equality. The hash store's bbolt key and the prefetch scheduler's in-memory
  task key are separate codecs over it; `Accept-Encoding` exclusion is the
  relay's adapter policy, not part of the value.
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
tar streaming/untar, guard accept/reject matrix, config precedence, the
`forward` half-close regression, the hash store (dual digests, strict LRU,
oversized bodies, restart/alias recovery, local reconcile, shared S3 via a fake
object store, pending-delete retry, `/hash-content` 400/404/405 routing, sharded
bucket keys, offline prune (paged LIST, only-unindexed deletion, foreign key and
other-prefix safety, failure counting)), and M2: policy field merge / `exit` /
invalid-manifest rejection, variant identity and limits, local hits,
revalidation (`304` / `200` / `404` / `410`), HEAD sharing, conditional requests,
single/multipart ranges and `416`/`If-Range`, the four transcodings plus
`no-transform`/digest protection, `X-MDB-Relay` parsing and stripping, ambiguity
re-fetch, stale OCI digest hits (URL/body/size agreement, uppercase hex, tag /
sha512 / mismatch / missing-body / size / force-revalidate fallbacks), and cache
failure fallback; and M3: lease creation/renewal/expiry,
freshness-driven scheduling, backoff and task limit, concurrency bound, clean
shutdown, `no-store`/`prefetch` conflict, variant-header replay, HEAD renewing
but never refreshing, escaped-path replay, schema-1 index parsing (unknown
fields, bad digests, unsafe paths, conflicting entries), per-mirror aggregate
task and retargeting, 304 deadline extension, hash-predicted GET/HEAD/Range and
`If-Range`, SHA-1 fallback, mismatched-variant drop, metadata-kind refusal, and
skipping prediction for a client revalidation. The immutable round adds
`rules.immutable` compilation, merge and digest extraction (one named group only;
two captures or non-hex declare immutability without a digest), the Worker's two
published patterns, mount-relative matching, the active segment (coalescing
concurrent readers, single and multipart assembly, cold Range fill, late readers
sharing one upstream, publication and old-reader drain, both finalize paths,
digest mismatch publishes nothing, digest-keyed sharing across mounts, URL-key
entries, capacity/unknown-size/low-disk refusals, idle reaping, close while a body
is in flight), `Adopt` (both digests, expectation mismatch, invalid source,
LRU participation, restart survival, shared bucket, concurrent Put/Adopt and
no-delete-of-in-flight-publish), the disk watermark (refusal before temp creation
and mid-write, reads/deletes still working, disabled by default), and the HTTP
path (cold and cached immutable GET, digest and immutable stale hits, response
prohibitions, passthrough staying on the proxy, HEAD never creating an entry,
forced revalidation reaching upstream, unknown size / over-budget fallbacks, and
`Close` stopping the active manager before the store). The response-declared
round adds directive parsing (`immutable`, `sha256`/`sha1` exact-length, lowercase,
same-algorithm conflict cancels, digest does not imply immutable, sha256 wins),
`activecache.SetExpectation` (URL key kept, late/invalid/conflicting values
refused, only SHA-1/SHA-256 algorithms, digest-keyed entries never overridden),
and the first-MISS seed: a `200` seed and a whole-object `206` seed, a partial
`206` seed that publishes nothing, sha1/sha256 expectation match and mismatch
(body delivered, nothing published, no variant), policy digest precedence, and
the refusal matrix (`no-store`, `private`, gzip, dangerous `Vary`, unknown
length, a `Range` answered `200`, `HEAD`, conditional request, client
`no-store`/`no-cache`, a stale variant, ambiguity cleanup, a `206` whose
`Content-Length` disagrees with its `Content-Range` staying on the ordinary
path, capacity fallback delivering the same response from one upstream request,
and a digest without `immutable` staying ordinary). It also pins delivery versus
storage headers (the first response keeps the upstream `Cache-Control` on both
the active and the ordinary path), the persistent stored lifetime (a year,
orthogonal directives kept, `Expires` dropped), its survival across a forced
revalidation answered by a `304` with `max-age=0`, prefetch scheduling at that
lifetime (and `no-store` still disabling it), the two-concurrent-miss contract
(both fetch, neither joins, one variant), and the `CACHE_LOG_IMMUTABLE_MISS`
switch (default off, policy miss versus hit, passthrough, no query in the line,
unresolved mirror silent) plus the candidate scanner's documented boundaries
(path start/end, 41..63 and runs longer than 64, escaped delimiters, shared
delimiter, non-ASCII delimiter). Shared-storage unit cases
use a fake object store; the separate manual Docker RustFS 1.0.6 check is not part
of CI. CI runs vet + test before building.

Worker side (`pnpm test`, both vitest projects): `/relaytest` returns all eight
cases with matching HEAD status/headers, relay-control merges without overriding
and strips a direct request's `X-MDB-Relay`, mirror-directive injection adds
`no-vary` after sanitising (tested with `debian` as the representative APT
mount), and `cache.workers.test.ts` proves a `Vary` response bypasses the Worker
Cache API instead of being stored without it. M3 adds: metadata path/hash/limit
tests, snapshot build (index choice, by-hash verification, content dedupe,
atomic rejection, releases and transient-vs-absent), the five snapshot mounts
end to end (HTML/JSON/WebDAV, redirects, index ETag/304/502, `prefetch`
injection), HEAD-method forwarding through the real fetch helpers, PyPI
conditional passthrough, and HTML/JSON escaping of hostile listing names. The
path-declared directive round adds: OCI digest `200`/`206`/`304` carrying
`immutable` plus the path `sha256`/`sha1`, `immutable` alone for another
algorithm, nothing for a tag manifest even with a `Docker-Content-Digest`, and a
clean Worker cache copy; APT SHA256/SHA1 by-hash declarations (nothing for
`MD5Sum`/`SHA512` beyond `immutable`, nothing for `dists`/`pool`/an unverified
hash), `Accept-Encoding: identity` on by-hash `GET` and `HEAD`, and directive
merging that keeps an existing `no-store`; and `debian-snapshot` pinned
`200`/`206`/`304` declaring `immutable` with future timestamps, `mr/` and `404`
declaring nothing.

A one-off manual check on 2026-11-23 built snapshots for all five real upstreams
and confirmed the content ETag stays stable across rebuilds (`hashicorp`
de-duplicated to 15 parsed indexes, ~5.92 MiB). It is **not** part of CI, and no
deployment has been verified.

### Build

Go 1.26+ (`google/go-lru` requires it). The immutable active segment uses
`github.com/geektr-cloud/go-tiered-cache v0.0.1` — its core plus the `adapters/fs`
and `adapters/http` subpackages — as the only additional runtime module;
`go.uber.org/goleak` is test-only.

```bash
cd tools/relay && go build -o relay
```
