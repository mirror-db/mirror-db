/**
 * Builder for an APT metadata snapshot.
 *
 * One *generation* pins the `InRelease`/`Release` of every configured suite,
 * registers every file those releases reference, then downloads and parses only
 * the `Packages`/`Sources` indexes. Verified against the hashes and sizes the
 * pinned release declares; a `.gz` index is preferred over plain, `by-hash`
 * over the mutable path. Any failure, unsupported compression, path escape,
 * hash/size conflict or limit overflow discards the *whole* generation — the
 * previously published snapshot keeps serving, so a bad upstream can never
 * turn a populated directory into an empty one.
 *
 * No package body is ever fetched. Building a snapshot is read-only against
 * upstream metadata.
 */

import { Release } from "apt-parser";
import pLimit from "p-limit";

import { cachedfetch, ezfetch } from "@server/pkgs/fetch";
import {
  WebListError,
  WebListFs,
  type WebListFetch,
} from "@server/pkgs/web-list/web-list";
import type { WebListEntry } from "@server/pkgs/web-list/types";

import {
  AptConflictError,
  AptFileTable,
  AptGenerationError,
  AptLimitError,
  AptSnapshot,
  aptIndexEtag,
  buildAptIndexDocument,
  digestHex,
  normalizeSha1,
  normalizeSha256,
  packagesConsumer,
  parseIndexStream,
  resolveLimits,
  sourcesConsumer,
  type AptFileSpec,
  type AptSnapshotLimits,
  type AptSnapshotSuite,
} from "./metadata";
import { DEFAULT_TTL_MS, stripClearsign } from "./repo";
import type { AptRepository, AptRepositoryFacts } from "./repository";
import { analyzeApt, toSuiteMeta, type AptMirrorStatus } from "./status";

export type SnapshotState = "idle" | "building" | "ready";

/** A failed build is not retried for this long, to avoid hammering upstream. */
const RETRY_COOLDOWN_MS = 60 * 1000;

/**
 * Index groups processed at once. Each one buffers its compressed body while it
 * decompresses and parses, so this is a memory bound as much as a politeness
 * bound: `maxCompressedBytes` × this must stay modest.
 */
const GROUP_CONCURRENCY = 4;

/** A release answer that proves the suite does not exist. */
const ABSENT_STATUSES = new Set([403, 404, 410]);

/** What one generation actually fetched and parsed, after content dedupe. */
export interface AptBuildStats {
  /** Index groups considered (one chosen representation per directory/kind). */
  planned: number;
  /** Groups actually downloaded, decompressed and parsed. */
  parsed: number;
  /** Groups skipped because identical content was already parsed. */
  deduped: number;
  /** Decompressed bytes actually parsed (deduplicated). */
  parsedBytes: number;
}

export interface AptSnapshotIndexOptions {
  /** Mirror name, echoed in the index document. */
  mirror: string;
  /** Upstream base, trailing `/` optional. */
  base: string;
  /**
   * Suites to pin. Never empty for a real mount: these upstreams serve no
   * directory listing, so a suite list cannot be discovered at runtime.
   */
  suites: string[];
  /** Injectable fetch (tests); defaults to the shared caching helpers. */
  fetchImpl?: WebListFetch;
  /** Override the design limits (tests, or a mirror that measures larger). */
  limits?: Partial<AptSnapshotLimits>;
  /** Index groups processed at once (tests may lower it). */
  concurrency?: number;
  /** Clock override (tests); defaults to `Date.now`. */
  now?: () => number;
}

/** A `Packages`/`Sources` directory under one suite. */
interface IndexGroup {
  dir: string;
  kind: "packages" | "sources";
  plain?: string;
  gz?: string;
  unsupported: string[];
}

const INDEX_FILE_RE = /^(Packages|Sources)(?:\.([A-Za-z0-9]+))?$/;

const kindOf = (base: string): "packages" | "sources" =>
  base.toLowerCase().startsWith("packages") ? "packages" : "sources";

/**
 * Isolate-scoped snapshot holder. Mirrors `AptRepo`: one resolve in flight,
 * state flips only after a complete generation exists.
 */
export class AptSnapshotIndex implements AptRepository {
  readonly base: string;
  readonly mirror: string;
  readonly limits: AptSnapshotLimits;

  private state: SnapshotState = "idle";
  private snapshot: AptSnapshot | null = null;
  /** Last build failure, surfaced by the endpoint and the 502 listing. */
  lastError: string | null = null;
  /** Counters for the last successfully published generation. */
  lastStats: AptBuildStats | null = null;

