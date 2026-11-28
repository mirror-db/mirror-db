# ADR 0016 — Immutable objects assemble through an in-process tiered cache before the hash store

- **Status:** accepted (2026-11-27, implemented — not deployment-verified); consumes
  `rules.immutable` from [ADR 0015](0015-immutable-policy-rule.md) and extends the
  request cache of [ADR 0011](0011-response-controlled-request-cache.md) without
  superseding it
- **Date:** 2026-11-27

## Context

The relay's request cache ([ADR 0011](0011-response-controlled-request-cache.md))
serves one URL variant at a time: a MISS takes the upstream body, tees it to the
client and to the hash store, and only a complete `200` GET becomes an entry
(`streamAndStore`). Every concurrent reader of the same cold object therefore
fetches it independently — M2 deliberately left singleflight out — and a reader
that asks only for a byte range pays for the whole body or waits for one client's
download to finish.

ADR 0015 fixed the missing pre-request fact: `rules.immutable` tells the relay,
before it asks upstream, that the bytes under a path cannot change, and may
capture their content digest. A path with that property does not need variant
identity or a freshness check before its bytes can be shared. It has one
representation — `identity`-encoded bytes at fixed offsets — so readers can share
one assembly instead of each running its own exchange.

The piece that was missing is not storage but assembly. Assembling means
coalescing overlapping byte-range requests from many readers, fetching each range
once, keeping the partial result somewhere addressable while readers consume it,
and publishing the finished object into the content store exactly once. That is a
tiered cache: passive fragment tier in front, complete-object tier behind.

## Decision

### 1. Use `go-tiered-cache v0.0.1` rather than reimplement paging

`github.com/geektr-cloud/go-tiered-cache` (package `tiered`) provides what the
relay needs: one `PassiveCache` per immutable object, byte-range coalescing across
overlapping readers, an authoritative object extent, a `CacheHandoff` hook so a
complete fragment can be adopted in place instead of copied, and the range
splitting needed to fill a hole on demand. Reimplementing that inside the relay
would duplicate a well-tested state machine and add a second range scheduler to
maintain. It is one small published module (see
[`tools/relay/go.mod`](../../tools/relay/go.mod)), not a second body store.

### 2. One process-local entry per key, and the entry is the singleflight

`activecache.Manager` maps an opaque key to one `Entry`. The key is the captured
content digest rendered as `sha256:<hex>` or `sha1:<hex>` when the immutable
pattern named one, otherwise the request URL key (a "temporary URL key"). A digest
key deduplicates across mounts and repositories, because the digest names the
bytes; a URL key only deduplicates concurrent requests for the same path. OCI
**manifests are always URL-keyed**: a manifest's media type and headers belong to
the repository, not to its digest, so one repository's response must not become a
synthetic representation for another repository's digest. Blob and APT by-hash
paths keep the global digest key.

`Acquire` returns the existing entry with an extra reference, or creates a new one
and makes that caller the creator. The creator is the only one that contacts
upstream; it must call `Init` with the range source, the shared metadata and the
declared size, or `Fail`. Every other acquirer waits on `Init` and, when it fails,
falls back to the ordinary chain instead of inheriting a broken entry. That wait
is the singleflight: no second upstream exchange starts while the first creator is
initializing. The manager never queues or blocks on capacity — a refusal is an
error the HTTP layer turns into the ordinary path.

### 3. The complete object still goes to the hash store

The tiered cache's `Store` is the hash store's adopt path (`hashstore.Adopt`,
or `AdoptTrusted` when the assembly digest is complete — ADR 0019), not a second
body store. A fragment lives in one scratch file under `<CACHE_DIR>/.tmp`; once
the cache covers the whole extent it is published into the hash store under its
real digest and the fragment storage is reclaimed. Nothing durable is added: the
hash store remains the only place a complete body lives, with the same LRU,
capacity, S3 backend and `/hash-content` interface as before. A body counts once,
under its digest, no matter how many URL variants or mounts would have stored it
separately.

