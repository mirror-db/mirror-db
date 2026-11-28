# ADR 0019 — A trusted caller may publish precomputed digests

- **Status:** accepted (2026-11-28, implemented — not deployment-verified); adds a
  trusted publish primitive to the hash store and uses the PrefixSinks of
  `go-tiered-cache` from [ADR 0016](0016-tiered-active-cache-for-immutable-objects.md)
  without changing its assembly, admission or delivery rules
- **Date:** 2026-11-28

## Context

[ADR 0016](0016-tiered-active-cache-for-immutable-objects.md) publishes a
completed assembly through `hashstore.Adopt`, which treats neither the path nor
the caller's size as trusted: it validates that the source is a completed regular
file inside `<CACHE_DIR>/.tmp` at the declared size, then **reads the whole file
once more** to compute both SHA-1 and SHA-256. [ADR 0017](0017-response-declared-immutability.md)
reuses the same machinery for a response-declared immutable object.

That read exists because `Adopt` is a general entry point: any component could
hand it a file. For the active segment it is redundant work. The tiered core has
already streamed those exact bytes through the cache, in order and exactly once,
before it starts the store handoff — so the relay can hash them while they are in
flight for free and publish the assembly with no second pass over the object.

At `CACHE_MAX_SIZE` (512 MiB by default) the second pass is a full
`read(2)`/page-cache walk plus two hashes per published immutable object. For a
local backend, `Adopt` renames the source into the data tree, so the verification
read is the **only** full traversal on the publish path; for a shared bucket the
upload must still stream the bytes, but the local pre-read and hash are avoidable.

The two contracts also differ in what they may do on a mismatch. `Adopt` must
reject a body whose real digest contradicts the caller's expectation because it
can always recompute it. A caller that computed the digest itself is the only
party that can pass it on without a read, and it cannot be verified again for
free. Hiding that difference behind a boolean parameter on `Adopt` would make the
security-relevant choice invisible at the call site, which is why the trust
boundary is a separate method.

## Decision

### 1. A separate trusted method, not a flag

`hashstore.Store` gains:

```go
func (s *Store) AdoptTrusted(ctx context.Context, path string, c Content,
    expectAlgorithm Algorithm, expectDigest string) (Content, error)
```

`Content` carries `SHA1`, `SHA256` and `Size`. The method:

