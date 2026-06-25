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

import { Release } from "apt-parser";

import { cachedfetch } from "@server/pkgs/fetch";
import { WebListFs, type WebListFetch } from "@server/pkgs/web-list";
import urlJoin from "url-join";

import { analyzeApt, toSuiteMeta, type AptMirrorStatus } from "./status";

export type RepoState = "idle" | "resolving" | "ready";

export interface AptRepoOptions {
  /** Full upstream prefix, ending in `/`, e.g. `http://ftp.us.debian.org/debian/`. */
  base: string;
  /** Fallback suite list when the `dists/` listing can't be parsed. */
  suites?: string[];
  /** Injected fetch (defaults to cachedfetch via WebListFs). */
  fetchImpl?: WebListFetch;
}

/** Used when a suite's Release omits `Valid-Until`, and as the re-resolve floor. */
const DEFAULT_TTL_MS = 60 * 60 * 1000; // 1 hour
/** Throttle for retrying a resolve that produced nothing (avoid hammering upstream). */
const RETRY_COOLDOWN_MS = 60 * 1000;

export class AptRepo {
  readonly base: string;
  private readonly configuredSuites: string[];
  private readonly fs: WebListFs;

  state: RepoState = "idle";
  knownPaths = new Set<string>();
  knownHashes = new Set<string>();
  /** Suites the index actually resolved — the scope enforcement is authoritative over. */
  knownSuites = new Set<string>();
  /** Parsed `Release` per resolved suite — drives {@link status}. */
  suites: Release[] = [];
  validUntil = 0;
  private lastAttempt = 0;
  /** The in-flight resolve, shared so concurrent callers dedup onto one run. */
  private inflight: Promise<void> | null = null;

  constructor(opts: AptRepoOptions) {
    this.base = opts.base.endsWith("/") ? opts.base : `${opts.base}/`;
    this.configuredSuites = opts.suites ?? [];
    this.fs = new WebListFs(this.base, { fetchImpl: opts.fetchImpl });
  }

  /** True once a resolve has populated the index. */
  get ready(): boolean {
    return this.state === "ready";
  }

  isExpired(): boolean {
    return Date.now() >= this.validUntil;
  }

  /**
   * Idempotently ensure the index is fresh. Kicks a background `resolve()` when
   * idle, or when ready-but-expired, throttled by `RETRY_COOLDOWN_MS`. Never
   * blocks the caller; pass `waitUntil` to keep the isolate alive for the write.
   */
  ensureResolved(waitUntil?: (p: Promise<unknown>) => void): void {
    if (this.state === "resolving") return;
    if (this.state === "ready" && !this.isExpired()) return;
    if (Date.now() - this.lastAttempt < RETRY_COOLDOWN_MS) return;

    const job = this.kick();
    if (waitUntil) waitUntil(job);
  }

  /**
   * Resolve and wait for the result, sharing any in-flight run. Use when the
   * caller needs a populated index in hand (e.g. `/status`) rather than the
   * fire-and-forget freshness nudge of {@link ensureResolved}. Bypasses the
   * retry cooldown — the caller is explicitly asking to block on a resolve.
   */
  async awaitResolved(): Promise<void> {
    if (this.ready && !this.isExpired()) return;
    await this.kick();
  }

  /** Start a resolve (or join the running one), tracking it as `inflight`. */
  private kick(): Promise<void> {
    if (this.inflight) return this.inflight;
    this.lastAttempt = Date.now();

    // Keep a ready (stale) index serving while we re-resolve in the background;
    // only the very first resolve flips enforcement on, at the end.
    if (this.state === "idle") this.state = "resolving";

    this.inflight = this.resolve()
      .catch((e) => {
        console.log("RESOLVE THREW:", e?.stack || e?.message || String(e));
        if (this.state === "resolving") this.state = "idle";
      })
      .finally(() => {
        this.inflight = null;
      });
    return this.inflight;
  }

