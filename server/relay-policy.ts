/**
 * Relay traffic policies distributed with the mirror manifest.
 *
 * Policies are evaluated in order and their `rules` are merged field by field:
 * every matching policy contributes the fields it sets, and a later match wins
 * for those fields. `exit: true` stops evaluation after the matching policy has
 * been merged, so a broad rule cannot undo a specific one placed before it.
 *
 * Defaults for an unmatched request are `traffic_policy: "latency"`,
 * `passthrough: false`. There is no catch-all policy: a trailing `latency` rule
 * would override a `speed` rule matched earlier under last-match-win.
 *
 * Response-side decisions never belong here. `no-store`, `no-vary`,
 * `no-transform`, actual freshness, encoding and digests are observed only
 * after the upstream answers, so they travel as response headers
 * (`X-MDB-Relay` or standard `Cache-Control`). This schema deliberately has no
 * `cache` or `cache_ttl`: request caching is relay-side behaviour, not a
 * per-mirror policy the Worker tunes.
 *
 * `path_pattern` must stay RE2-compatible: no backreferences, lookaround, or
 * other constructs that permit backtracking-based denial of service.
 */

export type TrafficPolicy = "latency" | "speed";

export interface PolicyMatch {
  /** Opaque tag advertised by a mirror (`policy_tags`). */
  tag?: string;
  /** Exact mirror name. */
  mirror?: string;
  /** Mount-relative path prefix, e.g. `/pool/`. */
  path_prefix?: string;
  /** Mount-relative path suffix. */
  path_suffix?: string;
  /** RE2-compatible regular expression matched against the whole path. */
  path_pattern?: string;
}

export interface PolicyRules {
  /** Transport to use. Omitted means the relay default (`latency`). */
  traffic_policy?: TrafficPolicy;
  /** Bypass relay request caching for this policy. Defaults to `false`. */
  passthrough?: boolean;
  /**
   * Evaluation control, not a merged value: after this policy matches and its
   * rules are merged, stop evaluating later policies.
   */
  exit?: boolean;
  /**
   * RE2-compatible regular expression, matched against the mount-relative URL
   * path, that declares the path's bytes immutable. It may carry a named group
   * `sha256` or `sha1` (syntax shared by RE2 and JS: `(?<sha256>…)`); when
   * present, the captured hex is the object's content digest. A match is a
   * pre-request fact and never changes `traffic_policy`. Omitted means inherit.
   */
  immutable?: string;
}

export interface RelayPolicy {
  name: string;
  description: string;
  match: PolicyMatch;
  rules: PolicyRules;
}

/**
 * OCI digest-addressed blobs and manifests. A tag manifest
 * (`/manifests/latest`) carries no digest and is deliberately not matched, so
 * it keeps the default latency pool; a digest blob also matches `oci-blobs`
 * afterwards and inherits `speed`.
 */
export const OCI_DIGEST_PATTERN =
  String.raw`^/v2/.+/(?:blobs|manifests)/sha256:(?<sha256>[0-9a-f]{64})$`;

/**
 * APT `by-hash` objects. This rule extracts the two digest algorithms the
 * schema declares, SHA-256 and SHA-1; an algorithm it does not name (such as
 * SHA512) does not match this extraction rule.
 *
 * The optional leading `(?:.+/)?` covers repositories nested below the mount
 * root rather than directly under it: `microsoft` serves
 * `/code/dists/…/by-hash/…` and `pigsty` serves `infra/dists/…` and
 * `pgsql/<codename>/dists/…`. The plain `debian`-style `/dists/…` layout still
 * matches, because the group is optional.
 */
export const APT_BY_HASH_PATTERN =
  String.raw`^/(?:.+/)?dists/.+/by-hash/(?:SHA256/(?<sha256>[0-9a-f]{64})|SHA1/(?<sha1>[0-9a-f]{40}))$`;

/**
 * PyPI package files under `/pypi/`. Upstream forbids re-uploading a filename:
 * a project may add a new file, but a path that exists keeps its bytes. The
 * 64-hex segment in a `/packages/` path is an upstream layout id, **not** the
 * body's SHA-256, so this rule declares immutability without capturing a digest.
 */