  private readonly configuredSuites: string[];
  private readonly fetchImpl?: WebListFetch;
  private readonly groupLimit: ReturnType<typeof pLimit>;
  private readonly clock: () => number;
  private lastAttempt = 0;
  private inflight: Promise<void> | null = null;

  constructor(opts: AptSnapshotIndexOptions) {
    this.base = opts.base.endsWith("/") ? opts.base : `${opts.base}/`;
    this.mirror = opts.mirror;
    this.configuredSuites = opts.suites;
    this.fetchImpl = opts.fetchImpl;
    this.limits = resolveLimits(opts.limits);
    this.groupLimit = pLimit(
      Math.max(1, Math.min(opts.concurrency ?? GROUP_CONCURRENCY, GROUP_CONCURRENCY)),
    );
    this.clock = opts.now ?? Date.now;
  }

  /** Append a repo-relative path verbatim to the slash-terminated base. */
  url(rel: string): string {
    return this.base + rel;
  }

  /** True once a generation was published. */
  get ready(): boolean {
    return this.state === "ready" && this.snapshot !== null;
  }

  private isStale(): boolean {
    return this.snapshot !== null && this.clock() >= this.snapshot.expiresAt;
  }

  /**
   * Return the current generation, rebuilding when it is stale. A stale
   * snapshot is never withheld because the refresh failed — the old directory
   * keeps serving. Returns `null` only when no generation has ever succeeded.
   */
  async awaitSnapshot(): Promise<AptSnapshot | null> {
    if (this.snapshot && !this.isStale()) return this.snapshot;
    // A successful build whose freshness already elapsed (e.g. a past
    // `Valid-Until`) must not trigger a rebuild on every request.
    if (!this.inflight && this.clock() - this.lastAttempt < RETRY_COOLDOWN_MS) {
      return this.snapshot;
    }
    await this.kick();
    return this.snapshot;
  }

  /** Return the facts atomically published with the current generation. */
  async facts(): Promise<AptRepositoryFacts | null> {
    return (await this.awaitSnapshot())?.repositoryFacts ?? null;
  }

  /** Start a build (or join the running one). Never rejects. */
  private kick(): Promise<void> {
    if (this.inflight) return this.inflight;
    this.lastAttempt = this.clock();
    if (this.state === "idle") this.state = "building";

    this.inflight = this.build()
      .then((snapshot) => {
        this.snapshot = snapshot;
        this.lastError = null;
        this.state = "ready";
      })
      .catch((error) => {
        this.lastError = (error as Error)?.message ?? String(error);
        console.log(
          `APT SNAPSHOT FAIL mirror=${this.mirror} reason=${this.lastError}`,
        );
        this.state = this.snapshot ? "ready" : "idle";
      })
      .finally(() => {
        this.inflight = null;
      });
    return this.inflight;
  }

  /** Build one complete generation. Throws without publishing anything. */
  async build(): Promise<AptSnapshot> {
    const generatedAt = new Date(this.clock());
    const table = new AptFileTable(this.limits);
    const budget = new Budget(this.limits);
    const dedupe = new IndexDedupe();
    const knownPaths = new Set<string>();
    const knownHashes = new Set<string>();
    const suites: AptSnapshotSuite[] = [];
    const releases: Release[] = [];

    // Suites and, inside a suite, index groups are independent. Fetch them
    // concurrently so a repository with many components/architectures is
    // bounded by the shared outbound fetch gate. `AptFileTable.add` is
    // synchronous, so cross-suite merges and conflicts stay well-defined.
    // Index groups additionally pass through `groupLimit`.
    const results = await Promise.all(
      this.configuredSuites.map((suite) =>
        this.buildSuite(suite, table, budget, dedupe, knownPaths, knownHashes),
      ),
    );
    for (const result of results) {
      if (!result) continue;
      suites.push(result.suite);
      releases.push(result.release);
    }

    if (suites.length === 0) {
      throw new AptGenerationError("no suite release resolved");
    }

    const earliest = suites
      .map((suite) => suite.validUntil?.getTime())
      .filter((time): time is number => typeof time === "number" && !Number.isNaN(time))
      .reduce<number | null>((acc, time) => (acc === null || time < acc ? time : acc), null);
    const expiresAt = earliest ?? generatedAt.getTime() + DEFAULT_TTL_MS;

    const files = table.entries();
    const body = buildAptIndexDocument(
      this.mirror,
      generatedAt,
      expiresAt,
      suites,
      files,
    );
    // The validator covers the upstream metadata only: a rebuild of unchanged
    // content keeps the same ETag even though `generated_at`/`expires_at` move.
    const etag = await aptIndexEtag(this.mirror, suites, files);

    this.lastStats = dedupe.snapshot();
    // Keep only the lightweight health summary in the published generation;
    // parsed Releases and their checksum trees are build-local state.
    const status: AptMirrorStatus = {
      health: "healthy",
      aptstatus: {
        ...analyzeApt(releases),
        suites: releases.map(toSuiteMeta),
      },
    };

    return new AptSnapshot(
      this.mirror,
      generatedAt,
      expiresAt,
      suites,
      files,
      [...knownPaths],
      body,
      etag,
      [...knownHashes],
      status,
    );
  }