### 4. `Adopt` verifies every byte for an untrusted caller

`hashstore.Adopt(ctx, path, size, algo, digest)` validates that `path` is a
completed regular file (not a symlink) strictly inside the store's temp directory
at exactly the declared size, reads it once to compute **both** SHA-1 and SHA-256,
and only then publishes. The digest the policy captured is an expectation, not a
shortcut: a mismatch is `ErrDigestMismatch`, nothing is published, and an existing
body under that digest is untouched. The same publish window as `Put` protects the
new body from a concurrent reconcile or an LRU eviction that could otherwise
delete bytes whose index record has not been committed yet; a shared backend rolls
back its upload only when the caller is the last active publisher with no record.

That verification read is the contract for a source the store did not watch being
written. [ADR 0019](0019-precomputed-adoption-digests.md) adds a separate
`AdoptTrusted` for the one caller that hashed the bytes while assembling them;
`Adopt` itself is unchanged and remains the only entry point for anything else,
so "trusts no caller" still describes the default path.

### 5. Two `Store` finalize paths, one publication

`entryStore` implements both `tiered.Store` and `tiered.CacheHandoff`:

- **Handoff (normal):** `FinalizeFromCache(ctx, cache, end)` adopts the complete
  scratch file in place, so the bytes are never copied. The HTTP-layer entry uses
  this path.
- **Copy (fallback):** if handoff is refused, the core streams the cached bytes
  through `entryStore.Write` into its own temp file and calls `Finalize`, which
  adopts that file.

Both paths end in the same publish tail (`publishAdopted` inside the store), so a
body is published exactly once and only at the exact declared size. Since
[ADR 0019](0019-precomputed-adoption-digests.md) the active segment also feeds
both digests from `PrefixSinks` while it caches the object and publishes through
`AdoptTrusted`, which skips the store's verification read only when both sinks
saw the whole extent; every other case still takes `Adopt` and its reread. Before
adoption the entry opens a local descriptor on the source; that descriptor stays
valid after a local backend renames the file into the data tree or after a shared
backend leaves the file for the caller, so readers that are still streaming do
not depend on the scratch name.

### 6. Publication reclaims the fragment; old readers drain on their own descriptor

On a successful publication (`Adopt` or `AdoptTrusted`) the entry leaves the
active map **immediately** — a new
request finds the hash store instead of joining an assembly that is already
finished. Existing readers keep reading their open descriptor until they release.
The scratch name is then removed (`Reclaim` treats a missing file as success,
because adoption already moved or uploaded it). Retirement closes the tiered
cache, the source and the descriptor, and releases the entry's byte accounting
only at that point: a published body still occupies local disk while a reader
holds it. Nothing is truncated to satisfy capacity — only new entries are refused
while the segment is full.

### 7. Immutable GET, including a cold Range, may assemble; everything else keeps the ordinary chain

The active path is entered from `serveCache` only when all of these hold: an
active manager exists, the merged policy matched `immutable`, the client did not
force revalidation (`no-cache` / `max-age=0`), the client accepts `identity`, and
the method is `GET`. A `HEAD` may reuse a body already addressable by digest
(`serveImmutableDigestHit`) but never creates an entry. [ADR 0017](0017-response-declared-immutability.md)
adds a second door with narrower admission: a response that carries
`X-MDB-Relay: immutable` may seed the same assembly on its first MISS, keyed by
its URL rather than by a declared digest.

- **Full GET (zero matching variants):** the first response seeds the entry; the
  body streams from the tiered cache and publishes when complete.
- **Cold Range GET:** the seed request forwards only the first interval of a
  multipart `Range`. A `206` whose `Content-Range` states a total size starts the
  assembly; later intervals and later readers fetch only their missing bytes. A
  `200` answering a ranged seed, or a `206` without a total size, falls back
  because neither gives a bounded object to assemble.