export const PYPI_PACKAGES_PATTERN = String.raw`^/packages/`;

/**
 * npm tarballs: `/<pkg>/-/<pkg>-<version>.tgz`, plus the scoped form
 * `/<@scope>/<pkg>/-/<pkg>-<version>.tgz`. Package metadata documents
 * (`/<pkg>`, `/<@scope>/<pkg>`) stay mutable — a new version rewrites them.
 */
export const NPM_TARBALL_PATTERN =
  String.raw`^/(?:@[^/]+/)?[^/]+/-/[^/]+\.tgz$`;

/**
 * Node.js release directories: `/v<major>.<minor>.<patch>/…`. The `latest-*`
 * aliases live at the same depth but do not start with `v<digits>.`.
 */
export const NODEJS_RELEASE_PATTERN = String.raw`^/v\d+\.\d+\.\d+/`;

/**
 * Go release files at the `go.dev/dl/` root. Covers source archives, the
 * per-platform installers (including the `betaN` / `rcN` suffixes and the old
 * `darwin-*-osx10.x` names) and the two `go<version>-bootstrap-<date>.tar.gz`
 * toolchain seeds. Adjacent `*.sha256`, `*.asc` and the root `index.json`
 * deliberately do not match: only release files are named.
 */
export const GO_RELEASE_PATTERN =
  String.raw`^/go\d+(?:\.\d+)+(?:beta\d*|rc\d*)?(?:\.(?:src\.tar\.gz|(?:linux|darwin|windows|freebsd|netbsd|openbsd|dragonfly|solaris|illumos|aix|plan9)-[a-z0-9]+(?:-osx[0-9.]+)?\.(?:tar\.gz|zip|pkg|msi|tar\.xz))|-bootstrap-\d{8}\.tar\.gz)$`;

/**
 * Kubernetes release directories: `/release/v<major>.<minor>.<patch>/…`.
 * `release/stable-<minor>.txt` is a moving pointer and does not match.
 */
export const KUBE_RELEASE_PATTERN = String.raw`^/release/v\d+\.\d+\.\d+/`;

/**
 * Debian CD/DVD release directories: `/12.9.0/…` and `/12.9.0-live/…`. The
 * `current/` and `project/` aliases are moving pointers and do not match.
 */
export const DEBIAN_CD_PATTERN = String.raw`^/\d+(?:\.\d+)+(?:-live)?/`;

/**
 * Debian cloud images: `/<codename>/<YYYYMMDD>-<build>/…`. The `daily/` tree
 * and the `latest/` aliases are moving pointers and do not match.
 */
export const DEBIAN_CLOUD_PATTERN = String.raw`^/[a-z]+/\d{8}-\d{4}/`;

/**
 * Ubuntu cloud images: `/releases/<codename>/release-<YYYYMMDD>/…`, including
 * the `.N` rebuild suffix upstream uses when a dated build is reissued. The
 * `daily*` trees and `latest` aliases do not match.
 */
export const UBUNTU_CLOUD_PATTERN =
  String.raw`^/releases/[a-z]+/release-\d{8}(?:\.\d+)?/`;

/**
 * Ubuntu release directories: `/24.04.5/…`, `/24.04.5.1/…`. At least three
 * numeric components are required, so a two-component moving alias such as
 * `24.04/` (upstream repoints it at the current point release) and a codename
 * alias such as `jammy/` both stay mutable.
 */
export const UBUNTU_RELEASES_PATTERN = String.raw`^/\d+\.\d+\.\d+(?:\.\d+)*/`;

/**
 * Archive, image, installer, and package suffixes that benefit from sustained
 * throughput. `gz` alone is deliberately absent: `Packages.gz`, tarball
 * signatures, and index files are small or latency-sensitive.
 */
export const LARGE_ARTIFACT_PATTERN =
  String.raw`\.(?:tar|tar\.gz|tgz|tar\.xz|txz|tar\.zst|tar\.bz2|tbz2|zip|7z|iso|img|squashfs|bin|deb|ddeb|udeb|rpm|apk|exe|msi|dmg|pkg)$`;

