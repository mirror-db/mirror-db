# ADR 0003 — Revalidation is time-based, not `max-age`-based

- **Status:** accepted
- **Date:** 2026-11-03

## Context

Read-through cache entries were served until the Cache API evicted them. Two
consequences:

- A mutable upstream document (npm metadata, PyPI Simple page, a directory
  listing) could be served stale for an unbounded time, because nothing in the
  front door ever looked at when the entry was stored — the Cache API does not
  expose that, and `age`/`date` are stripped on the way in.
- Conversely, `cachedfetch` skipped caching entirely for anything with a
  `Range` header, and the only lever on freshness was the upstream's own
  `Cache-Control`, which is often absent (autoindex pages) or conservative.

An earlier hypothesis blamed this on `Content-Encoding` transposition (a stored
gzip body re-served to a client that cannot decode it). Measurement did not
support it:

- `GET /npm/hono/-/hono-4.13.16.tgz` through the mirror is byte-identical to
  upstream (same SHA-1, 246,805 bytes), identity-encoded, five runs out of five.
- Sizes that looked truncated (74 KB / 112 KB of 246 KB) came from a congested
  direct path, not from the Worker: the same requests over a working path
  complete in ~1.2 s with exact lengths.
- Responses carrying `content-encoding: zstd` with a "small" `size_download`
  were correctly compressed; the client simply did not decode.

So the defect was never transcoding. It was **freshness**: nothing bounded how
long a mutable entry could be served, and nothing let the mirror notice an
upstream change cheaply.

## Decision

1. **Stamp every stored entry** with when it was stored (`x-mdb-stored-at`) and
   a revalidation interval (`x-mdb-revalidate-after`, default 10 minutes). The
   Cache API has no notion of store time, so it has to travel with the object.
2. **Re-check the source on a clock, not on `max-age`.** Once per interval the
   front door issues a conditional `HEAD` (`If-None-Match` / `If-Modified-Since`).
   A `304` re-stamps the entry and transfers nothing; a `200` refetches and
   replaces it; `404`/`410` drops it; an unreachable source keeps serving the
   stored copy.
3. **Decouple the advertised lifetime from the revalidation cadence.** Because
   the mirror re-checks every 10 minutes anyway, the advertised `max-age` is
   lifted to that floor (`liftCacheControl`). A copy that a client or the edge
   may hold is never staler than the check the mirror performs regardless.
   `no-store` / `no-cache` are never lifted — an upstream that forbade caching
   keeps forbidding it.
4. **Say what happened:** every cached fetch carries `x-mdb-cache:
   hit | revalidated | miss | bypass`, and the bookkeeping headers are stripped
   before the response leaves.

## Consequences

- One extra `HEAD` per cached object per 10 minutes of *activity*, not per
  request: the interval is anchored to the last store/re-stamp.
- A `HEAD` that upstreams answer badly (`405`/`501`) is handled by deleting the
  entry and doing a normal `GET`, so such upstreams degrade to plain
  read-through rather than breaking.
- `Range` requests still bypass the store; they now report `x-mdb-cache:
  bypass`.
- Entries written outside the manager (e.g. the OCI digest store) have no stamp
  and are served until evicted — correct for content-addressed data, which
  cannot change under its digest.

## Alternatives considered

- *Cache-Control only, trusting upstream `max-age`.* It is absent or wrong on
  autoindex pages, and 300 s is both too long for a release index and too short
  for a badge SVG.
- *Revalidate on every request.* Turns a cache into a proxy; with 10-minute
  intervals and a single HEAD per interval, the current design costs ~0.17 % of
  the requests it serves.
- *Store per-`Accept-Encoding` variants to "fix" the compression mismatch.*
  There was no mismatch to fix; it would have multiplied cache keys for nothing.
