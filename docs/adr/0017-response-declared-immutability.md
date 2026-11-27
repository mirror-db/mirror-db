# ADR 0017 — A response may declare its own immutability

- **Status:** accepted (2026-11-27, implemented — not deployment-verified);
  extends the `X-MDB-Relay` directives of [ADR 0011](0011-response-controlled-request-cache.md)
  and the active segment of [ADR 0016](0016-tiered-active-cache-for-immutable-objects.md)
  without superseding either
- **Date:** 2026-11-27

## Context

`rules.immutable` ([ADR 0015](0015-immutable-policy-rule.md)) states before the
request that the bytes under a path cannot change. It covers the paths the Worker
can describe from their shape — an OCI digest, an APT `by-hash/` object — but
not a path whose immutability only the upstream knows: a content-addressed file
tree, a version-pinned download, a release artifact whose name does not embed its
digest. For those, a relay learns the fact from the response, and with the
current directive set only after it has stored the body.

The first request for such a path therefore pays a full MISS with no assembly:
no coalescing, no ranged sharing, no digest-addressed reuse. The fact is already
in the response the relay is about to deliver; the missing piece is a way for the
response to say it.

Two candidate shapes were considered and rejected: a `hash_path` policy field
capturing the digest from the request URL (it duplicates what `rules.immutable`
already expresses and needs a second matching language), and a request-side
content hash computed by the relay (a new digest algorithm alongside SHA-1 and
SHA-256, with no upstream agreement on how a client would present it).

## Decision

### 1. Three response keywords, no new policy field

`X-MDB-Relay` gains:

| keyword | meaning |
|---|---|
| `immutable` | the bytes of **this response** cannot change |
| `sha256=<64 hex>` | the response body's SHA-256 |
| `sha1=<40 hex>` | the response body's SHA-1 |

Parsing rules:

- A boolean keyword is sticky, like the existing ones.
- A digest is accepted only when it is hexadecimal and exactly the length its
  algorithm requires; anything else is ignored as if absent.
- Two **valid but different** values for the same algorithm cancel that
  algorithm: presenting a body under either would be a guess. A repeat of the
  same value is harmless, and an invalid value never cancels a valid one.
- Hex is normalised to lowercase.
- When both algorithms survive, SHA-256 is the expectation; SHA-1 is recorded and
  otherwise unused.
- **A digest does not imply `immutable`.** Only the explicit keyword selects the
  active segment. A response that names a digest without declaring immutability
  is still a mutable URL.

The response digests are an Adopt expectation, never a cache key and never a
path capture. `rules.immutable` keeps its string expression and its optional
`sha1`/`sha256` named groups; no `hash_path` field exists (see the alternatives).

### 2. A first MISS may seed the active segment

`deliverUpstream` parses the response headers before writing anything to the
client. When the response declared `immutable` and the request qualifies, the
already-fetched response is handed to the existing active path — the same
`InitFactory`/adapter/`Adopt` machinery ADR 0016 built for policy-seeded
requests. Nothing about the assembly itself changes.

Admission is deliberately narrower than the policy path, because the fact arrived
late. All of these must hold:

- method `GET` (a `HEAD` may never create an entry);
- a `200` without `Content-Range`, or a `206` with a concrete total size whose
  `Content-Length`, when stated, equals the interval length;
- the response is storable by the ordinary rules and is `identity`-encoded, with
  no dangerous `Vary`, no `no-store`/`private`, and no `Content-Range` on a
  `200`;
- a concrete size greater than zero and no larger than the store's per-body cap;
- the request is not a client `no-store` or forced revalidation, carries no
  conditional header, accepts `identity`, and its `Range` — if any — is not
  multipart;
- no old variant was bypassed and the request is not an ambiguity cleanup;
- no policy already matched `immutable` for this request.

A ranged request that the origin answered with a complete `200` takes the
ordinary writable path: the full body is stored as an ordinary variant, which is
exactly what the response is.

A response that is admitted but whose persisted variant is written later keeps
two header sets: the one delivered for this response (upstream `Cache-Control`
untouched) and the one stored (persistent lifetime).

### 3. The key is the URL; the digest is only an expectation