  /** Pin one suite's release, register its files, parse its indexes. */
  private async buildSuite(
    suite: string,
    table: AptFileTable,
    budget: Budget,
    dedupe: IndexDedupe,
    knownPaths: Set<string>,
    knownHashes: Set<string>,
  ): Promise<{ suite: AptSnapshotSuite; release: Release } | null> {
    const release = await this.fetchRelease(suite);
    if (!release) {
      console.log(`APT SNAPSHOT skip mirror=${this.mirror} suite=${suite}: no release`);
      return null;
    }

    const { parsed, releasePath, size, sha1, sha256 } = release;

    // The release file itself is a metadata object the browser should see. Its
    // sibling `Release`/`Release.gpg` are not listed: this generation has no
    // evidence they exist, and a directory must not invent files.
    table.add(
      releasePath,
      { size, ...(sha1 ? { sha1 } : {}), ...(sha256 ? { sha256 } : {}) },
      "metadata",
    );
    knownPaths.add(releasePath);

    // Register everything the release references, with whichever hashes that
    // block declares. Packages/Sources files are metadata too; only the
    // indexes below are downloaded.
    const blocks: Array<[string, Release["md5"]]> = [
      ["md5", parsed.md5],
      ["sha512", parsed.sha512],
      ["sha256", parsed.sha256],
      ["sha1", parsed.sha1],
    ];
    const groups = new Map<string, IndexGroup>();

    for (const [algorithm, block] of blocks) {
      if (!block) continue;
      for (const entry of block) {
        const path = `dists/${suite}/${entry.filename}`;
        const spec: AptFileSpec = { size: entry.size };
        const hash = algorithm === "sha1"
          ? normalizeSha1(entry.hash)
          : algorithm === "sha256"
            ? normalizeSha256(entry.hash)
            : undefined;
        if ((algorithm === "sha256" || algorithm === "sha1") && !hash) {
          // Without a usable digest this file could only be size-checked. The
          // release names one, so a malformed value is a broken index, not a
          // reason to download something unverifiable.
          throw new AptGenerationError(
            `release ${releasePath}: unusable ${algorithm} digest for ${path}`,
          );
        }
        if (hash) spec[algorithm as "sha1" | "sha256"] = hash;
        // by-hash may be addressed under any algorithm the release declares.
        const declaredHash = entry.hash.trim().toLowerCase();
        if (/^[0-9a-f]{32,128}$/.test(declaredHash)) knownHashes.add(declaredHash);

        table.add(path, spec, "metadata");
        knownPaths.add(path);

        const relative = path.slice(`dists/${suite}/`.length);
        const slash = relative.lastIndexOf("/");
        const base = slash === -1 ? relative : relative.slice(slash + 1);
        const dirname = slash === -1 ? "" : relative.slice(0, slash + 1);
        const match = INDEX_FILE_RE.exec(base);
        if (!match) continue;

        const kind = kindOf(match[1]!);
        const key = `${dirname}\u0000${kind}`;
        let group = groups.get(key);
        if (!group) {
          group = { dir: dirname, kind, unsupported: [] };
          groups.set(key, group);
        }
        if (match[2] === "gz") group.gz = base;
        else if (!match[2]) group.plain = base;
        else group.unsupported.push(base);
      }
    }

    // Index groups within one suite are independent, but each one buffers its
    // compressed body while it decompresses and parses, so they share the
    // instance-wide `groupLimit` rather than fanning out per suite.
    await Promise.all(
      [...groups.values()]
        .sort((a, b) => (a.dir < b.dir ? -1 : a.dir > b.dir ? 1 : 0))
        .map((group) => this.buildGroup(suite, group, table, budget, dedupe, parsed)),
    );

    const summary: AptSnapshotSuite = { suite, releasePath };
    if (parsed.date) summary.date = parsed.date;
    if (parsed.validUntil) summary.validUntil = parsed.validUntil;
    if (sha256) summary.sha256 = sha256;
    if (sha1) summary.sha1 = sha1;
    return { suite: summary, release: parsed };
  }

