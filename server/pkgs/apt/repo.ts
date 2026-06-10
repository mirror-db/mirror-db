/**
 * In-memory index of an upstream APT repository.
 *
 * On first use the repo resolves itself: discover the suites under `dists/`,
 * fetch + parse each suite's `Release`/`InRelease`, and collect the set of known
 * file paths and sha256 hashes plus the earliest `Valid-Until`. That index lets
 * the proxy answer 404 for files that don't exist and choose a cache lifetime.
 *
 * Resolution is best-effort and lives only for the isolate's lifetime. Until the
 * first resolve completes the repo reports `state === "resolving"` (or `idle`)
 * and the proxy passes everything through unjudged. The index is re-resolved in
 * the background once `Valid-Until` passes; the stale index keeps serving until
 * the swap, so enforcement never has a gap.
 */

import { parseRelease, type ReleaseIndex } from "./release";

export type FetchImpl = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;

export type RepoState = "idle" | "resolving" | "ready";

export interface AptRepoOptions {
  /** Full upstream prefix, ending in `/`, e.g. `https://cloudflaremirrors.com/debian/`. */
  base: string;
  /** Fallback suite list when the `dists/` listing can't be parsed. */
  suites?: string[];
  /** Injected fetch (defaults to global). */
  fetchImpl?: FetchImpl;
}

/** Used when a suite's Release omits `Valid-Until`, and as the re-resolve floor. */
const DEFAULT_TTL_MS = 60 * 60 * 1000; // 1 hour
/** Throttle for retrying a resolve that produced nothing (avoid hammering upstream). */
const RETRY_COOLDOWN_MS = 60 * 1000;

export class AptRepo {
  readonly base: string;
  private readonly configuredSuites: string[];
  private readonly fetchImpl: FetchImpl;

  state: RepoState = "idle";
  knownPaths = new Set<string>();
  knownHashes = new Set<string>();
  validUntil = 0;
  private lastAttempt = 0;

  constructor(opts: AptRepoOptions) {
    this.base = opts.base.endsWith("/") ? opts.base : `${opts.base}/`;
    this.configuredSuites = opts.suites ?? [];
    this.fetchImpl = opts.fetchImpl ?? (globalThis.fetch as FetchImpl);
  }

  /** True once a resolve has populated the index. */
  get ready(): boolean {
    return this.state === "ready";
  }

  isExpired(now: number): boolean {
    return now >= this.validUntil;
  }

  /**
   * Idempotently ensure the index is fresh. Kicks a background `resolve()` when
   * idle, or when ready-but-expired, throttled by `RETRY_COOLDOWN_MS`. Never
   * blocks the caller; pass `waitUntil` to keep the isolate alive for the write.
   */
  ensureResolved(waitUntil?: (p: Promise<unknown>) => void): void {
    const now = Date.now();
    if (this.state === "resolving") return;
    if (this.state === "ready" && !this.isExpired(now)) return;
    if (now - this.lastAttempt < RETRY_COOLDOWN_MS) return;
    this.lastAttempt = now;

    // Keep a ready (stale) index serving while we re-resolve in the background;
    // only the very first resolve flips enforcement on, at the end.
    if (this.state === "idle") this.state = "resolving";

    const job = this.resolve().catch(() => {
      if (this.state === "resolving") this.state = "idle";
    });
    if (waitUntil) waitUntil(job);
  }

  /** Fetch + parse all suites, then atomically swap in the new index. */
  async resolve(): Promise<void> {
    const suites = await this.discoverSuites();

    const knownPaths = new Set<string>();
    const knownHashes = new Set<string>();
    let validUntil = Infinity;

    for (const suite of suites) {
      const idx = await this.fetchRelease(suite);
      if (!idx) continue;

      const prefix = `dists/${suite}/`;
      // The signed index files themselves are always valid paths.
      for (const name of ["InRelease", "Release", "Release.gpg"]) {
        knownPaths.add(prefix + name);
      }
      for (const file of idx.files) {
        knownPaths.add(prefix + file.path);
        knownHashes.add(file.hash);
      }

      const vu = idx.validUntil ?? Date.now() + DEFAULT_TTL_MS;
      validUntil = Math.min(validUntil, vu);
    }

    // A resolve that surfaced nothing usable (upstream hiccup, blocked listing)
    // must not flip enforcement on — that would 404 the whole repo. Leave the
    // state as-is so the next request retries after the cooldown.
    if (knownPaths.size === 0) {
      if (this.state === "resolving") this.state = "idle";
      return;
    }

    this.knownPaths = knownPaths;
    this.knownHashes = knownHashes;
    this.validUntil =
      validUntil === Infinity ? Date.now() + DEFAULT_TTL_MS : validUntil;
    this.state = "ready";
  }

  /** Discover suite dirs from the `dists/` listing, falling back to config. */
  private async discoverSuites(): Promise<string[]> {
    try {
      const listed = await this.fetchListing("dists/");
      if (listed.length) return listed;
    } catch {
      // fall through to configured suites
    }
    return this.configuredSuites;
  }

  /**
   * Parse an Apache/nginx-style directory listing for child directory names.
   * Returns `[]` when `HTMLRewriter` is unavailable (e.g. Node unit tests), so
   * the caller falls back to the configured suite list.
   */
  private async fetchListing(rel: string): Promise<string[]> {
    if (typeof HTMLRewriter === "undefined") return [];

    const res = await this.fetchImpl(this.url(rel));
    if (!res.ok) return [];

    const names: string[] = [];
    await new HTMLRewriter()
      .on("a", {
        element(el) {
          let href = el.getAttribute("href");
          if (!href) return;
          if (href.startsWith("/") || href.startsWith("http")) return;
          if (!href.endsWith("/")) return; // directories only
          href = href.replace(/\/$/, "");
          if (href === "." || href === "..") return;
          names.push(decodeURIComponent(href));
        },
      })
      .transform(res)
      .arrayBuffer();

    return names;
  }

  /** Fetch + parse a suite's `InRelease` (preferred) or `Release`. */
  private async fetchRelease(suite: string): Promise<ReleaseIndex | null> {
    for (const name of ["InRelease", "Release"]) {
      const res = await this.fetchImpl(this.url(`dists/${suite}/${name}`));
      if (res.ok) return parseRelease(await res.text());
    }
    return null;
  }

  /** Resolve a repo-relative path against the upstream base. */
  url(rel: string): string {
    return new URL(rel, this.base).toString();
  }
}
