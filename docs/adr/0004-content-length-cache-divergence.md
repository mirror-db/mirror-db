# ADR 0004 — The Cache API rewrites `Content-Length`; serve stored bytes as stored

- **Status:** accepted (note, no code consequence beyond ADR 0003)
- **Date:** 2026-11-03

## Context

`cache.match()` does not return the object that was put: it normalises
`Content-Length` to the body it holds. Reproduced inside workerd: storing a
100-byte body with no length and matching it yields `content-length: 100`;
storing a 1,000-byte body declared as 100 bytes yields `content-length: 1000`
and 1,000 bytes back.

That divergence is real at the API level, but it is a *correction*, not a
corruption: the stored body is intact, and the length reported afterwards
describes it. Live probing agreed — `GET /npm/hono/-/hono-4.13.16.tgz` returns
`content-length: 246805` and exactly 246,805 bytes, byte-identical to upstream.

The one thing that was genuinely wrong was visible bookkeeping: ADR 0003 has to
persist the store time *inside* the stored headers, and those headers were being
copied to clients.

## Decision

1. Serve the stored body as stored. Do not "fix" `Content-Length` — the Cache
   API's value is the correct one for the bytes it returns.
2. Strip the manager's own bookkeeping headers (`x-mdb-stored-at`,
   `x-mdb-revalidate-after`) on the way out, in one place, together with adding
   the `x-mdb-cache` outcome header.
3. Keep the existing size guard (512 MB, the Free/Pro per-object limit) and the
   `Range` bypass: a `206` is never stored.

## Consequences

- Clients never see cache internals; probes can still assert the outcome via
  `x-mdb-cache`.
- Objects with unknown length gain one on a cache hit, which clients accept.
- No behaviour change for content-addressed (digest) entries: they are stored by
  the OCI proxy, not by this manager, and keep their own key and headers.

## Alternatives considered

- *Strip `Content-Length` from every stored response.* Discards a correct,
  useful header and forces chunked encoding on clients that could have shown
  progress.
