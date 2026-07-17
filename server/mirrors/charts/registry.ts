/**
 * Registry of mirrored Helm repos. Owns one `HelmRepoKeeper` per seed entry in
 * `repos.ts`, resolves upstream repo urls to their mirror name (for dependency
 * rewriting), and auto-mounts dependency repos discovered in mirrored indexes.
 *
 * Discovery is closed: only repo urls that appear as a dependency `repository`
 * in an already-fetched index ever get mounted — never a url from the request
 * path — so this stays a mirror of a known set, not an open proxy. Discovered
 * repos get a slug derived from their url and are mounted alongside the seeds.
 *
 * Mounts are isolate-scoped. A cold isolate re-derives them deterministically
 * from the seed indexes via `ensure` (a bounded, one-shot discovery sweep).
 */

import { v5 as uuidv5, NIL as NIL_UUID } from "uuid";

import { HelmReposList, type HelmRepo } from "./repos";
import { HelmRepoKeeper, normalizeRepoUrl } from "./keeper";

/** `https://charts.example.com/foo` → `charts-example-com-foo`. */
const slugForRepo = (normUrl: string) =>
  normUrl
    .replace(/^https?:\/\//i, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

export class HelmRegistry {
  private readonly keepers = new Map<string, HelmRepoKeeper>();
  /** Normalized upstream repo url → mirror name (seeds + discovered). */
  private readonly urlToName = new Map<string, string>();
  /** Memoized one-shot cold-isolate discovery sweep. */
  private discovery?: Promise<void>;

  constructor(seeds: HelmRepo[] = HelmReposList) {
    for (const repo of seeds) {
      this.urlToName.set(normalizeRepoUrl(repo.url), repo.name);
      this.keepers.set(repo.name, new HelmRepoKeeper(repo, this));
    }
  }

  /** Mirror name for an upstream repo url, or undefined if not mounted. */
  mirrorNameFor(repoUrl: string): string | undefined {
    return this.urlToName.get(normalizeRepoUrl(repoUrl));
  }

  /**
   * Mount a repo url discovered as a dependency, returning its mirror name.
   * Idempotent — an already-mounted url returns its existing name.
   */
  register(repoUrl: string): string {
    const url = normalizeRepoUrl(repoUrl);
    const existing = this.urlToName.get(url);
    if (existing) return existing;

    let name = slugForRepo(url);
    // Guard the rare slug collision between two distinct urls.
    if (this.keepers.has(name)) name = `${name}-${uuidv5(url, NIL_UUID).slice(0, 8)}`;

    this.urlToName.set(url, name);
    this.keepers.set(name, new HelmRepoKeeper({ name, url }, this));
    return name;
  }

  /** Mount every un-mirrored dependency repo a keeper found on its last refresh. */
  private absorb(keeper: HelmRepoKeeper) {
    for (const url of keeper.unknownDeps) this.register(url);
  }

  /**
   * Resolve a repo by name, mounting discovered repos on a cold isolate first.
   * Seeds and already-mounted repos return immediately; a miss triggers a
   * bounded discovery sweep (refresh everything, absorb, repeat until stable).
   */
  async ensure(name: string): Promise<HelmRepoKeeper | undefined> {
    if (this.keepers.has(name)) return this.keepers.get(name);
    await (this.discovery ??= this.runDiscovery());
    return this.keepers.get(name);
  }

  private async runDiscovery() {
    for (let round = 0; round < 5; round++) {
      const before = this.keepers.size;
      await Promise.allSettled(
        [...this.keepers.values()].map((k) => k.waitInitialized()),
      );
      for (const keeper of [...this.keepers.values()]) this.absorb(keeper);
      if (this.keepers.size === before) break; // converged
    }
  }

  /** Refresh a keeper's index, then mount any dependency repos it references. */
  async refreshAndMount(keeper: HelmRepoKeeper) {
    await keeper.refresh();
    this.absorb(keeper);
  }
}

/** Isolate-scoped registry built from the static seed list. */
export const registry = new HelmRegistry();
