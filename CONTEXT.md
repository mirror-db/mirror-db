# mirror-db

Glossary of the project's domain language. Implementation details live in
`AGENTS.md`; this file only fixes what words mean.

## Terms

### Mirror

A public, no-login-facing view of one upstream distribution channel. A mirror is
identified by a **name**, and is reachable at exactly one **mount**: either a
subdomain (`dcr.mirs.uk`) or a path segment (`mirs.uk/debian/`). Clients reach a
mirror anonymously; whatever authentication the upstream demands is handled
inside the mirror and never surfaces to the client.

### Kind (mirror kind)

What a mirror is a view *of*, which decides how a request is served:

- **Registry** — an OCI distribution endpoint (pull manifests/blobs, resolve
  tokens). Routed by subdomain.
- **APT repository** — an upstream `dists/` + `pool/` tree that APT clients
  target via `sources.list`. Its mirror URLs are always HTTPS.
- **Package index** — an ecosystem-level index plus its fetch endpoints (npm
  metadata + tarballs, PyPI Simple + files).
- **Chart repository** — a Helm `index.yaml` plus the chart tarballs it points at.
- **File tree** — an upstream that is just browsable/downloadable files, split
  by whether the upstream serves a directory listing (**browsable**) or must be
  addressed by exact path (**opaque**).
- **Utility** — a non-renewable service: install scripts, signing keys,
  speed tests.

### Mount

The URL location a mirror occupies. The **mount name** is the mirror's name.
A path suffix may not reuse another switch's name (`keys`, `api`, `scripts`,
`speedtest`, `conf`).

### Source configuration

A deb822 description of an APT source, identifying its repository URI, suites,
components, optional architectures, and signing key. It is a client
configuration, not a mirror: several configurations may select different
repository roots below one mirror.

### Source preset

A named source-configuration default. A preset may have shortcuts for a
distribution release or repository layout. Shortcuts only supply defaults;
explicit client selections may override them.

### Mount switch

The first path segment on the bare domain that the router interprets before any
mirror (`keys/`, `api/`, `scripts/`, `speedtest/`). A mirror name colliding with
a switch is silently shadowed.

### Front door

The HTTP entry at the mirror's mount: request validation, upstream fetch,
header sanitising, and either a cache entry or a streaming pass-through. The
front door never returns upstream authentication challenges, cookies, or
hop-by-hop headers.

### Listing site

An upstream that answers a directory URL with an autoindex HTML page. A
browsing mirror over a listing site supports three access modes on the same
data: HTML, JSON (`?format=json`), and read-only WebDAV.

### Res resource

A chart tarball re-addressed under an opaque id in the rewritten Helm index, so
the tarball is fetched back through the mirror instead of from an arbitrary
upstream URL. Only resources present in a fetched index can be resolved.

### Relay

An optional reverse proxy in front of the Worker. It is *not* a mirror: it
carries no upstream knowledge, only transport and its own local cache. When
present, it tells the Worker which public host the client actually reached
(`X-MDB-Relay-Host`), and the Worker must emit absolute URLs on *that* host so
the client stays on the relay path.

### Relay host

The value of `X-MDB-Relay-Host` — the outermost relay's base domain as seen by
the end user. Untrusted input: anyone can set it, so it may only shape URLs
returned to the client, never grant access.

### Relay directive

A response header (`X-MDB-Relay`) by which an upstream tells the relay how to
treat that response. Its value has the shape of `Cache-Control`: comma-separated
`keyword` or `keyword=value`. Seven keywords are defined: `no-store` — do not
keep this response at all; `no-vary` — the URL path alone identifies the
resource, so `Vary` must not split it into variants, `Vary: *` included;
`ttl=<seconds>` — the lifetime to fall back on when the response states no
freshness, zero meaning none; `prefetch` — the mirror declares this resource
eligible for relay-driven refresh while the relay continues to see demand for
it; `immutable` — this response's bytes cannot change; and `sha256=<64 hex>` /
`sha1=<40 hex>` — the body's content digest. A digest without `immutable`
declares no immutability, and a digest names bytes, never a cache key. Unknown
keywords are ignored.

The header only describes a response. A relay consumes it and never forwards it
to the client, except on a passthrough path, where no response control is
applied at all. A request that did not arrive through a relay is not allowed to
influence anything with a request-side copy: the relay and the Worker remove
`X-MDB-Relay` from an inbound request that carries no relay-host header.

A directive travels with a response rather than being distributed as request
policy. It may control that response or declare eligibility for background
refresh. It never selects transport or passthrough: those remain policy fields
because the relay must know them before it makes the request. Where an upstream
forbids transformation with the standard `Cache-Control: no-transform`, the
relay honours that header as well.

### Response-declared immutability

A statement, carried by the `immutable` relay directive, that the bytes of one
response cannot change. It is a response fact, so it can only be acted on after
the headers arrive: the first request for such a path may already be assembled
and published, but no later request can be deduplicated before its response
headers exist. The address stays the cache key — a declared digest is an
expectation about the body, not an identity for the URL — and a stored variant of
such a response is kept for a persistent lifetime rather than the upstream's
stated freshness. That persistence is part of the stored metadata, so a later
revalidation cannot shorten it, while the first response still advertises what
the origin sent. It is distinct from a policy's immutability fact, which is
known before the request and may name the digest in the path.

