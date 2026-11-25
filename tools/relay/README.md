# Mirror Relay

Domestic reverse proxy for mirror-db. Discovers mirrors from upstream Worker,
replaces relay domain with upstream domain while preserving paths.
Uses Cloudflare fastest-node discovery to optimize edge routing.

## Build

Go 1.26 or newer (the content store uses `google/go-lru`, which requires it).

```bash
go build -o relay
```

## Config

Config file: `$RELAY_CONFIG_DIR/config.yaml` (default `/etc/mirror-relay`).
There is no CWD fallback: the config file, `secret.key` and the certmagic store
always resolve to the same directory.

```yaml
relay:
  # disguise_port: 344        # SSH-disguised TLS listener (bypasses HTTPS DPI)
  # secret_key: ""            # shared secret; auto-generated if omitted
  # manifest_sync_interval: 10m  # positive; conditional HEAD with jitter
  base_domains:
    - domain: relay.example.com
      cf_api_token: ""        # CF API token for DNS-01

upstream:
  url: "https://mirs.uk"     # upstream Worker or upper relay
  # disguise_port: 344        # connect to upstream via SSH-disguised TLS
  # proxy: ""                 # http(s)/socks5 proxy; bypasses edge selection

cf:                          # edge selection; every key is optional
  # top_n: 3                 # transports select randomly within Top-N
  # latency_capacity: 64
  # speed_capacity: 8
  # seed_count: 10           # latency results before Initialize returns
  # speed_seed_count: 3      # bootstrap IPs speed-probed during Initialize
  # latency_url: ""          # default: cloudflare.com/cdn-cgi/trace
  # speed_url: ""            # default: speed.cloudflare.com/__down
  # latency_min_score: 250   # reciprocal score; 250 ≈ "answers within 4s"
  # speed_min_score: 65536   # = below 64KiB/s is discarded
  # penalty: 0.5             # score multiplier for a failed request
  # intensity: 0.13          # sweep patience: 0.13 × block count probes
  #                          # without an improvement ends the opening sweep
  # interval: 10s            # steady state: one probe, then rest
  # patrol_latency_interval: 5m    # re-measure members (preempts everything)
  # patrol_speed_interval: 30m
  # init_timeout: 30s        # startup wait; exceeding it is not fatal
  # candidates_file: ""      # default: <config dir>/cf-candidates.csv
  # snapshot_interval: 1h    # also written on shutdown
  # bootstrap_host: ""       # default: hostname of upstream.url

cache:                       # content store + request cache; disabled while dir is empty
  # dir: /var/cache/mirror-relay  # CACHE_DIR; enables the store + local work dir
  # capacity: 128GiB       # CACHE_CAPACITY; space the store may use, required
  # max_size: 12GiB        # CACHE_MAX_SIZE; LRU budget + per-body cap,
  #                        # default 10% of capacity
  # s3:                    # optional shared RustFS bucket; bodies live there
  #   bucket: mirror-cache       # S3_BUCKET
  #   endpoint: https://rustfs.internal:9000  # S3_ENDPOINT (required with bucket)
  #   prefix: relay              # S3_PREFIX
  #   region: us-east-1          # S3_REGION

prefetch:                    # background metadata refresh; needs CACHE_DIR
  # concurrency: 4           # PREFETCH_CONCURRENCY; parallel refreshes
  # max_tasks: 4096          # PREFETCH_MAX_TASKS; in-memory leases

acme:
  source: acme                # "acme" = DNS-01; "upstream" = sync from upstream.url; or a URL origin
  email: acme@mirror-db.net
  ca: production
  agree: true
```

All keys can be overridden via env: `RELAY_POOL_SIZE=30`, `UPSTREAM_URL=...`,
`UPSTREAM_DISGUISE_PORT=344`, etc. (dots → underscores, uppercased).
Some have explicit names:

