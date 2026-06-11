import type { Release } from "apt-parser";

import type { MirrorStatus } from "@server/types";

/**
 * Aggregate counts over a set of files. Ported from the old analytics
 * `CollectionStats`: `deduplicated*` collapse identical content (same hash)
 * shared across suites/components.
 */
export interface CollectionStats {
  files: number;
  size: number;
  deduplicatedFiles: number;
  deduplicatedSize: number;
}

/**
 * Running tally of `(hash, size)` entries. `files`/`size` count every entry;
 * `deduplicated*` count distinct hashes only, so content shared across suites
 * (same hash) is charged once. Ported from the old analytics `CollectionCounter`.
 */
export class CollectionCounter {
  private files = 0;
  private size = 0;
  private map: Record<string, number> = {};

  constructor(items?: [string, number][]) {
    if (items) this.bulk(items);
  }

  add(item: [string, number]): void {
    this.files += 1;
    this.size += item[1];
    this.map[item[0]] = (this.map[item[0]] ?? 0) + item[1];
  }

  bulk(items: [string, number][]): void {
    for (const item of items) this.add(item);
  }

  merge(other: CollectionCounter): void {
    this.files += other.files;
    this.size += other.size;
    // Merge the hash map directly — `add()` would re-increment files/size that
    // we already folded in above, doubling the aggregate totals.
    for (const key in other.map) {
      this.map[key] = (this.map[key] ?? 0) + (other.map[key] as number);
    }
  }

  export(): CollectionStats {
    return {
      files: this.files,
      size: this.size,
      deduplicatedFiles: Object.keys(this.map).length,
      deduplicatedSize: Object.values(this.map).reduce((acc, s) => acc + s, 0),
    };
  }
}

/**
 * A suite's `Release` metadata with the per-file checksum blocks
 * (`md5`/`sha1`/`sha256`/`sha512`) dropped — those carry the full file listing,
 * which is far too large for a status payload.
 */
export type SuiteMeta = Omit<Release, "md5" | "sha1" | "sha256" | "sha512">;

/**
 * Strip the checksum blocks off a parsed `Release` for the status payload.
 * Spreads the instance, then deletes the four hash arrays plus the inherited
 * `raw`/`required` maps from `APTBase` (own enumerable props that re-serialize
 * the same file listing) — no need to enumerate every kept field.
 */
export function toSuiteMeta(release: Release): SuiteMeta {
  const meta = { ...release } as Record<string, unknown>;
  for (const key of ["md5", "sha1", "sha256", "sha512", "raw", "required"]) {
    delete meta[key];
  }
  return meta as unknown as SuiteMeta;
}

/**
 * Status payload for an APT mirror. Extends the generic {@link MirrorStatus}
 * with file statistics derived from the resolved suite `Release` indexes —
 * ported from the old `ServerStatApt`. Stats are `false` until the repo has
 * resolved (while `health` is `"initializing"`).
 */
export interface AptMirrorStatus extends MirrorStatus {
  aptstatus?: {
    stats: CollectionStats;
    suitesStats: Record<string, CollectionStats>;
    suites: SuiteMeta[];
  };
}

/**
 * Tally aggregate + per-suite stats from a set of parsed `Release` indexes.
 * Per suite we take the strongest checksum block present
 * (sha512 → sha256 → sha1 → md5); merging into a shared counter dedups content
 * that recurs across suites. Ported from the old `AnalyzeAptRepo`.
 */
export function analyzeApt(suites: Release[]): {
  stats: CollectionStats;
  suitesStats: Record<string, CollectionStats>;
} {
  const counter = new CollectionCounter();
  const suitesStats: Record<string, CollectionStats> = {};

  for (const suite of suites) {
    const hashes = suite.sha512 ?? suite.sha256 ?? suite.sha1 ?? suite.md5;
    if (!hashes) continue;

    const suiteCounter = new CollectionCounter(hashes.map((h) => [h.hash, h.size]));
    counter.merge(suiteCounter);

    if (suite.suite) suitesStats[suite.suite] = suiteCounter.export();
  }

  return { stats: counter.export(), suitesStats };
}