### Response disposition

The relay's decision about one upstream response once its headers are known:
deliver it without storage, file it as a stored variant, or assemble it into a
hash object. One response has one disposition and one body, so a fallback — an
assembly that cannot proceed, a store that refuses the response — delivers that
same response instead of asking the upstream again.

### Immutable-miss diagnostic

An opt-in relay log line reporting a request that resolved to a mirror, whose
merged policy did not declare immutability, and whose mount-relative path
contains a digest-shaped run. It is a hint that a policy rule is missing, not a
cache event: it names no client, query or header, and it runs only while its
switch is on.

### Policy tag

An opaque label attached to a mirror in its manifest. Tags select relay policies;
they do not classify the mirror's kind, grant a capability, or change how the
mirror serves content.

### Relay policy

A named, ordered rule distributed with the mirror manifest. Its matchers select
requests by tag, mirror name, or mount-relative path; all present matchers must
agree. Matched rules merge field by field in order, so a later match overrides an
earlier one; `exit` ends evaluation after its rule has merged and is not itself
part of the result, and a request that matches nothing keeps the defaults.

A policy distributes only what the relay must know **before it asks the
upstream**: the traffic preference, whether the path is passed through
untouched, and whether the path's bytes are immutable. An immutability fact names
an expression over the mount-relative path and may capture the content digest
through a named group; it is a statement about the bytes, not about freshness or
transport. A decision that can only be made from the response — store it or not,
transform it or not, its actual age, coding or digest — is not a policy field; it
is a relay directive. Whether the relay caches at all is neither: it belongs to
the relay's own content store, not to a rule. Policy fields sit beside each other
at the top level of a rule and merge the same way; `match` is unchanged. The
manifest carries one current semantics: it is unversioned and older readers are
not accommodated.

### Immutable path coverage

The set of mount-relative paths whose bytes a policy declares immutable, and the
evidence for each. A path qualifies only from the request itself: either it names
a content digest, or the upstream's publication contract fixes its bytes. A
response header or a checksum inside a body can prove integrity of one transfer,
but never establishes that the address is immutable, so it is recorded as
research evidence and not turned into coverage. The audit of all mirrors and
their external-versus-mount-relative paths is maintained in
[relay-immutable-coverage.md](brainstorm/features/relay-immutable-coverage.md).

### Traffic policy

A relay's transport preference for one request: `latency` favours prompt response
headers, while `speed` favours sustained transfer throughput. It never changes
the requested content. If the preferred transport is unavailable, the relay
falls back rather than failing the request; a request that nothing selects a
preference for uses `latency`.

### Passthrough

A policy outcome in which the relay forwards a matched request without unpacking
the response: no cache read or write, no response control, no transformation,
and the response is delivered as received. It is decided before the request is
made, so it is a policy field rather than a relay directive.

### Prefetch lease

A relay-local, in-memory record showing that a recently requested resource is
eligible for background refresh. It is learned from a `prefetch` response
directive, renewed by subsequent requests, and abandoned after three days
without demand. It is never persisted or reconstructed by scanning an upstream.
A refresh is driven by the resource's freshness; `no-store` still forbids an
ordinary cached response even when a prefetch lease exists.

### Repository facts

The paths, content hashes, suites, validity period, coverage and lightweight
health summary of one APT repository generation. Coverage determines which
absences are authoritative and which paths remain unjudged. Proxy, listing and
status consumers all read the same generation; without a published generation,
its paths remain unjudged and its health is initializing.

### Metadata index

One coherent, Worker-parsed snapshot of an APT repository's metadata. It relates
metadata files and the package files they reference to their sizes and content
hashes, and supports both synthetic directory listings and relay hash
expectations. It is replaced only as a whole, so a listing and its hash
expectations always describe the same upstream generation, and it is
authoritative for the paths it covers only while fresh. Building or refreshing
it never downloads the referenced package bodies.

### Hash prediction

Delivering a requested package path from an already stored body because a fresh
metadata index names that body's digest. It applies only to the immutable
package files the index declares; a missing body or a stale index falls back to
the ordinary request path, and a prediction never creates a long-lived cached
variant of its own.

### Telemetry

Relay operational measurements emitted through OpenTelemetry. Metrics aggregate
cache outcomes, traffic, storage and prefetch activity without identifying an
individual URL, digest, variant or client. Foreground mirror traffic and
background work such as prefetch are separate measurement sources.

### Direct request

A request that reached the relay or the Worker without passing through a relay,
identified by the absence of the relay-host header. Any `X-MDB-Relay` on such a
request is meaningless and is removed; response directives are unaffected.

### Absolute reference

Any URL a mirror writes into its own response body or `Location` header (npm
tarball URLs, Helm index chart URLs, APT directory redirects, install-script
endpoints). Every absolute reference must be built from the relay host, or the
client leaves the relay on the next request.