The response-only entry is acquired with the request's ordinary cache key
(`mirror://<mirror><mount-relative path>[?query]`). The response digest never
changes that key, so the entry only deduplicates requests for the same URL. That
is why no URL hint or pre-header coalescing is introduced: nothing can know the
object identity before the response arrives.

`activecache.Entry.SetExpectation` is the safety seam. A creator may set the
expected algorithm and digest before `Init`; joining readers cannot override it;
a digest already derived from a digest-shaped key accepts only the same value.
`Adopt` still recomputes both digests from the file, so a mismatch publishes
nothing. The client's body is unaffected — it streams from the scratch cache —
but no hash object and no URL variant is written, and the next request is a
fresh MISS.

When a policy already declared immutability, its pre-request key and captured
digest are used unchanged: a policy digest always wins over a response digest.
A URL-keyed policy path (pattern without a digest group) may use the response
digest as its expectation, because that changes no key.

### 4. Failure never costs a second upstream request

Every refusal before the adapter takes the response body leaves the response
untouched, and the caller delivers it through the ordinary chain — same bytes,
one upstream request. That covers capacity, the disk watermark, an unknown or
zero size, an over-budget body, a non-identity coding, `no-store`/`private`, a
dangerous `Vary`, a `206` whose `Content-Length` disagrees with its
`Content-Range`, and the extra conditions above. A fallback delivery never
re-enters the response-side path, so a refusal cannot recurse.

The `206` length check has to happen before the adapter: constructing the
adapter closes the response body when it rejects a mismatched `Content-Range`,
which would leave nothing to deliver. Eligibility performs the same comparison,
so the malformed response is served by the ordinary path instead of turning into
a `502`.

The one failure that cannot be recovered this way is an adapter construction
error after the response was handed over; it answers `502`, since the body is no
longer deliverable. Nothing in the admission list above can reach it: every
condition the adapter checks is also checked before the handoff.

A request that finds an entry already assembling the same URL does not join it:
it already holds a fetched response, so it delivers that response through the
ordinary chain and lets the storage rules decide. A `206` seed therefore
publishes only when the range it answered covers the whole object; a partial seed
serves its own reader, is left to the idle timeout, and a later full request is
stored ordinarily. Coalescing before response headers arrive was never a goal,
so two concurrent cold requests may each fetch once.

### 5. A directive-declared immutable variant is persistent

A response that declares `immutable` stores a persistent variant whether the
active segment took it or the ordinary tee did. Its **stored** header set has its
lifetime rewritten to a constant **365 days** and its `Expires` dropped, so it
does not come back stale on the very next request because the upstream stated a
short lifetime or none; orthogonal `Cache-Control` directives such as
`no-transform` survive. The header set delivered for the first response is never
rewritten: the client sees the upstream `Cache-Control` on both the active and
the ordinary path.

The persistence is also **variant metadata**, a `Persistent` flag on the stored
record, not a property inferred from the header text. A later `304` folds new
headers and freshness as usual, but a persistent variant keeps its 365-day
lifetime even when the `304` states `max-age=0` and does not repeat
`X-MDB-Relay`; only a `304` that newly declares `no-store` or `private` still
invalidates it. An ordinary (non-persistent) variant keeps the old `304` rule.
The flag is a spare bit of the existing variant flag byte, so records written
before it existed decode with the bit clear — no stored row is rewritten.

Prefetch uses the same lifetime as its **schedule** for such a response: a
`max-age=0` immutable response is refreshed at the persistent lifetime instead of
being silently unscheduled or polled in a short cycle. It changes only when the
refresh is due; `no-store` still disables background writes and still refuses the
response (ADR 0014 is unchanged).

This is a statement about the response, not an interpretation of the standard
`Cache-Control: immutable` header, which the relay still never reads. `no-store`
and `private` still refuse storage, and a client's `no-cache` / `max-age=0`
still forces revalidation before a fresh variant is served.

### 6. A diagnostic switch, off by default

`CACHE_LOG_IMMUTABLE_MISS` (`cache.log_immutable_miss`, default `false`) logs a
request that resolved to a mirror, whose **merged pre-request policy did not
match `immutable`**, and whose mount-relative path contains a digest-shaped run.
It exists to find a missing manifest rule, not to describe cache behaviour, so it
is not an active-fallback log and does not report every assembly.

