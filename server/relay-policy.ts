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

/**
 * Ordered policies. Specific rules come before broad ones; none is a catch-all,
 * because the relay already defaults to `latency`.
 */
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
];
