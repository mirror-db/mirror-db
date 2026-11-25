# ADR 0013 — One Worker-owned APT metadata index, published as a fixed schema

- **Status:** accepted (2026-11-23, implemented — not deployment-verified)
- **Date:** 2026-11-23

## Context

Several APT mounts point at upstreams with a real archive but no autoindex
(`syncthing-pkgs`, `hashicorp`, `mise`, `opentofu`, `github-cli`). They have
`dists/` and `pool/` trees that APT clients can consume, yet no directory listing
to browse, and the relay has no way to know the expected digest of a package
file before it fetches one. M3 needs both: a synthetic listing for the front
door, and a path-to-digest expectation for the relay's store.

Two implementations were possible: a parser in the Worker (TypeScript) for the
listing and a second parser in the relay (Go) for the digest table. That shape
would let the two disagree about what a `Release` references, which files are
metadata, and which digest wins. A disagreement is not cosmetic: the listing
would advertise a path the relay refuses to serve from the store, or worse, the
relay would serve bytes that a listing does not describe. It also doubles the
maintenance of APT's stanza format, `Release` checksum sections and `by-hash`
naming in two languages.

The relay is optional and carries no upstream knowledge; a mirror's browsing
support must not depend on it being present. The Worker already parses `Release`
for the APT proxy (`apt-parser`) and already serves the mount.

APT metadata is large but finite per repository, while package bodies are not.
Anything that walks references must stop at metadata, and every generation must
be bounded in decompressed bytes and retained paths.

## Decision

1. **The Worker is the only parser.** It walks `Release`/`InRelease` into the
   referenced metadata, parses `Packages`/`Sources`, and publishes one
   normalised index. The relay consumes that index and never parses APT metadata
   for listing or digest prediction. There is no second parser, and the index is
   not distributed through the mirror manifest.

2. **One fixed endpoint, one versioned schema.** The index is served at
   `/<mount>/.mdb/apt-index.json`, a path derived from the mount rather than
   configured. The body is a single JSON document with `schema: 1` carrying the
   mirror, generation and expiry times, each suite's `Release` identity, the
   `Release` metadata files, and the de-duplicated package files with path,
   size, SHA-256 and SHA-1. It carries no unrelated package fields and no
   absolute upstream URL: every path is mount-relative, so a relay or a client
   can only address the mirror. Schema 1 has no pagination, no NDJSON, no R2
   object and no scheduled builder.

3. **`Release` to `Packages`/`Sources` is the only recursion.** Every file a
   `Release` lists enters the directory and the metadata digest table, but only
   `Packages`/`Sources` are downloaded and parsed further. `Translation`,
   `Contents`, `dep11` and similar entries are registered, never fetched for
   their own sake. A `Release` carries the digest of `Packages`, not of the
   packages inside it, so package digests only come from parsing the referenced
   `Packages`/`Sources`. Fetching a listed package file is never implied by the
   index: no `.deb` body is downloaded while building or refreshing it.

4. **One generation, validated as a whole, switched atomically.** A build starts
   from one `Release` snapshot per suite and resolves every referenced metadata
   file from that snapshot, preferring `by-hash` and otherwise checking the
   listed size and digest. Only gzip and plain `Packages`/`Sources` are
   supported in schema 1; an index that only offers xz, bz2 or zstd is not
   parsed. Any required file failing validation, any resource limit exceeded,
   any path escaping the mount, or the same mount-relative path arriving with
   conflicting digests rejects the whole new generation across every suite. A
   rejected build publishes nothing; the previous generation remains served.
   Readers see one generation or the previous one, never a mix.

5. **The same snapshot serves both consumers.** Directory synthesis reads the
   metadata and package paths from the generation. Digest prediction uses the
   same generation and applies only to immutable package files it declares:
   a URL cache miss may look the digest up in the store and deliver a stored body
   directly, SHA-256 first and SHA-1 as fallback. The index never predicts a
   mutable `Release` or `Packages` and never lets a request skip upstream for
   one. A hash delivery does not create a long-lived URL variant, and its
   freshness never outlives the index. A missing body, a non-matching stored
   body, a failed refresh or a stale index falls back to the ordinary upstream
   path; the relay does not serve from a stale index, and old bodies are left to
   the store's LRU.

6. **Small, explicit bounds.** A single `Packages`/`Sources` may expand to at
   most 8 MiB, one mirror generation may parse at most 64 MiB and retain at most
   50 000 de-duplicated files. Exceeding any of them rejects the generation as
   in point 4. An empty index is a valid result.

## Consequences

- One implementation, one contract. A listing and its digest expectations cannot
  describe different upstream states, and a Go consumer needs no APT format
  knowledge beyond the JSON.
- A directory works with no relay deployed, because the Worker owns both the
  listing and the contract.
- The failure mode is all-or-nothing per mirror generation: a broken or
  over-budget refresh leaves the entire previous generation intact, so browsing
  keeps working and prediction keeps matching what it says.
- Refresh cost is bounded and metadata-only. Package bodies still arrive only
  through client demand (or a lease, [ADR 0014](0014-demand-driven-prefetch-leases.md)),
  so this does not become a repository mirror.
- The index adds one fixed path under each APT mount. It must be excluded from
  the mirror's ordinary tree resolution so a client cannot fetch it as an
  upstream path.
- A repository whose `Packages`/`Sources` exist only as xz, bz2 or zstd has no
  index under schema 1 and therefore no synthetic listing. That is a stated
  limit, not a silent empty answer.

## Alternatives considered

- *Parse APT metadata in both the Worker and the relay.* Rejected: two
  implementations of the same format drift, and the resulting disagreement is
  invisible until a listing and a cache hit contradict each other.
- *Parse in the relay only, with the Worker consuming the relay's result.*
  Rejected: the relay is optional, and a mount's browsing support must not
  disappear when no relay is in front.
- *Distribute the index through the mirror manifest.* Rejected: the manifest
  carries pre-request routing facts; an index is per-mount content with its own
  freshness, and putting it in the manifest mixes two lifetimes.
- *Publish partial generations as files become available.* Rejected: a directory
  and a digest table from different upstream moments are exactly the
  incoherence this ADR avoids.
- *Follow `Release` references recursively into `Contents`, `Translation` and
  package bodies.* Rejected: that turns a metadata index into a site crawler and
  a repository mirror.
- *Pagination, NDJSON or R2 storage for large indexes.* Deferred: schema 1
  bounds the input so one document stays feasible; a future size problem is a
  new schema, not an extension of this one.