The expression is the operator's, unmodified:

```
[^0-9a-zA-Z][0-9a-fA-F]{40}([0-9a-fA-F]{24})?[^0-9a-zA-Z]
```

No lookaround or boundary was added. Consequences, documented and tested:

- a digest at the very start or end of the path is not reported (no surrounding
  non-alphanumeric rune);
- a 41..63 character hex run fits neither 40 nor 64 and is not reported, and a
  run longer than 64 characters yields no candidate: the expression must start on
  a non-alphanumeric character, so it cannot begin inside the run and neither 40
  nor 64 fits before the run ends;
- escaped paths are matched as escaped: a delimiter written `%2F` ends in a
  hexadecimal letter and hides the run;
- two digests sharing one delimiter yield one non-overlapping match, so the
  second is not reported.

The switch is read once at startup, so it needs a restart. While it is off, no
path is scanned. When it is on, each matching request is logged with its mirror,
mount-relative path, HTTP method and the extracted candidates; the query string,
request headers and client address are never logged. `passthrough` requests are
covered because logging happens before the cache chain, while control endpoints
never reach its handler and an unresolved mirror has no policy to miss.

## Deployment order

The relay consumes the new keywords, and an older relay ignores unknown keywords,
so a Worker may publish them at any time: no manifest changes. The diagnostics
switch is relay-local. ADR 0015's ordering constraint applies only to new
**policy** fields, which this ADR does not add.

## Consequences

- A path whose immutability only the upstream knows now gets the active segment
  from its first response: one assembly, ranged sharing, and a digest-addressed
  body under its real digest.
- A wrong response digest can suppress publication but never corrupts or blocks
  the client body. Policy remains the authoritative pre-request fact.
- The response-only key is per URL, so it does not deduplicate the same bytes
  across URLs. Digest-keyed sharing still exists for policy-declared paths.
- A response-declared immutable variant is cached for a year regardless of the
  upstream's stated lifetime, and keeps that property through a forced
  revalidation: a `304` stating `max-age=0` cannot silently shorten it. The
  first response still advertises the upstream's own `Cache-Control`, so only a
  later hit shows the persistent value.
- Prefetch schedules such a response at the same persistent lifetime.
- The variant record grows one flag bit in its existing flag byte. Older reader
  builds ignore the bit and older records decode with it clear, so the change
  needs no migration in either direction.
- One extra `X-MDB-Relay` token has no effect on a relay that predates it, so the
  change is forward- and backward-compatible.
- The diagnostic reports candidates, not verdicts: it cannot know whether a
  digest-shaped run is a content address, and its documented false negatives are
  part of the tool.

## Alternatives considered

- *A `hash_path` policy field capturing the digest from the URL.* Rejected: it
  duplicates `rules.immutable`'s matching and extraction with a second syntax,
  and it cannot express immutability that is not visible in the path.
- *Derive the digest in the relay and key by it.* Rejected: a relay-computed
  digest would be a third algorithm with no upstream contract, and computing it
  before serving would require buffering the object.
- *Let the digest imply immutability.* Rejected: a digest identifies one
  response's bytes; it does not promise that a later request for the same URL
  returns the same bytes. The two facts stay separate.
- *Also treat a standard `Cache-Control: immutable` response as active-path
  eligible.* Rejected: that header is a freshness hint for client caches, not a
  promise about this URL's future bytes, and interpreting it would silently widen
  the active segment to arbitrary responses.
- *Re-key a response-seeded entry by the declared digest.* Rejected: the same
  bytes under two URLs can carry different media types and headers, and the
  digest must never re-point a request key.
- *Buffer the first response until its digest is known.* Rejected: it delays the
  first byte for every response that might be immutable and does not help
  deduplication, since the key is the URL anyway.
- *Persist the response-only immutability in a manifest-like side channel.*
  Rejected: it is a response fact (ADR 0012); a relay-local learning cache would
  need its own expiry, eviction and distribution story for no benefit over
  storing the variant.
- *Reuse `rules.immutable`'s regex for the diagnostic instead of the operator's
  expression.* Rejected: the diagnostic is about discovering a missing rule, so
  it must use the finding expression the operator asked for, not a policy
  pattern that may be arbitrarily anchored.
