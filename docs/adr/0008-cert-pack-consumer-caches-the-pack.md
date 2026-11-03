# ADR 0008 — A cert-pack consumer must load the pack into certmagic's cache

- **Status:** accepted
- **Date:** 2026-11-03

## Context

A relay with `acme.source: acme` issues its own certificates: `ManageSync` puts
them in certmagic's cache and renews them in place. A relay with
`acme.source: <url or "upstream">` is a pure consumer — it fetches a tar of its
parent's certmagic storage (`/.mirror-relay/cert-pack`), keeps it in memory as a
read-only `certmagic.Storage`, and issues nothing.

The consumer served nothing. Every handshake came back with

```
TLS handshake error: no certificate available for 'anitya.net'
```

while the pack had refreshed successfully with 24 entries — including
`certificates/acme-v02.api.letsencrypt.org-directory/anitya.net/anitya.net.{crt,key}`.

The cause is in certmagic's `getCertificate`:

```go
loadDynamically := cfg.OnDemand != nil || cacheAlmostFull
```

Storage is consulted only when certmagic is **on-demand managing** the name or
when the cache is nearly full. A cert-pack consumer does neither — it skips
`Manage` on purpose, so the cache stays empty — so the loaded pack was never
read, and every lookup fell through to `no certificate available`.

## Decision

In cert-pack mode, load the pack into the cache explicitly after building the
cache, and re-load it every 30 minutes.

- Certificates are added as **unmanaged** (`CacheUnmanagedCertificatePEMBytes`),
  which matches reality: this relay does not renew them and must not pretend to.
  certmagic's renewal loop ignores unmanaged certificates.
- A reload removes the hashes it loaded last time that are absent now, so a
  renewed certificate replaces its predecessor rather than sitting beside it.
  Without that, `GetCertificate` could keep selecting the old one until it
  expired.
- A pack with no `certificates/` directory (or none yet) loads zero and is not
  an error: the parent may still be issuing, and serving nothing is correct
  until it does.
- The pack's own hourly refresh is lazy (`CertClient.Load`/`List` call
  `Refresh()`), so the 30-minute ticker is what turns "the parent renewed" into
  "this relay serves the renewed certificate".

The path walk is recursive. The pack is keyed `certificates/<issuer>/<name>/…`,
so a non-recursive listing of `certificates` stops one level too high — and
`CertClient.List` was returning full file paths for a non-recursive request,
which is not the storage contract (a directory is a key of its own). Both are
fixed: the walk recurses, and `List` now reports immediate children.

## Consequences

- A consumer's TLS works from the first handshake, and picks up rotations within
  half an hour — far inside any renewal window.
- `acme.source` pointing at a parent is now a supported topology for real, not
  just in principle. This is the shape every second-level relay uses.
- If the pack fetch fails at startup the process still exits
  (`log.Fatalf` in `buildCertClient`): a relay with neither certificates nor a
  way to get them has nothing to serve, and a restart loop with a clear log line
  is more honest than a listener that fails every handshake.
