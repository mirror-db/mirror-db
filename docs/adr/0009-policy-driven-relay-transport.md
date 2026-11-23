# ADR 0009 — Distribute relay traffic policies with the mirror manifest

- **Status:** accepted (2026-11-17) — the first-match evaluation rule is
  superseded by [ADR 0012](0012-policy-field-merge-and-passthrough.md); the
  distribution, matching, manifest ETag and transport decisions stand
- **Date:** 2026-11-17

## Context

A relay maintains separate Cloudflare edge pools for latency and sustained
transfer speed, but every request historically used the latency pool. The right
choice depends on the resource: an APT index or OCI manifest benefits from a
prompt first byte, while a package, blob, or disk image benefits from throughput.
The Worker already knows the available mirrors; the relay already synchronises
that manifest. Requiring every relay operator to duplicate resource-routing rules
would let those rules drift from the service they describe.

A full manifest download on every refresh is also unnecessary. Manifest changes
are rare compared with the ten-minute refresh cadence.

## Decision

The mirror manifest distributes an ordered policy list alongside its mirrors.
Each mirror advertises opaque `policy_tags`; tags select policies and do not
reintroduce a mirror-kind taxonomy or describe capabilities.

A policy has a unique name, a description, matchers, and rules. Matchers may
select a tag, an exact mirror name, or a mount-relative request path by prefix,
suffix, or RE2-compatible regular expression. All present matchers are ANDed.
Paths begin with `/`, exclude the query string, and retain their original case
and escaping. For a path-mounted mirror, the mount prefix is removed before
matching; for a subdomain-mounted mirror, the URL path is used unchanged.

Policies are evaluated in array order and the first complete match wins. Rules
are not merged. The only rule in this version is `traffic_policy`, with values
`latency` and `speed`; an unmatched request uses `latency`. The initial policies
select OCI blobs, APT `pool/` payloads, and common archive, image, installer, and
package suffixes for speed. The generic suffix rule applies only after a request
has resolved to a mirror, never to control-plane endpoints. A final empty-match
policy makes the latency default explicit.

Traffic policy is a preference, not an availability requirement. An unavailable
speed pool falls back to latency. Explicit upstream proxy and SSH-disguise
configuration continues to bypass edge selection, and an in-flight response never
changes transport because policy configuration refreshed.

The API emits a deterministic strong ETag for the manifest and supports
conditional GET and HEAD. A relay initially fetches the full manifest with GET.
Subsequent checks use HEAD with `If-None-Match`; an unchanged response avoids a
body download, while a changed or missing ETag triggers a full GET. Mirrors and
policies are validated and replaced atomically. A failed refresh or invalid
update leaves the last known good configuration active; before the first
successful load, the relay retains its existing short retry and 502 behaviour.

The refresh interval is `relay.manifest_sync_interval`, defaulting to ten
minutes, with the existing jitter. It must be positive so policy distribution
cannot be disabled accidentally.

Old relays ignore the added manifest fields. New relays accept an absent policy
list from an old API and retain the latency default.

## Consequences

- The Worker is the single source of truth for resource-routing policy, and all
  relays converge without local rule duplication.
- A policy update takes one refresh interval plus jitter to reach a relay.
- Unchanged refreshes normally transfer headers only; a changed ETag costs one
  HEAD followed by one GET.
- Policy order is part of the public contract. Broad fallback rules must follow
  specific rules, and new matchers or rule kinds require explicit compatibility
  handling.
- Regular expressions are restricted to Go's RE2 syntax so validation and request
  matching cannot introduce backtracking-based denial of service.