- **Delivery:** complete body, single range, multipart ranges (the existing
  32-range cap), `416`, `If-Range` and conditional evaluation all read from the
  shared fragment. A partial entry is never presented as a complete body.
- **Hole fill without byte ranges:** a missing interval whose range request is
  refused (a `200` answer, or a failure before the reader exists such as `500`)
  is re-read as one complete `GET` and the prefix is discarded while streaming.
  Nothing is buffered or stored for the skipped bytes, and the section is exactly
  the requested length or `io.ErrUnexpectedEOF`. The entry fails only when the
  complete `GET` fails too; a failure after the section already produced bytes,
  or a continuation fragment answered `200`, is terminal and never replayed.
- **Variant commit:** after a delivery that completed the object, the HTTP layer
  flushes and returns, and a detached per-entry waiter (bounded at 60 s) writes
  the ordinary URL variant once publication succeeds, so a later request without
  a digest capture is still served locally. The client's response and its EOF
  never wait for the store.
- **Non-immutable requests are untouched.** They never enter the manager and keep
  `streamAndStore` exactly as ADR 0011 defined it.

Seed requests carry `Accept-Encoding: identity` and only the first range
interval; later range requests reuse the relay host, user agent and `Accept`, never
the creator's cookies or credentials. The response must be storable by the same
rules as the ordinary chain (no `X-MDB-Relay: no-store`, no `Cache-Control:
no-store`/`private`, no `Vary` other than `Accept-Encoding`, no non-identity
`Content-Encoding`, a body, status `200`/`206`), plus the byte-addressing
requirements. An unknown or zero length, a body above `CACHE_MAX_SIZE` (the store
would reject the adoption), a full segment, a low disk watermark, or any
initialization failure delivers the already-fetched response through the ordinary
path instead of asking upstream twice.

### 8. `SequentialCache` is rejected

The module also offers `SequentialCache`, which shares one stream among readers
that all start at offset zero. It requires pulling the complete prefix before any
mid-object read can proceed, and it has at most one upstream. Relay readers are
the opposite: OCI clients resume layers at arbitrary offsets, APT clients fetch
one `Packages` file from the middle of a path tree, and multipart ranges ask for
disjoint spans at once. A sequential cache would turn each of those into a full
download and could not answer `416` or a bounded section without reading
everything before it. Random-access `PassiveCache` is the only fit.

### 9. Active limits and `StalledTime` reaping

Three admission checks bound the segment, and all of them only refuse **new**
entries:

- `CACHE_ACTIVE_MAX_ENTRIES` (default 64) bounds live keys.
- `CACHE_ACTIVE_CAPACITY` (default `CACHE_MAX_SIZE`) is the byte budget. A known
  size is reserved at `Init`; an unknown-size entry is admitted alone and grows
  under a hard cap, because there is no reservation to make up front.
- `CACHE_ACTIVE_IDLE_TIMEOUT` (default 10 min) bounds how long an entry with no
  readers stays. The reaper polls at a quarter of the timeout (clamped to
  10 ms–1 s) and drops an entry whose `StalledTime` — no downstream, upstream or
  finalization activity — exceeds it. A published entry is already out of the
  map; a failed one leaves when its last reference does.

An explicit `0` for `CACHE_ACTIVE_MAX_ENTRIES` or `CACHE_ACTIVE_CAPACITY`
disables the active segment while leaving the passive store enabled. Zero is
never a default: the segment is on whenever `CACHE_DIR` is set and neither limit
says otherwise.

An entry admitted before capacity or disk pressure appeared is never truncated,
failed or evicted mid-write; its readers finish normally.

### 10. One disk watermark for every local write

`CACHE_DISK_WATER_MARK` (`cache.disk_water_mark`, default `0` = disabled) is the
minimum free space on the filesystem holding `CACHE_DIR`. It gates every local
write: `hashstore.Put`, `hashstore.Adopt` and active admission all check it before
creating a temp file, and a large `Put` re-probes mid-write (every 16 MiB or 1 s)
so a filling disk is noticed. Below the watermark an immutable request takes the
ordinary upstream path and is delivered uncached; reads, deletions, reconcile and
prune are never gated, because the explicit capacity limits already bound the
store and a measurement problem must not become an outage. The guard applies to
local temp files even when bodies live in a shared bucket, since both backends
stage through `<CACHE_DIR>/.tmp`.