  /**
   * Download, verify and parse one `Packages`/`Sources` group.
   *
   * Identical index content is parsed once per generation: the Release-declared
   * digest + size (SHA-256, else SHA-1) is the identity, so a file repeated
   * under several suites or components costs one download and one parse. The
   * metadata table still lists every path the Release named.
   */
  private async buildGroup(
    suite: string,
    group: IndexGroup,
    table: AptFileTable,
    budget: Budget,
    dedupe: IndexDedupe,
    release: Release,
  ): Promise<void> {
    // Prefer the compressed representation; a declared `.gz` that cannot be
    // fetched falls back to the plain copy in the same directory.
    const candidates = [group.gz, group.plain].filter(
      (name): name is string => typeof name === "string",
    );
    if (candidates.length === 0) {
      throw new AptGenerationError(
        `unsupported index compression in dists/${suite}/${group.dir} (${group.kind}): ` +
          group.unsupported.join(", "),
      );
    }

    const primaryPath = `dists/${suite}/${group.dir}${candidates[0]}`;
    const primary = table.get(primaryPath);
    if (!primary) {
      throw new AptGenerationError(`index ${primaryPath} is not declared by the release`);
    }
    const identity = dedupe.identityOf(group.kind, primary);

    await dedupe.run(identity, (recordBytes) =>
      this.groupLimit(async () => {
        // Each candidate is fetched, verified **and** parsed inside the slot: the
        // limiter must bound the whole download/decompress/parse unit, not just
        // the response headers.
        let lastError = "no attempt";
        for (const name of candidates) {
          const path = `dists/${suite}/${group.dir}${name}`;
          const declared = table.get(path);
          if (!declared) {
            throw new AptGenerationError(`index ${path} is not declared by the release`);
          }

          let bytes: Uint8Array;
          try {
            bytes = await this.fetchIndex(suite, path, declared, release);
          } catch (error) {
            if (error instanceof AptLimitError || error instanceof AptConflictError) {
              throw error;
            }
            lastError = (error as Error)?.message ?? String(error);
            continue;
          }

          try {
            await parseIndexStream(
              bytes,
              name.endsWith(".gz"),
              this.limits,
              (count) => {
                budget.add(count);
                recordBytes(count);
              },
              group.kind === "packages" ? packagesConsumer(table) : sourcesConsumer(table),
            );
            return;
          } catch (error) {
            if (error instanceof AptLimitError || error instanceof AptConflictError) {
              throw error;
            }
            // The bytes matched the declared hash but do not decode (a truncated
            // gzip member, say). A declared plain copy in the same directory is
            // a legitimate second attempt.
            lastError = (error as Error)?.message ?? String(error);
          }
        }
        throw new AptGenerationError(
          `index dists/${suite}/${group.dir} (${group.kind}) failed: ${lastError}`,
        );
      }),
    );
  }

  /**
   * Download one index, preferring the content-addressed `by-hash` location and
   * falling back to the mutable path. Every attempt is checked against the size
   * and hashes the pinned release declares.
   */
  private async fetchIndex(
    suite: string,
    path: string,
    declared: { size: number; sha1?: string; sha256?: string },
    release: Release,
  ): Promise<Uint8Array> {
    const relative = path.slice(`dists/${suite}/`.length);
    const slash = relative.lastIndexOf("/");
    const dirname = slash === -1 ? "" : relative.slice(0, slash + 1);

    const attempts: Array<{ url: string; cached: boolean }> = [];
    if (release.acquireByHash) {
      if (declared.sha256) {
        attempts.push({
          url: `${this.base}dists/${suite}/${dirname}by-hash/SHA256/${declared.sha256}`,
          cached: true,
        });
      } else if (declared.sha1) {
        attempts.push({
          url: `${this.base}dists/${suite}/${dirname}by-hash/SHA1/${declared.sha1}`,
          cached: true,
        });
      }
    }
    attempts.push({ url: `${this.base}${path}`, cached: false });

    let lastError = "no attempt";
    for (const attempt of attempts) {
      try {
        const res = await this.fetchUrl(attempt.url, attempt.cached);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const bytes = await readCapped(res, this.limits.maxCompressedBytes);
        await verifyIndex(bytes, declared, path);
        return bytes;
      } catch (error) {
        lastError = (error as Error)?.message ?? String(error);
        if (error instanceof AptLimitError || error instanceof AptConflictError) {
          throw error;
        }
      }
    }
    throw new AptGenerationError(`index ${path} failed: ${lastError}`);
  }

