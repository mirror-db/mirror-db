# Mirror Relay

Domestic reverse proxy for mirror-db. Discovers mirrors from upstream Worker,
replaces relay domain with upstream domain while preserving paths.
Uses Cloudflare fastest-node discovery to optimize edge routing.

## Build

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

acme:
  source: acme                # "acme" = DNS-01; "upstream" = sync from upstream.url; or a URL origin
  email: acme@mirror-db.net
  ca: production
  agree: true
```

All keys can be overridden via env: `RELAY_POOL_SIZE=30`, `UPSTREAM_URL=...`,
`UPSTREAM_DISGUISE_PORT=344`, etc. (dots → underscores, uppercased).
Two have explicit names:

- `RELAY_CONFIG_DIR` — config/key/cert directory (see above).
- `RELAY_BASE_DOMAINS` — JSON array, replaces the yaml list entirely:
  `RELAY_BASE_DOMAINS='[{"domain":"r.example.com","cf_api_token":"tok"}]'`

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
6. Applies the first matching policy to the mirror-relative request path and
   selects its latency or speed transport. Unmatched requests use latency.
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
by oversight ([ADR-0012](../cft/docs/adr/0012-measure-both-pools.md)).

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

## systemd

`sudo ./relay install` handles everything: creates user, copies binary,
writes systemd unit, opens config for editing.

To update: `sudo ./relay install` again (detects existing install, offers
binary update + restart).
