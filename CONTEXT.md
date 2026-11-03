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
`speedtest`).

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
carries no upstream knowledge, only transport. When present, it tells the
Worker which public host the client actually reached (`X-MDB-Relay-Host`), and
the Worker must emit absolute URLs on *that* host so the client stays on the
relay path.

### Relay host

The value of `X-MDB-Relay-Host` — the outermost relay's base domain as seen by
the end user. Untrusted input: anyone can set it, so it may only shape URLs
returned to the client, never grant access.

### Absolute reference

Any URL a mirror writes into its own response body or `Location` header (npm
tarball URLs, Helm index chart URLs, APT directory redirects, install-script
endpoints). Every absolute reference must be built from the relay host, or the
client leaves the relay on the next request.

### Cache key

What a stored upstream response is filed under. Two kinds:

- **Address key** — the request URL; usable for mutable resources, expires.
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
