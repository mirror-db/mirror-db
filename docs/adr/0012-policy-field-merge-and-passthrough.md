# ADR 0012 — Policy rules merge field by field, and passthrough is declared before the request

- **Status:** accepted (2026-11-23, implemented — not deployment-verified; supersedes the first-match evaluation rule of ADR 0009)
- **Date:** 2026-11-23

## Context

ADR 0009 made the first complete match win, with rules explicitly not merged.
With a single rule field that was simple enough. The manifest now distributes
more than a transport preference: a path can also be declared passthrough, and
the relay needs to know that before it asks upstream. Under first-match a later
rule cannot amend an earlier one, every rule must restate every field, a broad
fallback silently discards specific intent, and expressing "APT pool → speed,
but this subtree is untouched" requires duplicating matchers in both rules.

The manifest is unversioned, fetched atomically, and consumed by relays built
from the same project. There is no version negotiation to lean on.

## Decision

Policies keep array order. Every matching rule merges its present fields into the
accumulated result; a later match overrides an earlier one, and an absent field
inherits. The merge is top-level and per field, never recursive. An explicit
`false` overrides; `null` is a configuration error, not "unset".

The rule fields are `traffic_policy` (`latency` or `speed`), `passthrough`
(boolean, default `false`), and `exit` (evaluation control, never part of the
result). Defaults are latency and no passthrough. `exit` stops evaluation after
its rule has merged, so a rule that must not be amended later can say so. A
request that resolves to no mirror matches no policy.

`passthrough: true` forwards the matched request without unpacking the response:
no cache read or write, no response-control parsing, no transformation, and the
response is delivered as received. It is knowable before the request is made,
which is why it is a policy field; everything only the response can state stays a
relay directive (ADR 0011).

Validation is all-or-nothing. An unknown `traffic_policy`, a non-boolean
`passthrough` or `exit`, `null`, or an invalid pattern rejects the whole
manifest and the relay keeps its last known good snapshot. Control-plane
endpoints and `/relaytest` are bypassed before matching, so an empty-match rule
cannot pull them back in.

The manifest carries only current semantics. This supersedes ADR 0009's
"first complete match wins; rules are not merged"; its distribution, matching,
ETag, atomic replacement, retry behaviour, and transport fallback all stand.
The change is deployed directly, with no version negotiation and no
compatibility path. The earlier draft's `cache` and `cache_ttl` fields are
removed in the same upgrade, without a shim.

## Consequences

- Order still matters, but a broad rule can now amend a specific one, so authors
  reason about accumulation rather than shadowing. Isolation requires `exit`.
- Adding a pre-request field no longer multiplies rules: a rule flips one field
  and inherits the rest.
- A path-level passthrough does not need its own copy of the transport rule, and
  a transport rule does not need to know which subtrees are passthrough.
- A relay built from the earlier draft expects `cache` / `cache_ttl`, sees
  neither, and caches nothing, while a new relay caches by default. That
  divergence is accepted because no relay implementing the draft was released;
  the manifest is unversioned and both sides deploy from the same source.
- Removing a field is as breaking as changing a semantic, because the manifest
  has no version. Future changes of this size need the same direct cut-over.

## Alternatives considered

- *Keep first-match and duplicate fields in every rule.* Rejected: each new
  pre-request field multiplies duplication, and composition becomes accidental
  rather than declared.
- *Deep-merge nested structures.* Rejected: rules are flat; recursive merging
  would make an omitted nested value ambiguous and invite pattern-level
  surprises.
- *Version the manifest.* Rejected: manifests and relays ship from one project
  and deploy together; a version field would add a compatibility matrix for a
  transitional window that does not exist.
- *Put passthrough on the mirror instead of a policy field.* Rejected: it is a
  per-path decision, not a per-mount one, and it would need a second distribution
  channel beside the policy list.