  /** Fetch + parse all suites, then atomically swap in the new index. */
  async resolve(): Promise<void> {
    const suites = await this.discoverSuites();

    // Fetch every suite concurrently — sequential fetches over a full mirror's
    // suite list take ~20s wall, past the Worker's budget.
    const indexes = await Promise.all(
      suites.map(async (suite) => ({ suite, idx: await this.fetchRelease(suite) })),
    );
    console.log(
      "RESOLVE suites=",
      suites.length,
      "ok=",
      indexes.filter((x) => x.idx).length,
      "names=",
      JSON.stringify(suites),
    );

    const knownPaths = new Set<string>();
    const knownHashes = new Set<string>();
    const knownSuites = new Set<string>();
    const resolved: Release[] = [];
    let validUntil = Infinity;

    for (const { suite, idx } of indexes) {
      if (!idx) continue;
      resolved.push(idx);
      knownSuites.add(suite);

      const prefix = `dists/${suite}/`;
      // The signed index files themselves are always valid paths.
      for (const name of ["InRelease", "Release", "Release.gpg"]) {
        knownPaths.add(prefix + name);
      }
      // Every checksum block lists the same files; collecting all of them keeps
      // `by-hash/<algo>/<hash>` lookups working under any algorithm.
      for (const block of [idx.md5, idx.sha1, idx.sha256, idx.sha512]) {
        if (!block) continue;
        for (const file of block) {
          knownPaths.add(prefix + file.filename);
          knownHashes.add(file.hash.toLowerCase());
        }
      }

      const vu = idx.validUntil?.getTime() ?? Date.now() + DEFAULT_TTL_MS;
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
    this.knownSuites = knownSuites;
    this.suites = resolved;
    this.validUntil =
      validUntil === Infinity ? Date.now() + DEFAULT_TTL_MS : validUntil;
    this.state = "ready";
  }

  /**
   * Snapshot the mirror's health + file statistics. Before the first resolve
   * `aptstatus` is omitted (`health: "initializing"`); once `ready` it carries
   * the stats derived from the parsed suite indexes.
   */
  status(): AptMirrorStatus {
    if (this.state !== "ready") return { health: "initializing" };
    return {
      health: "healthy",
      aptstatus: {
        ...analyzeApt(this.suites),
        suites: this.suites.map(toSuiteMeta),
      },
    };
  }

  /**
   * Which suites to resolve. The curated `suites` option wins — the live
   * `dists/` listing on a full mirror enumerates 40+ suites (experimental,
   * rc-buggy, numbered aliases, …), and resolving them all in one invocation
   * blows the Worker's CPU/time budget, so the index never goes ready. The
   * listing is only a fallback when no suites are configured.
   */
  private async discoverSuites(): Promise<string[]> {
    if (this.configuredSuites.length) return this.configuredSuites;
    try {
      const entries = await this.fs.readdir("dists/");
      return entries
        .filter((e) => e.type === "directory")
        .map((e) => e.name);
    } catch {
      return [];
    }
  }

  /**
   * Fetch + parse a suite's `InRelease` (preferred) or `Release`. Any failure
   * — network throw, non-200, or parse error — is swallowed so one bad suite
   * never aborts the whole resolve (which would discard every other suite's
   * work and leave the index stuck `initializing`).
   */
  private async fetchRelease(suite: string): Promise<Release | null> {
    for (const name of ["InRelease", "Release"]) {
      try {
        // `cachedfetch` reads through the upstream cache so a re-resolve after
        // `Valid-Until` reuses an unchanged index instead of refetching.
        const res = await cachedfetch(this.url(`dists/${suite}/${name}`));
        if (!res.ok) continue;
        // `skipValidation` keeps real-world Release files (which vary in which
        // optional fields they carry) from throwing.
        return new Release(stripClearsign(await res.text()), {
          skipValidation: true,
        });
      } catch (e) {
        console.log(`FETCHREL ${suite}/${name} ERR:`, (e as Error)?.message);
        // try the next candidate, then give up on this suite
      }
    }
    return null;
  }

  /** Resolve a repo-relative path against the upstream base. */
  url(rel: string): string {
    return urlJoin(this.base, rel);
  }
}

/**
 * Strip an `InRelease` inline PGP clear-signature, returning the bare RFC822
 * body so `apt-parser` sees plain `Release` content. Unsigned text is returned
 * unchanged.
 */
function stripClearsign(text: string): string {
  if (!text.includes("BEGIN PGP SIGNED MESSAGE")) return text;
  // Body starts after the blank line following the armor + `Hash:` headers.
  const blank = text.indexOf("\n\n");
  if (blank === -1) return text;
  const body = text.slice(blank + 2);
  const sig = body.indexOf("-----BEGIN PGP SIGNATURE-----");
  return sig === -1 ? body : body.slice(0, sig);
}
