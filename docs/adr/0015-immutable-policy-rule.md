# ADR 0015 — `rules.immutable` declares a content-addressed path before the request

- **Status:** accepted (2026-11-26; Worker distribution and relay consumption
  implemented, not deployment-verified)
- **Date:** 2026-11-26

## Context

The relay already serves content-addressed bytes from its local store without
revalidation in one place: an OCI URL whose path digest equals a variant's body
SHA-256 (ADR 0011's stale-digest exception). The same fact exists for other
mounts — for example APT `by-hash/` objects — but at the time the relay could
only learn it by storing the response first and comparing digests afterwards.
That cost a miss on the first request and could not help a relay that had the
body under its real digest but no URL variant.

The information is knowable *before* the request: the path often names the hash.
Policy is where pre-request facts live (ADR 0012), and a string is enough — no
new transport value, no boolean, no response-side freshness.

## Decision

Add an optional string `rules.immutable` to the manifest policy schema.

- It is an RE2-compatible regular expression matched against the **mount-relative
  path**, exactly like `path_pattern`.
- It may expose a named group `sha256` or `sha1` — syntax `(?<sha256>…)` /
  `(?<sha1>…)`, supported by both RE2 and JS `RegExp`. When present, the captured
  hex is the object's content digest; a pattern without such a group only
  declares that the path's bytes are immutable.
- It participates in the existing top-level field merge: a later matching rule
  may restate or omit it, and omission inherits. It is not a boolean.
- It never changes `traffic_policy`, `passthrough` or `exit`, and it is not
  freshness. It states that the path addresses immutable bytes, plus a digest
  when the expression names one.
- Validation stays all-or-nothing: an invalid expression, a non-string value or
  `null` rejects the whole manifest. A pattern without a digest group is valid.

The Worker distributes two initial patterns, placed before the transport rules so
the broad rules still merge afterwards:

| name | match | example |
|------|-------|---------|
| `oci-digests` | `tag: oci`, `^/v2/.+/(?:blobs\|manifests)/sha256:(?<sha256>[0-9a-f]{64})$` | digest blob, digest manifest |
| `apt-by-hash` | `tag: apt`, `^/dists/.+/by-hash/(?:SHA256/(?<sha256>[0-9a-f]{64})\|SHA1/(?<sha1>[0-9a-f]{40}))$` | APT by-hash object |

A digest blob matches `oci-digests` and then `oci-blobs`, so it inherits `speed`
from the existing rule; a digest manifest matches only `oci-digests` and keeps
the default `latency`. Tag manifests, non-digest uploads, APT `Release` /
`InRelease` and undeclared `by-hash` algorithms do not match.

## Deployment order

At the time this field was introduced the relay did not consume `immutable`, and
an older strict relay rejects a manifest carrying it: an unknown key in `rules` is
a validation error, so such a relay keeps its last known good snapshot — or, on a
cold start, serves no policy at all. The field must therefore be published only
after a relay that accepts it is deployed. The Worker manifest and the relay ship
from the same project, but the cut-over direction is relay first, Worker second,
unlike ADR 0012's simultaneous direct replacement.

That order has since been honoured in code: the relay now consumes the field
([ADR 0016](0016-tiered-active-cache-for-immutable-objects.md)), and the
constraint becomes a release rule again whenever an older relay may still be
running — upgrade the relay before publishing a manifest that uses a newer key.

## Consequences

- A relay can reuse a stored body by predicted digest without a URL variant,
  when the pattern names a digest group; it validates that capture and checks
  size/digest before serving.
- The captured digest is also a **store expectation**, not only an active-path
  one. When the active segment is unavailable or refuses an assembly, the
  ordinary tee hashes the body it is already streaming and publishes nothing if
  the bytes contradict the path's digest (`hashstore.PutExpected`). The response
  is still delivered whole; it simply never becomes a URL variant or a hash
  object under a false address, so the next request is a fresh MISS rather than
  a hit on bytes the path did not name. A normal prefetch refresh applies the
  same expectation and keeps the last good variant on a mismatch. A response
  with a non-identity coding is a negotiated representation, not the object the
  digest names: on such a path it is delivered (or discarded by a refresh) and
  never stored at all, while paths without a captured digest keep their coding
  behaviour.
- The relay merges the pattern like every other rule field, evaluates the
  surviving expression once against the mount-relative path, and treats a match
  as "these bytes cannot change" plus the captured digest when there is one. A
  later rule that declares a different pattern does not fall back to an earlier
  match's pattern; a declared pattern that does not match declares nothing.
- The manifest gains a field older strict consumers reject. The deployment order
  above is the standing mitigation: a relay that predates the field must be
  upgraded before a manifest using it is published.
- `immutable` is not a cache directive. Whether to cache remains `CACHE_DIR` plus
  the response (ADR 0011); the field only says the bytes cannot change. What the
  relay does with a match — a local digest hit, a stale-variant rescue, or an
  in-process assembly — is described in
  [ADR 0016](0016-tiered-active-cache-for-immutable-objects.md). A path whose
  immutability only the upstream knows is expressed by the `immutable` relay
  directive instead, which never changes this field or the manifest
  ([ADR 0017](0017-response-declared-immutability.md)).
- The Worker owns the initial patterns, so adding a mount with a new digest path
  shape needs a manifest change, not a relay release.

## Alternatives considered

- *A boolean `immutable: true`.* Rejected: it states the fact but not where the
  digest is, so the relay would have to re-parse each mount's URL grammar.
- *Derive the digest in the relay from `path_pattern` captures.* Rejected: it
  overloads `match` with result semantics and requires a naming convention the
  schema does not express; a dedicated field keeps matching and extraction
  separate.
- *Response-side detection only.* Rejected: the first request still misses, and a
  relay holding the body by digest cannot use it. Response-side detection was
  later added as a complement, not a replacement: the `immutable` relay directive
  seeds a first MISS but cannot deduplicate before the headers arrive
  ([ADR 0017](0017-response-declared-immutability.md)).
- *A separate policy kind or a new transport policy value.* Rejected:
  `immutable` is orthogonal to transport; a digest manifest should stay on
  latency while a digest blob uses speed.