### Hash object

An immutable body addressed by its real SHA-1 and SHA-256 digests. Both digests
identify the same stored body, which exists once and is not scoped to a mirror.
A relay may lose or evict the object at any time; callers treat that as a cache
miss, never as proof that the upstream content disappeared.

### Fragment cache

A temporary, partial representation of an immutable object while its bytes are
being obtained. Once the complete object store takes over, the fragment storage
is reclaimed. That reclamation is distinct from eviction of the complete hash
object, which continues to follow the content store's own capacity policy.

### Active cache index

A process-local association between an object identity and the fragment cache
currently filling that object. The identity is the content hash when one is
known, otherwise the request URL whose bytes are being assembled. It is not
durable: a restart abandons incomplete work, while completed hash objects remain
discoverable through the content store.

### Disk watermark

The minimum free space that must remain on the filesystem holding the cache.
Below it, new fragment and complete-object storage writes stop and requests
continue without caching; existing objects remain readable, and deletion and
pruning remain available. It applies to local temporary files as well as cache
bodies, not to remote object storage.

### Variant identity

The label that files one stored representation of a URL apart from its siblings.
A URL may hold several variants — two media types, two languages — each kept
with its own status, stored response headers, body digest and freshness. A
variant identity is the set of normalised request values for the request headers
the upstream's `Vary` names: variants are compared field by field, so the same
URL reached with the same normalised values is the same variant, and a URL whose
upstream sends no `Vary` has exactly one. A request that did not carry one of
those headers and one that carried it with an empty value are different
identities. An upstream may declare the path
sufficient on its own with the `no-vary` relay directive: then `Vary` does not
split the URL, `Vary: *` included, and it has one variant. `Accept-Encoding` is
never one of those fields, even when `Vary` names it. `Content-Encoding` is a
property of the stored body, not of the identity: a request whose encoding
preference differs from a stored variant's is served that variant and transcoded
on delivery, never filed as a different representation. A write for the same URL and the same identity
replaces that whole variant. The headers kept for delivery and revalidation
(validators, coding, lifetime, digest) are stored beside the identity, not in it.
`ETag` and `Last-Modified` are not part of it: they are *revalidation metadata*,
answering whether a stored variant is still current, never which variant a
request wants.

### Transcoding

Serving a stored variant's body in a content coding other than the one it was
stored in, on the fly during delivery. `identity`, `gzip`, `deflate`, `br` and
`zstd` convert to one another freely, in both directions, by decoding to
`identity` and then encoding to the target. The target is chosen by the request's
full `Accept-Encoding` preference — quality values, `*`, and `identity`. At
equal quality, an explicitly listed coding beats an `identity` that is merely
acceptable by default; otherwise the stored coding wins, then `zstd`, `br`,
`gzip`, `deflate`. The result is never stored and never filed as another variant. Because the delivered
bytes differ from the stored object, a transcoded delivery drops the headers that
describe those bytes (`Content-Length`, `Content-MD5`, `Digest`,
`Docker-Content-Digest`) and demotes a strong `ETag` to weak. Content whose
identity *is* a digest — an OCI digest path, or any entry carrying a
content-digest header — is never transcoded: an encoding mismatch there is
answered from upstream. A range request is likewise never answered from a
transcoded stream. A response marked `no-transform`, or one whose stored coding
cannot satisfy any acceptable target, is answered from upstream.

### Freshness

Whether a stored variant may still be handed to a client without asking the
upstream. It comes from the upstream's own lifetime (`s-maxage`, `max-age`,
`Expires`, in that shared-cache order); the `ttl` relay directive supplies a
lifetime only where the upstream states none, and defaults to none. A variant
marked `no-cache` is stored but must be revalidated before every delivery; a
request that says `no-cache` or `max-age=0` asks for the same, and a request
marked `no-store` must neither read nor write the cache. A response that declared
its own immutability keeps its stored variant for a persistent lifetime instead
of the upstream's stated one, because its bytes cannot change.

### Cache key

What a stored upstream response is filed under. Two kinds:

- **Address key** — the request URL; usable for mutable resources, expires. A
  stored mutable response is filed under the address key and its variant
  identity together. The method is not part of it: a `GET` and a `HEAD` share one
  entry, and only a complete `200` answer to the `GET` files a body. A `HEAD`
  never creates an entry — it is answered from an existing one, or sent upstream
  and left uncached.
- **Digest key** — the content digest, scoped per upstream host; immutable and
  shareable across repositories.

### Sanitised response

An upstream response with credentials-bearing and client-poisoning headers
removed, and (where appropriate) a cache lifetime chosen by the mirror instead
of the upstream. Sanitising happens before storing, so the cache never holds a
header the client must not see.

### Keep-HTTP

A per-mirror capability flag: the relay serves this mount over plain HTTP
without redirecting to HTTPS. Package managers whose upstream is declared over
HTTP depend on it.

### Status

A per-mirror health report (`initializing` / `healthy` / `unhealthy`) plus
mirror-specific statistics. Status is only available for mirrors that can
report it; the absence of a status is not an error.
