# ADR 0005 — Registry auth is scoped to the upstream's own origin, off the edge

- **Status:** rejected (2026-11-03) — accepted risk: the upstreams are
  operator-chosen, and the added machinery buys little for an open mirror
- **Date:** 2026-11-03

## Context

`proxyRegistryRequest` follows an RFC 7235 Bearer challenge: it reads `realm`
from the upstream's `WWW-Authenticate`, fetches a token there (with Basic auth
when credentials exist), and retries the original request with
`Authorization: Bearer …`. Anything that answers a mirror's registry mount can
therefore choose where the mirror sends its next authenticated request.

The intended challenge is always on the upstream's own host, except for one
documented case: registries hosted behind Cloudflare's edge
(`registry.npmjs.org` was verified to answer with `cf-ray`), where the token
endpoint lives on a different host than the content endpoint.

## Decision

1. A challenge is followed only when its `realm` is on the upstream host **or**
   the original response was served through Cloudflare's edge (`cf-ray`
   present). Any other realm is ignored and the 401 is returned sanitised.
2. `Basic` credentials are only ever sent to a token endpoint reached over
   `https` that passes the check above.
3. A downstream redirect hop carries `Bearer` (and `Basic`-derived token) only
   when the hop stays on the *original* upstream origin. CDN targets get none.

## Consequences

- A malicious upstream cannot collect the mirror's registry credentials by
  naming a third-party realm.
- The Cloudflare-hosted exception keeps same-ecosystem registries working
  without listing them by name; it is keyed on a property of the *response*,
  not on a hardcoded host table.
- A registry that legitimately moves its token endpoint off-host and is not
  behind Cloudflare is not supported; such a mirror must be declared explicitly
  (allow-list entry) rather than by relaxing the rule.

## Alternatives considered

- *Allow-list the realm per mirror.* Explicit but duplicates knowledge upstream
  already publishes; adds a config knob per registry and rots.
- *Only allow same-host realms.* Breaks the Cloudflare-hosted registry case,
  which is a real upstream today.