  /**
   * Fetch and parse a suite's `InRelease` (preferred) or `Release`.
   *
   * Returns `null` **only** when both names are absent (`403`/`404`/`410`): that
   * proves the configured suite does not exist upstream, so it is skipped and
   * the rest of the generation still stands. Any other outcome — a network
   * error, a `5xx`, a body that is not a `Release` (HTML from a captive portal,
   * an empty body), a parse failure, or a release without a suite name or a
   * usable checksum block — rejects the whole generation. Treating those as
   * "suite absent" would publish a nearly empty index while the old one was
   * fine.
   */
  private async fetchRelease(
    suite: string,
  ): Promise<{
    parsed: Release;
    releasePath: string;
    size: number;
    sha1?: string;
    sha256?: string;
  } | null> {
    for (const name of ["InRelease", "Release"]) {
      const path = `dists/${suite}/${name}`;

      let res: Response;
      try {
        res = await this.fetchUrl(`${this.base}${path}`, false);
      } catch (error) {
        throw new AptGenerationError(
          `release ${path} fetch failed: ${(error as Error)?.message ?? String(error)}`,
        );
      }
      if (ABSENT_STATUSES.has(res.status)) continue;
      if (!res.ok) {
        throw new AptGenerationError(`release ${path} answered HTTP ${res.status}`);
      }

      const bytes = await readCapped(res, this.limits.maxCompressedBytes);
      const parsed = parseRelease(bytes, path);

      return {
        parsed,
        releasePath: path,
        size: bytes.byteLength,
        sha1: await digestHex("SHA-1", bytes),
        sha256: await digestHex("SHA-256", bytes),
      };
    }
    return null;
  }

  private fetchUrl(url: string, cached: boolean): Promise<Response> {
    if (this.fetchImpl) return this.fetchImpl(url);
    return cached ? cachedfetch(url) : ezfetch(url);
  }
}

// ── Helpers ─────────────────────────────────────────────────────────────────

/** Shared decompressed-byte budget for one generation. */
class Budget {
  private used = 0;

  constructor(private readonly limits: AptSnapshotLimits) {}

  add(count: number): void {
    this.used += count;
    if (this.used > this.limits.maxTotalBytes) {
      throw new AptLimitError(
        `generation exceeds ${this.limits.maxTotalBytes} decompressed bytes`,
      );
    }
  }
}

/**
 * Per-generation content dedupe for `Packages`/`Sources`.
 *
 * Two release-declared paths with the same canonical digest and size describe
 * the same bytes; the first task downloads and parses it, every later one joins
 * that promise. The same digest declared with a different size is contradictory
 * and rejects the generation.
 *
 * The kind is part of the key only to keep a pathological case honest: a file
 * classified as `Packages` and a byte-identical one classified as `Sources`
 * must not share a parse, because the consumers differ.
 */
class IndexDedupe {
  private readonly inflight = new Map<string, Promise<void>>();
  private readonly sizes = new Map<string, number>();
  private readonly stats: AptBuildStats = {
    planned: 0,
    parsed: 0,
    deduped: 0,
    parsedBytes: 0,
  };

  snapshot(): AptBuildStats {
    return { ...this.stats };
  }

  /** Canonical identity of a declared index file, or `null` if it has no hash. */
  identityOf(
    kind: "packages" | "sources",
    file: { size: number; sha1?: string; sha256?: string },
  ): string | null {
    const algorithm = file.sha256 ? "sha256" : file.sha1 ? "sha1" : null;
    if (!algorithm) return null;
    const hash = algorithm === "sha256" ? file.sha256 : file.sha1;
    const digest = `${algorithm}:${hash}`;

    const previous = this.sizes.get(digest);
    if (previous !== undefined && previous !== file.size) {
      throw new AptConflictError(
        `digest ${digest} declared with sizes ${previous} and ${file.size}`,
      );
    }
    this.sizes.set(digest, file.size);
    return `${kind}:${digest}:${file.size}`;
  }

