# ADR 0010 — Generate APT source files from explicit presets

- **Status:** accepted
- **Date:** 2026-11-21

## Context

The service mirrors APT repositories with several incompatible layouts. A
normal repository uses `dists/<suite>` below one root; a flat repository has its
indexes beside its packages; Docker, Microsoft, Pigsty, CUDA, and Kubernetes add
product, operating-system, release, or architecture path components. Deriving a
client configuration from the mirror name or an upstream directory listing would
silently produce plausible but invalid configurations.

A public configuration URL also creates compatibility obligations. Moving
release aliases, ambiguous default versions, and unverified upstream directories
into the catalog would make generated files change meaning or fail for apt
clients.

## Decision

`/conf/apt/<name>.sources` renders a named preset. Each preset is a function that
returns an array of deb822 stanzas, allowing special repositories to choose their
own URI mapping and to emit multiple stanzas. Presets are explicit and tested;
repository layout is never guessed from a mirror's directory listing.

Every preset defines suitable suites, components, architecture behaviour, and
signing material. Debian and Ubuntu omit `Architectures` by default so apt uses
its native architecture selection. Third-party repositories default to `amd64`.
A repository whose URI contains the architecture emits one stanza per selected
architecture. Flat repositories use `Suites: /` and omit Components.

Query parameters override suite, component, and architecture defaults. Besides
the plural names, the accepted aliases are `suite`, `component`, `comp`, `comps`,
`architecture`, `arch`, and `archs`. Values are comma-separated, trimmed,
deduplicated, and order-preserving. `protocol` / `proto=http|https` selects the
scheme of every generated URI and defaults to HTTPS. Unknown parameters,
conflicting aliases, and values invalid for a preset are errors rather than
silently corrected input.

A versioned Debian preset includes its main and security stanzas. Each release
has exactly one security suite; it is fixed by the preset, while a `suites`
override applies only to the main stanza. There is no separate Debian security
configuration entry.

Debian and Ubuntu configurations use their installed archive keyring paths.
Other repositories embed the armored public key directly in `Signed-By`, reusing
the key proxy's parsing and deb822 formatting logic. Multi-certificate keyrings
remain one armored block. Every absolute URI is built from the active relay
origin.

The catalog includes only useful, verified presets. Versioned operating-system
repositories require an explicit version or codename; there are no unversioned
`debian.sources` or `ubuntu.sources` defaults. Ports, snapshots, non-Debian
families, empty or duplicate repositories, and preview/test/internal channels are
excluded. Known compatibility restrictions, such as older CUDA certificates
under apt 3.x, are stated in comments rather than hidden or bypassed with
insecure apt options.

`/conf/apt/` lists exactly the registered preset paths. Browser user agents
receive a line-oriented HTML `<pre>` list; other clients receive JSON.

## Consequences

- Adding a repository requires an explicit preset and fixtures, but cannot
  accidentally invent an unsupported layout.
- Preset names and their default stanzas are public compatibility surface.
- Rendering an inline-key configuration depends on the upstream key being
  available; key failures surface as 502 instead of emitting an unsigned source.
- Users can override defaults without learning repository-specific parameter
  names, while preset validation still rejects malformed flat or multi-root
  combinations.
