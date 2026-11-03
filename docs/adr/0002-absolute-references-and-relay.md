# ADR 0002 — Every absolute reference is built from the relay host

- **Status:** accepted (2026-11-03) — npm metadata, the APT directory
  redirect and `relayInfo()` validation are implemented
- **Date:** 2026-11-03

## Context

The mirror set is reachable through an optional **relay**, which forwards to the
Worker after rewriting the host. In the Worker, `request.url` is always the
Worker's own origin; the host the user actually dialled only travels in
`X-MDB-Relay-Host`. The relay's whole reason to exist (an alternate transport
for networks where the Worker's edge is reachable but throttled) is defeated the
moment a response hands the client a Worker-origin URL: the client follows it
and leaves the relay for good.

Today the situation is inconsistent:

- Helm indexes, the mirror-manifest API and the install scripts use
  `relayInfo(request)`.
- npm metadata rewrites tarball URLs with `new URL(req.url).origin` — the
  Worker origin.
- The APT directory cross-slash redirect uses `new URL(request.url)` and a
  `301`, which clients cache permanently.

`X-MDB-Relay-Host` is client-controllable (the project is open by design), so
using it is only safe if its value can never do more than pick a hostname in an
URL the client asked for anyway.

## Decision

1. **One helper, one rule:** an absolute reference is only ever produced by
   `relayInfo()`. Code that builds a URL for the response body or a `Location`
   header must not touch `request.url`'s origin.
2. **Validate before use:** the relay host must be a bare hostname
   (`[a-z0-9.-]+`, no port, no path, bounded length). Anything else falls back
   to `BASE_DOMAIN`. Never interpolate it into a template that could change the
   URL's structure.
3. **No origin in redirects:** directory redirects are relative (`/debian/...`
   → `/debian/.../`) or derived from the requester's own host, and use `308`
   (method- and body-preserving) except for the static-site root.

## Consequences

- A relay-only user never receives a Worker-origin link; the relay's
  `X-MDB-Relay-Host` passthrough (first relay wins) is the contract this relies
  on.
- A forged relay host can only produce links to the forger's own host — no
  escalation, because no mirror treats the header as an identity.
- Redirects stop being permanent caches pointing at a host that may change.

## Alternatives considered

- *Make the relay rewrite response bodies.* Fragile (streamed bodies, JSON, XML,
  binary) and would put mirror knowledge into the transport. Rejected: the
  Worker knows its own absolute references; the relay cannot know them all.
- *Serve everything under relative URLs.* Already true where possible (PyPI
  Simple, WebDAV), but npm clients and Helm index readers resolve relative
  files against the document, not the registry, so some absolute forms are
  unavoidable.