/**
 * Ordered policies. Specific rules come before broad ones; none is a catch-all,
 * because the relay already defaults to `latency`.
 */
export const relayPolicies: RelayPolicy[] = [
  {
    name: "oci-digests",
    description:
      "Digest-addressed OCI blobs and manifests; a tag manifest keeps latency.",
    match: { tag: "oci", path_pattern: OCI_DIGEST_PATTERN },
    rules: { immutable: OCI_DIGEST_PATTERN },
  },
  {
    name: "apt-by-hash",
    description:
      "APT by-hash objects are addressed by their own content digest, at the mount root or nested below it.",
    match: { tag: "apt", path_pattern: APT_BY_HASH_PATTERN },
    rules: { immutable: APT_BY_HASH_PATTERN },
  },
  {
    name: "pypi-packages",
    description:
      "PyPI package files; upstream never accepts a second upload under one filename.",
    match: { mirror: "pypi", path_pattern: PYPI_PACKAGES_PATTERN },
    rules: { immutable: PYPI_PACKAGES_PATTERN },
  },
  {
    name: "npm-tarballs",
    description: "npm tarballs, scoped and unscoped; metadata stays mutable.",
    match: { mirror: "npm", path_pattern: NPM_TARBALL_PATTERN },
    rules: { immutable: NPM_TARBALL_PATTERN },
  },
  {
    name: "nodejs-releases",
    description:
      "Node.js version directories; `latest-*` aliases stay mutable.",
    match: { mirror: "nodejs", path_pattern: NODEJS_RELEASE_PATTERN },
    rules: { immutable: NODEJS_RELEASE_PATTERN },
  },
  {
    name: "go-releases",
    description: "Go release archives and installers at the download root.",
    match: { mirror: "go-releases", path_pattern: GO_RELEASE_PATTERN },
    rules: { immutable: GO_RELEASE_PATTERN },
  },
  {
    name: "kube-releases",
    description:
      "Kubernetes release directories; `stable-*.txt` pointers stay mutable.",
    match: { mirror: "kube-releases", path_pattern: KUBE_RELEASE_PATTERN },
    rules: { immutable: KUBE_RELEASE_PATTERN },
  },
  {
    name: "debian-cd",
    description:
      "Debian CD/DVD release directories; `current/` and `project/` are aliases.",
    match: { mirror: "debian-cd", path_pattern: DEBIAN_CD_PATTERN },
    rules: { immutable: DEBIAN_CD_PATTERN },
  },
  {
    name: "debian-cloud",
    description:
      "Dated Debian cloud image builds; `daily/` and `latest/` are aliases.",
    match: { mirror: "debian-cloud", path_pattern: DEBIAN_CLOUD_PATTERN },
    rules: { immutable: DEBIAN_CLOUD_PATTERN },
  },
  {
    name: "ubuntu-cloud",
    description: "Dated Ubuntu cloud image builds; `daily*` and aliases stay mutable.",
    match: { mirror: "ubuntu-cloud", path_pattern: UBUNTU_CLOUD_PATTERN },
    rules: { immutable: UBUNTU_CLOUD_PATTERN },
  },
  {
    name: "ubuntu-releases",
    description: "Ubuntu numbered release directories; codename aliases stay mutable.",
    match: { mirror: "ubuntu-releases", path_pattern: UBUNTU_RELEASES_PATTERN },
    rules: { immutable: UBUNTU_RELEASES_PATTERN },
  },
  {
    name: "oci-blobs",
    description: "OCI layer blobs are large and benefit from sustained throughput.",
    match: { tag: "oci", path_pattern: "^/v2/.+/blobs/" },
    rules: { traffic_policy: "speed" },
  },
  {
    name: "apt-packages",
    description: "APT pool payloads are packages; indexes stay on the latency pool.",
    match: { tag: "apt", path_prefix: "/pool/" },
    rules: { traffic_policy: "speed" },
  },
  {
    name: "large-artifacts",
    description:
      "Common archive, image, installer, and package suffixes on any mirror.",
    match: { path_pattern: LARGE_ARTIFACT_PATTERN },
    rules: { traffic_policy: "speed" },
  },
];
