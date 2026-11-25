# ADR 0014 — Prefetch is demand-driven, response-directed, and relay-local

- **Status:** accepted (2026-11-23, implemented — not deployment-verified); adds the
  `prefetch` keyword to the header defined by
  [ADR 0011](0011-response-controlled-request-cache.md) without superseding it
- **Date:** 2026-11-23

## Context

M2 ([ADR 0011](0011-response-controlled-request-cache.md),
[ADR 0012](0012-policy-field-merge-and-passthrough.md)) draws a hard line: a
policy carries only what the relay must know before it asks upstream (transport
preference, passthrough), and everything knowable only from the response is a
relay directive. Keeping metadata fresh is knowledge of the second kind — a
mirror decides per response that this particular `Release` or index is worth
refreshing, and only after it answered a real request does anyone know the relay
actually serves it.

An earlier draft made prefetch a manifest policy field marking paths. That
inverts the boundary: a distributed rule would claim, for every relay and every
moment, that a path should be refreshed, even for repos no client has ever
touched. It also has no natural bound. "Refresh these paths periodically" applied
to a PB-scale archive is a crawler, and a resource whose response states no
freshness would be refreshed forever with no way to tell whether anything
changed.

Demand is the only cheap signal available. A relay already knows which
resources it served recently. Prefetch should be an extension of that demand, not
a substitute for it: a resource nobody requested is never refreshed, and a
resource that stops being requested stops being refreshed.

## Decision

1. **A response directive, not a policy field.** `prefetch` joins `no-store`,
   `no-vary` and `ttl` as an `X-MDB-Relay` keyword (ADR 0011's mechanism). A
   mirror emits it on responses it considers worth keeping fresh. No manifest
   policy field, no mirror manifest change, and no additional distribution
   channel: a relay learns eligibility only by serving the response.

2. **A lazy, in-memory lease.** Serving a prefetch-marked response through the
   relay creates a relay-local lease for that resource with a three-day initial
   retention. A later `GET` or `HEAD` for the same resource renews it to three
   days. Three days without demand deletes the lease and any pending refresh.
   Leases live only in memory: they are never persisted, never discovered by
   scanning an upstream, and never restored after a restart. A restarted relay
   waits for real traffic to teach it again.

3. **Refresh is driven by freshness, never by a clock alone.** While a lease
   exists and the resource reaches its freshness expiry, the relay issues a
   conditional background `GET` (`If-None-Match` / `If-Modified-Since`). `304`
   folds new freshness and headers into the entry's metadata, `200` replaces a
   storable variant atomically, and `404`/`410` drops it. A resource that states
   no freshness is not refreshed on a loop: without an expiry there is nothing to
   refresh towards. A live request for a stale resource still takes M2's
   synchronous revalidation and renews the lease; the background task does not
   duplicate it.

4. **`no-store` wins, visibly.** A prefetch lease never makes a response
   storable. When a response carries both `no-store` and `prefetch`, no ordinary
   request-cache entry is written and a warning is logged once per resource —
   not once per request. For APT this is the normal case: mutable `Release` and
   `Packages` are not stored as client-facing variants at all; a refresh updates
   only the independent normalised index and its digest expectations
   ([ADR 0013](0013-worker-owned-apt-metadata-index.md)).

5. **APT refreshes one aggregate index per mirror.** `Release`/`InRelease`
   responses carry `prefetch`. All of a mirror's release requests register and
   renew a single fixed task whose target is `/<mount>/.mdb/apt-index.json`,
   never one task per `Packages` file. The index's freshness is the earliest
   `Valid-Until` across its suites; if none is present, a successfully built
   index is fresh for one hour from generation, and `Date` is not used as a
   lifetime. The endpoint advertises matching `Cache-Control` and a
   whole-generation `ETag`, so the background refresh is a conditional `GET`
   that a `304` renews and a `200` replaces as a new generation.

6. **Ordinary resources are one task per address and variant.** A non-APT
   resource's task is keyed by URL plus variant identity. The background request
   replays that variant's request headers and pins `Accept-Encoding: identity`,
   so a refresh never invents a new variant and never stores a transcoded body.
   `HEAD` can create or renew a lease, but a background refresh is always `GET`,
   and one task never has two refreshes in flight.

7. **Bounded by configuration, failing quiet and open.** Default four
   concurrent background requests, default 4096 in-memory tasks; both
   configurable. At the task limit a new lease is refused with a warning and no
   active lease is evicted. Expiries carry a small jitter. A failed refresh backs
   off at 1, 5, 15 and 30 minutes, capped at 30, and never deletes the last good
   data. A single body remains bounded by `CACHE_MAX_SIZE`, and prefetch is
   disabled entirely when `CACHE_DIR` is empty.

## Consequences

- No policy, manifest or distribution change. Prefetch reuses the header that
  already exists, and the relay's scheduler is local state, consistent with
  ADR 0011's "response-side decisions are header-driven".
- Traffic is proportional to recent demand, not to repository size. An untouched
  mirror costs nothing, and an abandoned path disappears after three days.
- A restart forgets everything, so the first refresh after a restart always
  follows a real request. This is deliberate: a persisted job list would be a
  local cache of upstream facts with no freshness of its own.
- APT's `no-store` metadata does not become cacheable, and the index refresh path
  is separate from the client-facing request cache. The warning exists so the
  conflict is visible without spamming logs or weakening storage rules.
- A resource whose response never states freshness is renewed by live traffic
  only. That is a limit, not a bug: there is no safe background cadence to
  invent for it.
- The lease is per relay. Two relays learn independently, which is already how
  request-cache metadata behaves.

## Alternatives considered

- *A manifest policy field (`prefetch` / `prefetch_ttl`) marking paths.*
  Rejected: it asserts demand that may not exist, cannot reflect per-response
  decisions, and puts response-side knowledge back into policy against ADR 0011
  and ADR 0012.
- *Persist leases so a restart keeps refreshing.* Rejected: the lease is a
  statement about local demand; persisting it turns a forgotten resource into a
  permanent job and makes a restart resume traffic nobody asked for.
- *Refresh on a fixed period regardless of freshness.* Rejected: it cannot
  terminate for a resource with no expiry, and it fetches unchanged content on a
  schedule.
- *One task per `Packages` file for APT.* Rejected: a repository has thousands of
  metadata files, while one generation covers them all; per-file tasks multiply
  concurrency and cannot switch generations coherently.
- *Prefetch as a cache-read behaviour (serve stale and refresh in the
  background).* Rejected: it would make a client request the trigger, blur
  demand with refresh, and leave no way to refresh an unrequested resource that
  is still under a live lease.
- *Promote a prefetch-marked `no-store` response into a stored entry.* Rejected:
  `no-store` is a prohibition from the response, and a refresh hint is not a
  storage permission.
