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
  base_domains:
    - domain: relay.example.com
      cf_api_token: ""        # CF API token for DNS-01

upstream:
  url: "https://mirs.uk"     # upstream Worker or upper relay
  # pool_size: 25             # CF edge node pool capacity (default 25)
  # disguise_port: 344        # connect to upstream via SSH-disguised TLS
  # proxy: ""                 # http(s)/socks5 proxy; bypasses the CF optimizer

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

1. On startup, loads CF edge nodes from `cf-nodes.csv` in config dir (pool size default 25, active rotation 3 IPs)
2. Fetches mirror manifest from `GET <upstream>/api/mirrors`. A failure is not
   fatal: the relay serves `502` until the first fetch succeeds, then refreshes
   every ~10 min (jittered) and keeps serving the last good manifest on errors.
3. Listens on :443 (HTTPS) + :80 (HTTP redirect/keepHTTP)
4. Routes by Host header, replacing relay domain with upstream domain:
   - `<sub>.<relay-domain>` → proxies to `<sub>.<upstream-domain>` with same path
   - `<relay-domain>` → proxies to `<upstream-domain>` with same path
5. Sets `X-MDB-Relay-Host` **only when the client did not already send one**, so
   a multi-tier chain keeps the origin closest to the user.
6. Refreshes mirror list every 10 minutes

### `X-MDB-Relay-Host`

The Worker reads this header (`server/relay.ts`) to build absolute URLs —
Helm chart tarballs, npm tarballs, install scripts — that point back through
the relay instead of straight at the Worker.

The relay treats it as **client-supplied and trusted**: anyone may proxy
(this is an open project) and the value only influences generated links, never
authorization. It is forwarded untouched; the relay fills in its own base
domain only when the header is absent. Relay-originated requests
(`/api/mirrors`, `/speedtest?passthrough`, cert-pack sync) do not set it.

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