- `RELAY_CONFIG_DIR` — config/key/cert directory (see above).
- `RELAY_BASE_DOMAINS` — JSON array, replaces the yaml list entirely:
  `RELAY_BASE_DOMAINS='[{"domain":"r.example.com","cf_api_token":"tok"}]'`
- `CACHE_DIR`, `CACHE_CAPACITY`, `CACHE_MAX_SIZE`, `S3_BUCKET`, `S3_ENDPOINT`,
  `S3_PREFIX`, `S3_REGION` — content store (see [Hash content store](#hash-content-store)).
- `PREFETCH_CONCURRENCY`, `PREFETCH_MAX_TASKS` — background refresh bounds (see
  [Metadata prefetch](#metadata-prefetch)); defaults 4 / 4096.

### `acme.source`

| value | meaning |
|---|---|
| `acme` | issue locally via DNS-01, needs `cf_api_token` per domain |
| `upstream` | fetch the cert-pack from `upstream.url` |
| a URL | fetch the cert-pack from that origin |

In cert-pack mode the relay is a **pure consumer**: it never issues or renews,
and its storage rejects writes. Cert sync follows the same transport path as
proxying, so it works through `upstream.disguise_port` and `upstream.proxy`.
Peers must share `relay.secret_key`.

### Secret key

`relay.secret_key` (or `secret.key` in the config dir) signs the JWT that
guards `/.mirror-relay/cert-pack`. Every relay in a chain must share it. When
omitted, a random key is generated with `0640` permissions in the config dir —
set it explicitly for multi-tier deployments.

## Run

```bash
# Direct
./relay serve

# Install as systemd service (interactive)
sudo ./relay install
```

## How it works

1. On startup, ranks Cloudflare edge IPs from this host (see [Edge selection](#edge-selection)).
   Failing to do so is not fatal — the relay still serves, unoptimised.
2. Fetches mirror manifest and traffic policies from `GET <upstream>/api/mirrors`.
   A failure is not fatal: the relay serves `502` until the first fetch succeeds.
3. Rechecks every `relay.manifest_sync_interval` (default 10m, jittered) with a
   conditional HEAD. Only a changed or missing ETag causes a GET; errors and
   invalid updates keep the last good snapshot.
4. Listens on :443 (HTTPS) + :80 (HTTP redirect/keepHTTP)
5. Routes by Host header, replacing relay domain with upstream domain:
   - `<sub>.<relay-domain>` → proxies to `<sub>.<upstream-domain>` with same path
   - `<relay-domain>` → proxies to `<upstream-domain>` with same path
6. Evaluates the ordered policy list against the mirror-relative request path.
   Every matching rule merges its fields into the decision (later wins, omitted
   inherits, `exit` stops evaluation), selecting the latency or speed transport
   and whether the path is `passthrough`. Unmatched requests use latency and are
   not passthrough.
7. Sets `X-MDB-Relay-Host` **only when the client did not already send one**, so
   a multi-tier chain keeps the origin closest to the user.

### `X-MDB-Relay-Host`

The Worker reads this header (`server/relay.ts`) to build absolute URLs —
Helm chart tarballs, npm tarballs, install scripts — that point back through
the relay instead of straight at the Worker.

The relay treats it as **client-supplied and trusted**: anyone may proxy
(this is an open project) and the value only influences generated links, never
authorization. It is forwarded untouched; the relay fills in its own base
domain only when the header is absent. Relay-originated requests
(`/api/mirrors`, `/speedtest?passthrough`, cert-pack sync) do not set it.

### Edge selection

Two pools, because the fastest IP differs by workload: **latency** (time to
first byte — decides a manifest fetch) and **speed** (bytes/sec — decides a layer
download). Both are maintained, and the distributed policy list selects one per
request. OCI blobs, APT packages, and common archive/image/installer suffixes
prefer speed; metadata and unmatched requests prefer latency. If speed is not
available the request falls back to latency. Explicit `upstream.proxy` and SSH
disguise configuration bypass edge selection and therefore ignore this preference.

Each pool keeps the best N scored IPs, and a transport picks randomly among the
top few. A failed request multiplies that IP's score, so a bad edge drifts out of
selection on its own rather than being banned — and comes back when it measures
well again.

Measurements are gated: while this host cannot reach the canary
(`223.5.5.5:443`), nothing is probed and in-flight results are discarded. A local
outage must not look like "every Cloudflare IP got worse".

Three jobs share the work. A **patrol** re-measures every member, which is bounded
by the pools' capacity and therefore cheap — every five minutes. A **scan**
explores the address pools for addresses nobody holds yet; it is the expensive
one, on an hourly cadence. A **recruit** is a small scan on a short cadence, aimed
at replenishment.

The order at startup is deliberate: initialization only seeds a handful of
addresses, so **one full scan runs immediately** and the periodic jobs start once
it finishes. It logs what it did:

```
sweep: 412 probed, 64 admitted, stopped after 303 without improvement in 1m12s (latency=64 speed=8)
```

The sweep ends when the pool stops improving, not when it is full and not when a
budget runs out: `cf.intensity` (default `0.13`) is how many consecutive probes
without an improvement to tolerate, as a fraction of the block list — ~303
against 2334 blocks.

After that the steady state probes **one address per `cf.interval`** (default
10 s), which walks the whole address space in about 68 days. That is the long
tail, not the main path to a good pool: patrols keep the members honest.

A patrol takes the whole gate, so no probe — sweep or steady — runs during one.

### What probing costs

A latency probe moves about **1.1 KB** (request line, response headers, a small
body). A speed probe reads for `speed_probe_duration` (three seconds by default),
so it moves whatever the link delivers in that time — hundreds of KB, measured as
0-700 KB on a congested path. Three orders of magnitude more. Measured on a
nominally 0.5 MB/s Cloudflare path:

| job | cadence | probes/day | bytes/day |
|---|---|---|---|
| opening sweep | once | ~300-900 | ~1 MB |
| steady state | 1 probe / 10s | ~8,600 | ~9 MB |
| patrol latency | 5m × 64 members | ~18,400 | ~20 MB |

Both pools are maintained regardless of which one is selected, because routing is
per-request: a cold high-speed pool cannot be switched to on the first request
that needs it. That accepts a real cost — **speed probes add 300-500 MB a day**
against ~10 MB for latency probes at the same cadence — deliberately rather than
by oversight (ADR-0012 in the separate transport module).

Candidate addresses survive a restart: the top of both pools is written to
`cf-candidates.csv` every hour and on shutdown, and a restart measures those
instead of exploring the published ranges. With no snapshot, the engine resolves
`cf.bootstrap_host` (default: the upstream hostname) and uses what DNS says.
Scores in the file are for humans — every candidate is measured again, because a
score describes a moment and a restart is a different one. (This replaces the old
`cf-nodes.csv`, which nothing reads any more.)

`GET /.mirror-relay/pool` (JWT-guarded) reports both pools and the gate verdict.

Set `cf.latency_url` / `cf.speed_url` to your own endpoints when proxying a
specific zone: edge routing differs per zone, so ranking on `cloudflare.com`
while serving `mirs.uk` optimises the wrong path.

### Proxy semantics

Proxying is handled by `httputil.ReverseProxy`, which gives RFC 9110
hop-by-hop handling (including tokens named in `Connection`), `Upgrade`
passthrough for WebDAV/PROPFIND, and `X-Forwarded-For`/`-Host`. The request URI
is preserved byte-for-byte (OCI tags and digests containing `:` or `%2F` survive).

Plain HTTP redirects to HTTPS with **308** (method and body preserved).

---

## `relay forward` — L4 test aid

`forward` is not a request proxy: it forwards raw TCP so a local client can
reach an upstream over the SSH-disguised transport that only the relay normally
speaks.

```
local client ──plain TCP──▶ forward :8443 ──SSH banner + TLS──▶ upstream :344
```

```bash
relay forward --upstream ssh://relay.example.com:344 :8443
relay forward --upstream https://relay.example.com        # plain TCP, no disguise
```

There is **no authentication** — by design, matching the rest of the open
project. It tunnels to a fixed upstream address, so it is not an open proxy.

## SSH Disguise (anti-DPI)

Some ISPs throttle HTTPS traffic via deep packet inspection. The disguise
feature wraps TLS in a fake SSH banner exchange:

```
Server → Client: SSH-2.0-OpenSSH_9.6\r\n
Client → Server: SSH-2.0-OpenSSH_9.6\r\n
(TLS handshake follows on the same TCP connection)
```

The server sends first and the client reads first — both sides are strict about
the banner, so a mismatched peer fails the handshake rather than desyncing.

DPI sees SSH → no throttle. Enable with:

- **Server side**: `relay.disguise_port: 344` — listens on :344 alongside :443
- **Client side**: `upstream.disguise_port: 344` — connects to upstream relay on :344 with SSH banner

## Speedtest

`GET /speedtest/<mb>` returns `<mb>` megabytes of random data (max 1024 MB).
Use for bandwidth testing through the relay.

## Hash content store

An optional, content-addressed body store and the switch for relay request
caching. `CACHE_DIR` is the switch: empty (default) means no store and no
`/hash-content` route; set it and the relay starts storing bodies, publishing
their digests, and serving already-resolved mirror `GET`/`HEAD` requests from
cache (see [Request cache](#request-cache)).

Config (env names in parentheses):

| key | meaning |
|---|---|
| `cache.dir` (`CACHE_DIR`) | local working directory; enables the store |
| `cache.capacity` (`CACHE_CAPACITY`) | disk space the store may use, e.g. `128GiB`; required |
| `cache.max_size` (`CACHE_MAX_SIZE`) | LRU budget, and the largest body kept; default 10% of capacity |
| `cache.s3.bucket` (`S3_BUCKET`) | use a shared RustFS bucket instead of local bodies |
| `cache.s3.endpoint` (`S3_ENDPOINT`) | RustFS endpoint; required with a bucket |
| `cache.s3.prefix` (`S3_PREFIX`) | key prefix inside the bucket |
| `cache.s3.region` (`S3_REGION`) | default `us-east-1` |

Byte sizes accept binary units (`256MiB`, `128GiB`, plain bytes). Credentials
come from the standard AWS environment variables / SDK credential chain; nothing
else is configured here.

Layout:

- `<CACHE_DIR>/.tmp/` — bodies being written. Always local.
- `<CACHE_DIR>/.index.db` — bbolt index: sizes, SHA-1 aliases, LRU order, any
  delete still waiting to be retried, and the local variant metadata that files
  cached responses by URL and `Vary` identity. Always local.
- `<CACHE_DIR>/data/<ab>/<cd>/<sha256>` — bodies, when no bucket is configured.
- In the bucket: `blobs/sha256/<ab>/<cd>/<sha256>`, plus two tiny pointer objects
  `aliases/sha1/<ab>/<cd>/<sha1>` and `aliases/sha256/<ab>/<cd>/<sha256>` so a
  peer relay can resolve either digest. Every object is sharded by the first two
  hex chars and the next two, exactly like the local layout. Pointers are not
  counted against capacity.

Behaviour:

- A body is stored once, keyed by its real SHA-256, and is readable through
  either digest. SHA-1 is an alias, not a second copy.
- `cache.max_size` is both the total cache budget (strict byte LRU over logical
  body bytes) and the largest single body. It defaults to 10% of the available
  capacity; a larger body is skipped and never evicts anything.
- A shared bucket is opportunistic reuse only: every relay keeps its own LRU and
  may delete any object, so a body another relay already dropped is a cache MISS
  (fall back upstream), not an error. A transient or permission error (denied,
  timeout) is not a MISS and leaves the index entry alone. Objects written by
  another relay are adopted into this relay's index after a successful read.
- On startup a local store reconciles in the background: index entries whose
  file vanished are dropped, files without an index entry are removed. A delete
  that fails is queued in the index and retried. Variant metadata that names a
  gone body is removed in the same transaction, and `relay prune` repairs older
  drift.
- Variant metadata is capped: 16 variants per URL, 65536 rows in total, 32 KiB
  per encoded row. A write over a limit is skipped with a log line; nothing is
  truncated. `S3_BUCKET` does not move this metadata — only bodies and digest
  pointers leave the host.

### `/hash-content/<algo>/<digest>`

Every relay serves the cached bodies publicly, read-only:

```bash
curl -O https://relay.example.com/hash-content/sha256/<64 hex>
curl -I https://relay.example.com/hash-content/sha1/<40 hex>
```

`GET` and `HEAD` only. A malformed algorithm or hex length is `400`, an absent
body is `404`, a storage error that is not a definite absence is `502`, and any
other method is `405`. Responses stream, so a body may be evicted by another
relay while it is being transferred. The route is registered before the proxy
catch-all and does not depend on the upstream manifest.

### Write permissions

`relay install` creates `/var/cache/mirror-relay` and lists it in
`ReadWritePaths=`. Point `CACHE_DIR` at that path, or add your own directory to
the unit's `ReadWritePaths=` and give the service user ownership — otherwise
`ProtectSystem=strict` makes it read-only and the relay fails to start.

## Request cache

With `CACHE_DIR` set, a `GET` or `HEAD` that resolves to a manifest mirror is
served from the local store when possible. Only those two methods enter the
chain; everything else is proxied untouched. Internal endpoints never resolve to
a mirror, and `/relaytest` is skipped explicitly. A policy that resolves to
`passthrough: true` bypasses the chain before it starts: no lookup, no write, no
response `X-MDB-Relay` handling, no transcode. Inbound request sanitising still
runs first for every request.

- **Fresh hit** — no upstream request. Status, stored headers and the local body
  are delivered; `Content-Length` is rebuilt from the body size and `Age` from
  the store time. `If-None-Match` / `If-Modified-Since` / `If-Match` /
  `If-Unmodified-Since` are answered locally (`304` / `412`) before Range.
- **Stale entry** — revalidated with the stored `ETag` / `Last-Modified`. `304`
  updates freshness and headers only, `200` replaces the variant (and the body),
  `404` / `410` delete it. Without validators the relay fetches the full
  response. A stale ranged request is forwarded instead of revalidated.
- **Index prediction** — no matching variant is not always a MISS: a fresh APT
  metadata index can name the body of a package file by digest, and the relay
  then serves it from the store (see [Hash prediction](#hash-prediction-apt)).
  A client that asked to revalidate is always sent upstream.
- **MISS** — the upstream body is streamed to the client while a tee writes
  `<CACHE_DIR>/.tmp` and computes SHA-1 + SHA-256; metadata is committed after a
  complete `200` GET whose bytes reached the client. Only a complete `200` GET
  creates an entry, so a `HEAD` never leaves a body-less entry behind.
- **Client request cache control** — `Cache-Control: no-store` skips both lookup
  and write; `no-cache` and `max-age=0` force revalidation of a fresh entry
  (`Pragma: no-cache` only when no `Cache-Control` is present).
- **Freshness** — `no-cache` stores but is immediately stale; otherwise a shared
  cache prefers `s-maxage`, then `max-age`, then `Expires`. `X-MDB-Relay:
  ttl=<seconds>` is a fallback only when none of those is present and defaults to
  none; it never overrides upstream freshness.
- **Not stored** — `X-MDB-Relay: no-store`, `Cache-Control: no-store` or
  `private`, any non-`200`, a `Vary: *` unless `no-vary` was declared, a
  `206`/`Content-Range`, or a `Content-Encoding` the relay cannot decode.

### `X-MDB-Relay`

A response-control header the upstream sends to the relay, shaped like
`Cache-Control`: comma-separated `keyword` or `keyword=value`.

| keyword | effect |
|---|---|
| `no-store` | do not write this response to the relay cache |
| `no-vary` | the URL path alone identifies the resource; `Vary` does not split variants, `Vary: *` included |
| `prefetch` | the resource is eligible for background refresh while the relay keeps seeing demand for it; storage and delivery of this response are unchanged |
| `ttl=<seconds>` | fallback lifetime when the response states none; `0` means none |

The relay reads it and removes it before delivery, except on a `passthrough`
response. Unknown keywords are ignored; `no-transform` is honoured from the
standard `Cache-Control`. The Worker injects `no-vary` for the eleven
`createAptMirror` mounts; every other mirror keeps ordinary `Vary` handling.
It injects `prefetch` on `Release`/`InRelease` of the five snapshot APT mounts
and on the `/pypi/simple/` root index only (both on `200`/`304`). These
decisions never appear in the manifest policy.

### Encodings and ranges

Stored codings are `identity`, `gzip`, `deflate`, `br` and `zstd`. Delivery picks
the best acceptable coding from the request's full `Accept-Encoding` (qvalues,
`*`, `identity`) and, when it differs, decodes to `identity` and re-encodes on
the fly. A transcoded response is never stored and never becomes a variant; it
drops `Content-Length`, `Content-MD5`, `Digest` and `Docker-Content-Digest`, and
demotes a strong `ETag` to weak. OCI digest paths and entries carrying a digest
header are never transcoded. zstd decoding accepts a frame window of at most
8 MiB, so one request cannot allocate an arbitrary decoder buffer.

Range requests are answered locally only from a fresh complete entry whose
negotiated coding is the stored one: single and multipart `206`, `416`, and
`If-Range` (strong `ETag` or exact date) are supported. At most 32 ranges are
served; a larger list is ignored in favour of one complete `200`, preventing
multipart amplification. A Range MISS is proxied and `206` is never stored; if
upstream ignores Range and returns a complete `200`, the relay may store it and
logs one warning about the full-body transfer.

### Failure and concurrency

A cache read or metadata failure before the response starts logs and falls back
to the ordinary proxy; a definite body absence is a MISS, a transient error
leaves metadata alone. A write failure only disables the store tee — the client
still receives the complete upstream response. Nothing retries after response
headers are sent.

There is **no singleflight**: concurrent misses on the same response each go
upstream and stream independently. Storage is idempotent by real hash, and
variant metadata is replaced atomically, so the end state is one body and one
self-consistent record. Metadata is per-relay; a shared bucket does not share it.

### Hash prediction (APT)

When a request resolves to an apt-tagged mirror and the relay holds a fresh
schema-1 metadata index for it, a package file whose path the index declares can
be delivered from the content store without contacting upstream.
The relay tries the declared SHA-256 first and SHA-1 second, checks the size,
and serves the body through the same hit path as a cache hit (`GET`, `HEAD` and
Range). This creates no long-lived URL variant and its freshness never outlives
the index. Metadata paths (`Release`, `Packages`, …) are never delivered this
way. A missing body, a size mismatch, a stale index, a client request that
demands revalidation, or a client that refuses `identity` falls back to the
ordinary upstream path. A stored variant whose body disagrees with the fresh
index is dropped before it can be served.

The index is fetched from `/<mount>/.mdb/apt-index.json`, a Worker-owned
document; the relay only parses that JSON and never parses APT metadata itself.

## Metadata prefetch

A lease-based, demand-driven refresh (ADR 0014). The relay creates a lease only
when it serves a response carrying `X-MDB-Relay: prefetch`. Nothing is persisted
and nothing is discovered by scanning, so a restart starts with an empty table
and waits for real traffic.

- **3-day lease.** `GET`/`HEAD` for the same resource renews it. After three days
  without demand the lease and any pending refresh are dropped.
- **Refresh follows freshness.** At the resource's freshness expiry the relay
  sends a conditional `GET` (`If-None-Match` / `If-Modified-Since`). `304`
  folds new freshness and headers into metadata, `200` replaces the stored
  variant, `404`/`410` deletes it. A resource that states no freshness is not
  refreshed automatically — a live request still revalidates it.
- **`no-store` wins.** A prefetch lease never makes a `no-store` response
  storable; the conflict is logged once per state change.
- **APT is one index task per mirror.** Every `Release`/`InRelease` on an
  apt-tagged mirror renews the same task, whose target is the mirror's fixed
  `/.mdb/apt-index.json`. The index's freshness is the earliest `Valid-Until`
  across its suites, or one hour from generation when none is stated. A `304`
  can extend the current generation's deadline.
- **Bounded and quiet on failure.** Default 4 concurrent refreshes and 4096
  in-memory tasks, configurable; at the task limit a new lease is refused with a
  warning and no active lease is evicted. Expiries carry a small jitter. A
  failed refresh backs off 1, 5, 15, then 30 minutes (cap) and never deletes the
  last good data. Everything is disabled while `CACHE_DIR` is empty. A body is
  still bounded by `CACHE_MAX_SIZE`.

The refresh replays the variant's `Vary`-matched request headers and pins
`Accept-Encoding: identity`, so it never invents a variant or stores a
transcoded body. A background run is always `GET`: a `HEAD` may create or renew
a lease, but never becomes the refresh method.

## `relay prune` — offline cache cleanup

Deletes every cached object this relay's index does not know about:

```bash
sudo systemctl stop mirror-relay
sudo -u mirror-relay env CACHE_DIR=/var/cache/mirror-relay CACHE_CAPACITY=128GiB \
  /usr/local/bin/relay prune
```

It is an **offline** command: it opens the same `CACHE_DIR` (the bbolt lock
refuses a second instance while the relay is running) and builds no server, edge
selection, certificates or manifest watcher. Stop the relay first.

> **WARNING — with `S3_BUCKET` configured this deletes from the SHARED cache.**
> Every object this relay does not index is removed, including objects written by
> other relays and by relays that are online right now. Only run it against a
> bucket no other relay depends on, or when you deliberately want to drop
> everyone's cache. Deleted content is fetched upstream again; the upstream data
> is unaffected.

What it does:

- completes deletes that earlier evictions queued but could not finish;
- drops this instance's index rows whose body is gone (local: a file stat; S3:
  one HEAD per indexed body), drops alias rows whose primary record is missing,
  and drops variant metadata whose body record is gone. This is a *fail-closed*
  cleanup: a permission, timeout or I/O error keeps the index row and fails the
  run, so a broken bucket cannot silently empty the index;
- then lists the configured bucket prefix (local: the data directory) and
  deletes bodies and pointer objects that are not in the index. The index
  cleanup runs first, so a body another relay removed also loses its pointers
  in the same pass — no second run needed. Keys outside the prefix, and
  unrecognised keys under it, are never deleted.

Normal reads and the startup reconcile never list a shared bucket; this command
is the only listing path. `stale` counts index rows removed (missing bodies plus
dangling aliases). It prints the counts and exits non-zero if anything could not
be checked, listed or deleted, after finishing the rest:

```
$ relay prune
prune: scanned=1204 kept=980 deleted=224 pending=0 stale=1 skipped=2 failed=0
```


## systemd

`sudo ./relay install` handles everything: creates user, copies binary,
writes systemd unit, opens config for editing.

To update: `sudo ./relay install` again (detects existing install, offers
binary update + restart).
