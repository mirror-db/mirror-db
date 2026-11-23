# ADR 0011 — Response-controlled request caching: `X-MDB-Relay` decides what may be stored

- **Status:** accepted (2026-11-23, implemented — not deployment-verified)
- **Date:** 2026-11-23

## Context

M1 added a content-addressed store with an LRU and a public read-only
`/hash-content` interface. M2 wires request caching into the reverse proxy
(implemented 2026-11-23; not deployment-verified). An
earlier draft made caching opt-in per manifest policy (`cache`, `cache_ttl`),
which conflated two different kinds of knowledge:

- whether the relay caches at all is a property of the deployment (does it have
  a store?) and of the resource (may it be stored?), never of the Worker's
  manifest;
- how long a response stays fresh is a property of that response.

A distributed manifest cannot answer either well. An operator enabling a local
store would still wait for the Worker to mark paths cacheable, the common case
would be default-off, and a manifest lifetime would apply to every relay at every
moment regardless of what the upstream just said.

## Decision

1. `CACHE_DIR` is the switch. Empty: no store, every request passed through.
   Non-empty: every GET/HEAD that resolves to a manifest mirror attempts a cache
   lookup, and a complete `200` GET is writable by default. No path is opted in
   through the manifest.
2. Writes are still refused by prohibitions that only the response can state:
   `X-MDB-Relay: no-store`, `Cache-Control: no-store`, `private`, a non-`200`
   status, an incomplete body, `206`, and `Vary: *` unless the response also says
   `no-vary`. A client's own `Cache-Control: no-store` skips lookup and write for
   that exchange, and `no-cache` / `max-age=0` force revalidation of a fresh
   entry before it is served.
3. `X-MDB-Relay` is a generic response-control header travelling upstream →
   relay, consumed and removed before delivery except on a passthrough path.
   Keywords are `no-store`, `no-vary`, and `ttl=<seconds>` — a fallback lifetime
   used only when the response states no freshness (`s-maxage`, `max-age` and
   `Expires` are preferred in that order), defaulting to `0` (no fallback, not
   "never expires"). Unknown keywords are ignored. Standard HTTP prohibitions
   such as `no-transform` are honoured identically. Future response-side controls
   are added as keywords, never as manifest fields.
4. `no-vary` overrides `Vary: *`: the path alone identifies the resource. Its
   scope is limited to variant identity; it does not relax `no-store`, `private`,
   `no-transform`, the digest/transcode prohibition, or policy `passthrough`.
   The first batch is the eleven `createAptMirror` mounts — `debian`,
   `debian-ports`, `debian-security`, `ubuntu`, `ubuntu-ports`, `mise`,
   `syncthing-pkgs`, `hashicorp`, `opentofu`, `github-cli`, and
   `gitlab-runner-deb`. That list states an upstream property, not a cache
   policy; mirrors absent from it keep ordinary cache eligibility.
5. The only cache-adjacent policy field is `passthrough`, a pre-request
   statement (ADR 0012): a matching request skips cache and response control
   entirely.
6. `X-MDB-Relay` describes a response only. A request that did not arrive
   through a relay — identified by the absence of `X-MDB-Relay-Host` — has any
   inbound `X-MDB-Relay` request header removed by both the relay and the Worker.
   Response headers are unaffected.

## Consequences

- Enabling or disabling cache is one deployment decision, not a manifest
  rollout, and a relay with no store cannot be told to cache by an upstream.
- Freshness comes from the response. A relay-wide policy TTL no longer exists:
  a mirror that needs a fallback says so per response, where the knowledge
  actually lives.
- The manifest shrinks: no cache fields to keep in sync with relay disk capacity
  or with each operator's tolerance for staleness.
- `no-vary` is a permanent claim each listed mirror must keep true. Adding a
  mirror requires the same evidence that the bytes at a path do not depend on
  request headers. Removed from the list, a mirror keeps working, just with
  `Vary`-driven variants again.
- A direct request can no longer carry response-control semantics inward, and a
  relay-forwarded request never does because the relay strips it first.

## Alternatives considered

- *Cache on/off and lifetime in the policy.* Rejected: wrong knowledge boundary,
  default-off in the common case, and every relay operator would depend on a
  manifest update to use its own store.
- *One header per feature (`X-MDB-Relay-No-Store`, …).* Rejected: the header
  must grow with each response-side control; one keyword list keeps one parsing
  and one stripping rule. The old name is retired, not aliased.
- *Keep `Vary: *` an absolute veto even under `no-vary`.* Rejected: APT paths
  genuinely do not vary, and `no-vary` is exactly that claim. Scoping it to
  variant identity keeps every other protection intact.
- *A separate operator config file to enable caching.* Rejected: `CACHE_DIR`
  already is the store switch, and a second switch can disagree with it.