### 11. Local and shared-storage constraints

The adopt path (`Adopt` and `AdoptTrusted` alike) only accepts a source inside
`<CACHE_DIR>/.tmp`, so the active manager
must be opened with the same directory as the hash store. A local backend renames
the adopted file into the content tree (the source path disappears on success); a
shared backend uploads it and leaves the file for the caller to remove. Readers
never re-read a shared object: delivery uses the descriptor opened before
publication. `hashstore.Store` reports `CACHE_MAX_SIZE` through `Stats().Capacity`,
which is how the manager refuses a body it could never adopt.

### 12. A restart restores nothing

The active map, the fragment files and the 60 s publish wait are process-local.
The hash store's startup cleanup removes everything left in `<CACHE_DIR>/.tmp`, so
an interrupted assembly is deleted rather than resumed, and the next request
fetches it again. Complete hash objects are unaffected and remain discoverable
through the store, exactly as ADR 0011 requires for cache metadata.

## Deployment order

The relay must be upgraded **before** the Worker publishes `rules.immutable`. An
older strict relay treats an unknown `rules` key as an invalid manifest and
discards the whole snapshot, so publishing first would leave every relay with no
policy at all (ADR 0015 records the same constraint from the field's side). The
cut-over direction is relay first, Worker second. Once deployed, a relay without
`CACHE_DIR` or with the active segment disabled still serves immutably matched
paths through the ordinary cache chain.

## Consequences

- One upstream fetch per immutable object at a time, shared by concurrent readers
  and by readers of different ranges. A cold `Range` no longer pays for the whole
  body before serving.
- Content-addressed bodies are stored once under their real digest, even when
  several URLs or mounts name them; the active segment adds no second durable
  store and no new eviction policy.
- A digest captured by policy is verified, not trusted: a mismatched upstream
  response is delivered once and never published under the wrong name.
- Fragment assembly costs one extra local file per active object and holds a
  published body's bytes until its last reader leaves, which is why the segment
  needs its own entry, byte and idle limits beside `CACHE_MAX_SIZE`.
- Non-immutable traffic is entirely unaffected, so a bad setting or an unusable
  directory only disables the optimisation: startup logs it and keeps serving.
- A restart loses partial work. That is acceptable because a fragment is not an
  upstream fact, and a resumed fragment would need its own freshness reasoning.

## Alternatives considered

- *Keep `streamAndStore` and add singleflight around the MISS path.* Rejected: it
  coalesces identical full-body requests only. Two readers wanting different byte
  ranges of one layer would still each fetch the whole object, and a cold `Range`
  would still need a complete transfer before serving.
- *Use `SequentialCache` from the same module.* Rejected: see §8 — every reader
  would wait for the prefix, and multipart or resumed ranges cannot be served.
- *Publish fragments into the hash store as they fill, addressing partial bodies.*
  Rejected: a hash object means complete bytes; a partially filled body under a
  content digest would make the digest a lie and would need its own invalidation.
- *Copy the fragment into the store instead of adopting it in place.* Rejected:
  it doubles local I/O and transient disk use for every published object, and the
  handoff hook exists precisely to avoid that. It remains the fallback when handoff
  is unavailable.
- *Wave a low watermark off for an already-admitted entry.* Rejected: aborting an
  admitted assembly leaves a hole the tiered core believes is cached. The
  watermark refuses new work and keeps reads and deletes working; already-admitted
  writers finish.
- *Persist the active index so a restart resumes fragments.* Rejected: it makes a
  temporary artifact durable and forces freshness reasoning about bytes nobody has
  asked for yet. The store already has the complete objects; the rest is
  re-fetchable.