- validates and normalises both digests (`NormalizeDigest`: hex alphabet and the
  algorithm's fixed length), so an empty, short, long or non-hex digest is
  `ErrInvalidDigest`;
- requires `Size >= 0` and `Size <= CACHE_MAX_SIZE` (`ErrInvalidAdopt` for a
  negative size, `ErrOversized` above the budget);
- re-checks the context and the disk watermark **before** any side effect;
- still requires the source to be a completed regular file, not a symlink,
  strictly inside the store's temp directory at exactly `Size` (`adoptSource`);
  this is size and path validation, not content verification;
- compares the optional expectation against the caller's digest **before**
  `beginPublish` and before the backend put, so a mismatch is `ErrDigestMismatch`,
  publishes nothing and leaves an existing body untouched;
- then runs the **same** publish tail as `Adopt`: the publish window, the backend
  put, `testAfterPublish`, the index commit, LRU accounting, and the shared
  rollback. The tail is one helper (`publishAdopted`), not a second copy of the
  semantics.

The doc comment states the contract in the code: a wrong `Content` is committed
under a false address, poisons the local index and a shared bucket, and is then
treated as a real body by reconcile, LRU and prune. Only a caller that
established both digests of exactly these bytes may use it.

### 2. `Adopt` keeps its full verification

`Adopt` is unchanged: any caller, any source, one complete read, both digests
computed, expectation compared after the read. Existing callers and tests keep
their exact error ordering and side-effect guarantees. The two methods share only
the publish tail, so a future change to publication cannot silently diverge.

### 3. The active segment hashes while it assembles

`activecache.Entry.InitFactory` creates one pair of sinks per entry:
`digest.NewSHA1()` / `digest.NewSHA256()` from
`go-tiered-cache/adapters/digest` (lazy hashes), each wrapped by a
`digestSink` that also counts the bytes it accepted. The pair is passed as
`tiered.Config.PrefixSinks` and owned by the entry's `entryStore`; the manager
does not hold digest state, so an entry's hashes die with the entry.

The `PrefixSinks` contract supplies the completeness the publish needs: every
accepted prefix byte is delivered exactly once and in order, and the core starts
the store handoff only when the cache covers the whole extent. Direct handoff,
the copy fallback, an unknown-size object discovered at EOF and out-of-order
range assembly all feed the same sinks before that point, so the digest covers
the whole body rather than a prefix.

### 4. An O(1) completeness gate before trusting the digest

`entryStore.publish` uses `AdoptTrusted` only when **both** sinks' byte counts
equal the published size. Counts are the cheap defence against a digest that
describes a prefix: a short count means the cache never held the complete object
and a longer one cannot describe it. Either case falls back to `Adopt`, which
rereads and recomputes, so a partial prefix can never be committed as the
object's digest. The check is constant time and needs no extra buffer. It proves
**completeness only** — see the dependency premise below.

Publication also needs the adopter to support the trusted capability. The
interface is consumer-side (`activecache.trustedAdopter`, an optional assertion
on `adopter`): the real `*hashstore.Store` satisfies it, and a test fake or any
other adopter that only has `Adopt` keeps the old path automatically. Nothing in
the manager configuration selects it.

`SetExpectation` and the policy/response digest semantics are untouched: the
expectation is forwarded to `AdoptTrusted`, which compares it before publishing,
so a wrong policy or response digest still publishes nothing.

### 5. Dependency premise: sink advancement and coverage registration are one step

The gate in §4 counts bytes the sinks were fed; it does not, and cannot,
establish that those exact bytes are the ones the cache registered and will
publish. That tie comes from the tiered core's prefix contract and must hold
**per fragment**: advancing a `PrefixSink` and registering the same fragment in
the cache coverage must be consistent, so a fragment is never hashed and then
retried or replaced with different bytes while the count still totals the size.

`go-tiered-cache v0.0.2` writes the cache fragments, feeds the sinks, then
re-checks `u.destroyed` (and `closed`/`terminalErr`/`useStore`) before adding the
fragment to `cached`. A cancellation or termination window therefore exists in
theory in which an already-hashed interval is not registered, can be dropped
from the cache, and can later be re-fetched with different bytes. Both sink
counts would still equal the size, so the byte-count gate would accept the
digest. The active segment's `immutable` contract states that the bytes under an
interval cannot change, so this ADR accepts the premise: a conforming upstream
returns the same bytes for the same interval, and the count gate is a
completeness check on top of that fact.

Defending against a violating upstream — one that returns different bytes for the
same interval — needs the upstream library to make sink advancement and coverage
registration atomic for a fragment, or a content verification the relay does not
have. It cannot be done with a stronger byte count. The relay must not describe
the current gate as proving that the stored bytes equal the hashed bytes; it
proves the sinks saw a complete object, assuming the prefix contract.

### 6. What is saved and what remains

Saved on every trusted publication:

- one complete local read of the assembled body;
- one SHA-1 and one SHA-256 pass over it.

Still paid, deliberately:

- the body is still staged in `<CACHE_DIR>/.tmp` and still validated by `stat`
  (regular file, exact size, inside the temp dir);
- a shared backend still streams the bytes to the object store and still writes
  both digest pointers;
- no new fsync or durability contract is added: the file is not re-read and not
  fsynced beyond what the tiered scratch file and backend already do, so a
  precomputed digest rests on the same local staging assumptions as before;
- the sinks hash every accepted byte, so the CPU cost moves from a second pass to
  the assembly pass; there is no extra copy.

A sink write error terminates the tiered cache through its existing terminal
path, so the entry never reaches publication and the ordinary upstream fallback
applies. The active path's admission, delivery and retire order are unchanged;
publication and the URL variant are finished by a detached per-entry waiter
(bounded at 60 s) after the handler flushes and returns, so the client's EOF
never waits for the store (see ADR 0016 §7).

## Consequences

- An immutable assembly publishes with one fewer traversal: the digest is
  computed while the bytes are already moving through the cache.
- The trust boundary is visible: `Adopt` still verifies every byte for untrusted
  callers; only `AdoptTrusted` skips it, and its name, signature and comment say
  so.
- A wrong trusted digest is only possible if the caller lies about bytes it
  claims to have hashed. That is a caller bug, not an upstream one; the Worker
  and server paths never construct `AdoptTrusted` directly.
- An adopter without the capability, a byte-count mismatch, a partial assembly or
  a failing sink all keep the previous behaviour — the digest is recomputed or
  nothing is published.
- The relay needs `go-tiered-cache v0.0.2` for `PrefixSinks` and
  `adapters/digest`. This is a build-time dependency, not a protocol change: no
  manifest field, header or Worker behaviour changes, so there is no deployment
  ordering constraint.

## Alternatives considered

- *Add a `trusted bool` parameter to `Adopt`.* Rejected: it turns a
  security-relevant decision into an argument at every call site, and the two
  contracts have different validation orders; a separate method keeps the
  untrusted path's guarantees intact and readable.
- *Trust the policy or response digest and skip hashing entirely.* Rejected: the
  assembler must still establish what it stored. A policy digest is an
  expectation the upstream may violate, and the response digest is only an
  expectation too; the sink digests are facts about the bytes in the cache.
- *Use the sink digest as the cache key.* Rejected: the key must be known before
  the body, and a URL-keyed entry's identity is the URL; the digest stays a
  post-assembly fact (ADR 0016 §2, ADR 0017 §3).
- *Publish as soon as the sinks are complete without a count check.* Rejected:
  the count is constant-time insurance against a future PrefixSinks contract
  change or a partially assembled entry, and its fallback is the existing safe
  path.
- *Remove the `Adopt` verification read entirely.* Rejected: it is the only
  defence for untrusted sources, including any future caller that stages a file
  in `.tmp`.
