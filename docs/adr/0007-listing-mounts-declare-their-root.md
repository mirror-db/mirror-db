# ADR 0007 — A listing mount is declared by its upstream root, not its name

- **Status:** accepted (2026-11-03, narrowed) — the root invariant is
  enforced by fixing each mount's base; the ordering/marking rules are not
  implemented yet
- **Date:** 2026-11-03

## Context

File-tree mounts are declared in `server/mirrors/web-mirrors.ts` as a name plus
a base URL. The name is a pretty label; the base URL decides whether the mount
can work at all. Nothing in the type system, the manifest or the tests connects
the two, and the mismatch is live:

| mount | base | upstream root? |
|-------|------|----------------|
| `cuda` | `…/compute/cuda/repos/` | autoindex (`<ul class='directorycontents'>`) — **only** at `repos/`; `…/compute/cuda/` is 404 |
| `microsoft` | `packages.microsoft.com/` | autoindex, but the useful root is `repos/` (`config/`, `keys/`, `yumrepos/` are site chrome) |
| `debian-cd` | `cdimage.debian.org/debian-cd/` | autoindex — but it holds `13.7.0/` while the Debian site links `current/`; both exist and diverge silently |
| `ubuntu-cloud` | `cloud-images.ubuntu.com/` | autoindex — but releases are `releases/<codename>/` **or** date-stamped, and `daily/` is a rolling tree |
| `ubuntu-releases` | `releases.ubuntu.com/` | autoindex |
| `docker-ce` | `download.docker.com/` | autoindex |

So "file tree" is not one behaviour: the upstream layout is part of the
mirror's contract, and the four layouts above are the ones actually in use.

## Decision

1. A file-tree mount declares **what its root is** in a small enum
   (`archive`, `release-tree`, `rolling-release-tree`, `autoindex`), which
   carries the conventions the mirror may assume: which directories are stable
   vs moving, whether subdirectories are versioned or codenamed.
2. Any listing generated from such a mount is ordered **stable first, moving
   after**, and marks moving entries (`current/`, `latest/`, `daily/`) instead
   of presenting them as peers of the pinned ones.
3. A mount must point at a listing root, verified by the probe set (ADR 0006):
   a base that answers 404 or serves no parseable listing fails its probe.

## Consequences

- `cuda`'s base is correct only as `…/compute/cuda/repos/`; that constraint
  becomes declarative instead of folklore.
- `cloud.debian.org`'s rolling `latest/` symlinks (rewritten in place per SRU)
  stop being advertised as the primary path; pinned `bookworm/20230531-1397/`
  style dirs sort first.
- `web-mirrors.ts` stops being "name → URL" and becomes a semantic table —
  which is also what the UI needs to render stable/rolling badges.

## Alternatives considered

- *Infer the layout from the parsed listing at runtime.* The parser sees names,
  not conventions; `current/` and `13.7.0/` are indistinguishable without a
  rule. Encoding the rule once per mount is more honest than guessing per
  request.
- *Drop the moving aliases from the listing.* Breaks links people already have;
  marking them is enough.
