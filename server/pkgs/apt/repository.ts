import type { AptMirrorStatus } from "./status";

/** How much of the repository tree a generation can judge absent. */
export type AptRepositoryCoverage = "metadata-only" | "tree";

/**
 * Immutable facts published with one APT repository generation. All consumers
 * of a generation read the same path, hash, freshness and health summary.
 */
export interface AptRepositoryFacts {
  readonly paths: ReadonlySet<string>;
  readonly hashes: ReadonlySet<string>;
  readonly suites: ReadonlySet<string>;
  readonly validUntil: number;
  readonly coverage: AptRepositoryCoverage;
  readonly status: AptMirrorStatus;
}

/** The repository facts seam shared by directory discovery and snapshots. */
export interface AptRepository {
  readonly base: string;
  url(rel: string): string;
  /** Wait for a current generation when needed; `null` means none has succeeded. */
  facts(): Promise<AptRepositoryFacts | null>;
}
