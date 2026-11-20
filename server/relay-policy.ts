/**
 * Relay traffic policies distributed with the mirror manifest.
 *
 * The relay evaluates `policies` in array order and the first complete match
 * wins; an unmatched request uses `latency`. Paths are mount-relative (the
 * mount prefix is already removed), start with `/`, exclude the query string,
 * and keep their original case and escaping. Every present matcher is ANDed.
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
  traffic_policy: TrafficPolicy;
}

export interface RelayPolicy {
  name: string;
  description: string;
  match: PolicyMatch;
  rules: PolicyRules;
}

/**
 * Archive, image, installer, and package suffixes that benefit from sustained
 * throughput. `gz` alone is deliberately absent: `Packages.gz`, tarball
 * signatures, and index files are small or latency-sensitive.
 */
export const LARGE_ARTIFACT_PATTERN =
  String.raw`\.(?:tar|tar\.gz|tgz|tar\.xz|txz|tar\.zst|tar\.bz2|tbz2|zip|7z|iso|img|squashfs|bin|deb|ddeb|udeb|rpm|apk|exe|msi|dmg|pkg)$`;

/** Ordered policies. Broad fallbacks must come after specific rules. */
export const relayPolicies: RelayPolicy[] = [
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
  {
    name: "default",
    description: "Unmatched requests prefer a prompt first byte.",
    match: {},
    rules: { traffic_policy: "latency" },
  },
];