  /**
   * Run `task` unless identical content is already being parsed. Decompressed
   * bytes are accounted once, through the `record` callback the task calls.
   */
  async run(
    identity: string | null,
    task: (record: (count: number) => void) => Promise<void>,
  ): Promise<void> {
    this.stats.planned += 1;

    if (identity !== null) {
      const existing = this.inflight.get(identity);
      if (existing) {
        this.stats.deduped += 1;
        await existing;
        return;
      }
    }

    this.stats.parsed += 1;
    let bytes = 0;
    const promise = task((count) => {
      bytes += count;
    }).then(() => {
      this.stats.parsedBytes += bytes;
    });
    if (identity !== null) this.inflight.set(identity, promise);
    await promise;
  }
}

/** Read a response body into memory, refusing anything above `cap`. */
async function readCapped(res: Response, cap: number): Promise<Uint8Array> {
  const declaredHeader = res.headers.get("content-length");
  if (declaredHeader !== null) {
    const declared = Number(declaredHeader);
    if (Number.isFinite(declared) && declared > cap) {
      throw new AptLimitError(`response of ${declared} bytes exceeds ${cap}`);
    }
  }

  const body = res.body;
  if (!body) return new Uint8Array(0);

  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > cap) {
      await reader.cancel().catch(() => {});
      throw new AptLimitError(`response exceeds ${cap} bytes`);
    }
    chunks.push(value);
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

/** Check a downloaded index against the size and hashes the release declares. */
async function verifyIndex(
  bytes: Uint8Array,
  declared: { size: number; sha1?: string; sha256?: string },
  path: string,
): Promise<void> {
  if (bytes.byteLength !== declared.size) {
    throw new AptGenerationError(
      `size mismatch for ${path}: ${bytes.byteLength} != ${declared.size}`,
    );
  }
  if (declared.sha256) {
    const actual = await digestHex("SHA-256", bytes);
    if (actual !== declared.sha256) {
      throw new AptGenerationError(`sha256 mismatch for ${path}`);
    }
  }
  if (declared.sha1) {
    const actual = await digestHex("SHA-1", bytes);
    if (actual !== declared.sha1) {
      throw new AptGenerationError(`sha1 mismatch for ${path}`);
    }
  }
}

/**
 * Parse a downloaded release, refusing anything that is not a usable one.
 *
 * `skipValidation` keeps real-world releases (they vary in optional fields) from
 * throwing, so the two facts this project relies on are checked here instead:
 * the release must name its suite and declare at least one checksum block. An
 * HTML error page or an empty body satisfies neither.
 */
function parseRelease(bytes: Uint8Array, path: string): Release {
  const text = new TextDecoder().decode(bytes);
  if (!text.trim()) {
    throw new AptGenerationError(`release ${path} is empty`);
  }

  let parsed: Release;
  try {
    parsed = new Release(stripClearsign(text), { skipValidation: true });
  } catch (error) {
    throw new AptGenerationError(
      `release ${path} parse failed: ${(error as Error)?.message ?? String(error)}`,
    );
  }

  if (!parsed.suite && !parsed.codename) {
    throw new AptGenerationError(`release ${path} names no suite or codename`);
  }
  const checksums = (parsed.sha256?.length ?? 0) + (parsed.sha1?.length ?? 0);
  if (checksums === 0) {
    throw new AptGenerationError(`release ${path} declares no SHA256/SHA1 checksums`);
  }
  return parsed;
}

/**
 * Directory source for `WebListProxy`: listings come from the snapshot, file
 * bodies still stream from upstream. A missing snapshot (no generation ever
 * succeeded) is an explicit failure, never an empty listing.
 */
export class SnapshotListSource {
  constructor(
    private readonly index: AptSnapshotIndex,
    private readonly upstream: WebListFs,
  ) {}

  async readdir(rel = ""): Promise<WebListEntry[]> {
    const snapshot = await this.index.awaitSnapshot();
    if (!snapshot) {
      throw new WebListError(
        `apt index for ${this.index.mirror} unavailable: ${this.index.lastError ?? "not built"}`,
        this.index.lastError ? 502 : 404,
      );
    }
    return snapshot.readdir(rel);
  }

  fetch(rel = "", init?: RequestInit): Promise<Response> {
    return this.upstream.fetch(rel, init);
  }
}
