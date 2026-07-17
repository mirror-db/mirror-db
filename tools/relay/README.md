# Mirror Relay

Domestic reverse proxy for mirror-db. Discovers mirrors from upstream Worker,
replaces relay domain with upstream domain while preserving paths.
Uses Cloudflare fastest-node discovery to optimize edge routing.

## Build

```bash
go build -o relay
```

## Config

Config file: `/etc/mirror-relay/config.yaml` (or `./config.yaml`).

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

acme:
  source: acme                # "acme" = DNS-01; "upstream" = sync from upstream.url; or a URL origin
  email: acme@mirror-db.net
  ca: production
  agree: true
```

All keys can be overridden via env: `RELAY_POOL_SIZE=30`, `UPSTREAM_URL=...`,
`UPSTREAM_DISGUISE_PORT=344`, etc. (dots → underscores, uppercased).

## Run

```bash
# Direct
./relay serve

# Install as systemd service (interactive)
sudo ./relay install
```

## How it works

1. On startup, loads CF edge nodes from `cf-nodes.csv` in config dir (pool size default 25, active rotation 3 IPs)
2. Fetches mirror manifest from `GET <upstream>/api/mirrors`
3. Listens on :443 (HTTPS) + :80 (HTTP redirect/keepHTTP)
4. Routes by Host header, replacing relay domain with upstream domain:
   - `<sub>.<relay-domain>` → proxies to `<sub>.<upstream-domain>` with same path
   - `<relay-domain>` → proxies to `<upstream-domain>` with same path
5. Sets `X-MDB-Relay-Host` header so upstream can generate correct script URLs
6. Refreshes mirror list every 10 minutes

## SSH Disguise (anti-DPI)

Some ISPs throttle HTTPS traffic via deep packet inspection. The disguise
feature wraps TLS in a fake SSH banner exchange:

```
Client → Server: SSH-2.0-OpenSSH_9.6\r\n
Server → Client: SSH-2.0-OpenSSH_9.6\r\n
(TLS handshake follows on the same TCP connection)
```

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
