# ADR 0001 — Mirrors are taxonomised by kind, and the kind decides the front door

- **Status:** rejected (2026-11-03) — the classification was not adopted; a
  per-mirror capability flag is the fallback if the manifest ever needs to
  advertise listing support
- **Date:** 2026-11-03

## Context

Today a mirror becomes browsable or opaque by which *factory* happened to be
used to declare it (`createWebListMirror` vs `createPassthroughMirror`, both in
`server/mirrors/web-mirror.ts` — the same config type, the same name). The
`Mirror` type itself only knows `host` / `path` / `keepHTTP`; nothing about it
says what the mirror serves or what `?format=json` / `PROPFIND` are supposed to
mean for it.

That has visible consequences on the live Worker:

- Browsable file trees and opaque ones are indistinguishable through
  `/api/mirrors`, so consumers (relay, client UIs, the smoke-test script) must
  hardcode which mounts answer listings.
- A WebDAV `PROPFIND` against an opaque mount (e.g. `/nodejs/...`) is proxied
  upstream and comes back as whatever the origin says, with an origin `<href>`
  in the body — a cross-origin reference escaping the mirror.
- Adding a mirror is ambiguous: nothing forces the author to decide whether the
  upstream has a listing at all, which is exactly the mistake behind the
  `cuda` / `docker-ce` configs (clean names, base URLs that are not listing
  roots).

## Decision

Give `Mirror` an explicit **kind**, and let the kind own the protocol surface:

| kind | path routing | listing modes | APT enforcement | status |
|------|--------------|---------------|-----------------|--------|
| `registry` | subdomain | — | — | optional |
| `apt` | path | HTML + JSON + WebDAV | yes | yes |
| `package-index` | path | — | — | — |
| `charts` | path | — | — | — |
| `file-tree` | path | HTML + JSON + WebDAV | no | — |
| `utility` | switch | — | — | — |

`file-tree` replaces the two factory configs; the "browsable" vs "addressed"
distinction is carried by the *upstream* (does it answer a directory URL with an
autoindex page), not by the mirror name, so the same code path serves both. The
JSON manifest exposes the kind so consumers never guess.

## Consequences

- One dispatch table decides which front-door features a mount may have; a
  passing-through mount can no longer accidentally answer `PROPFIND` with an
  upstream body.
- The config file for file trees must state the upstream listing root. It is no
  longer enough to write a pretty name.
- Renaming a mount is a rename of its kind entry too — the smoke tests read the
  manifest instead of hardcoding names.
- `web-mirror.ts` disappears; its two config types collapse into one.

## Alternatives considered

- *Keep the factories, add a boolean `browsable`.* Cheapest, but leaves APT and
  the package indexes outside the taxonomy — the ambiguity simply moves one
  level up.
- *Infer browsable-mode from the upstream at first request.* Tempting (fetch the
  directory and see if it parses), but makes the protocol surface of a URL
  depend on network state; a listing outage would silently downgrade a mount.
