/**
 * APT metadata snapshot: the normalised index the Worker can synthesise from a
 * repository's own metadata, plus the fixed `/.mdb/apt-index.json` document.
 *
 * An APT repository that serves no directory listing still describes itself:
 * `Release` lists every metadata file with size + hashes, `Packages` lists the
 * `.deb` files, `Sources` lists the source files. This module turns that into
 * one coherent table of repo-root-relative paths, and synthesises directory
 * listings from it. Package bodies are never downloaded.
 *
 * See `snapshot.ts` for the builder that fetches and verifies the metadata.
 */

import { WebListError } from "@server/pkgs/web-list/web-list";
import type { WebListEntry } from "@server/pkgs/web-list/types";

import type { AptRepositoryFacts } from "./repository";
import type { AptMirrorStatus } from "./status";
import { StanzaStreamParser, type Stanza } from "./stanza";

// ── Limits ──────────────────────────────────────────────────────────────────

export interface AptSnapshotLimits {
  /** Decompressed bytes of a single `Packages`/`Sources` index. */
  maxFileBytes: number;
  /** Decompressed bytes of all indexes in one generation. */
  maxTotalBytes: number;
  /** Distinct file paths retained in one generation. */
  maxFiles: number;
  /**
   * Compressed bytes buffered for one index before decompression. A guard
   * against a hostile upstream; a real index compresses well below this.
   */
  maxCompressedBytes: number;
}

const MiB = 1024 * 1024;

/**
 * Defaults from the design: 8 MiB per index, 64 MiB per generation, 50k files.
 *
 * The compressed cap matches the decompressed one. Four index groups may be in
 * flight at once, so the worst case buffers 4 × 8 MiB compressed plus the
 * decompressed chunks accounted against the generation budget. A real
 * `Packages.gz` is well under 1 MiB.
 */
export const DefaultAptSnapshotLimits: AptSnapshotLimits = {
  maxFileBytes: 8 * MiB,
  maxTotalBytes: 64 * MiB,
  maxFiles: 50_000,
  maxCompressedBytes: 8 * MiB,
};

export function resolveLimits(limits?: Partial<AptSnapshotLimits>): AptSnapshotLimits {
  return { ...DefaultAptSnapshotLimits, ...limits };
}

// ── Errors ──────────────────────────────────────────────────────────────────

/** The whole generation must be discarded; the previous one keeps serving. */
export class AptGenerationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AptGenerationError";
  }
}

export class AptLimitError extends AptGenerationError {
  constructor(message: string) {
    super(message);
    this.name = "AptLimitError";
  }
}

export class AptConflictError extends AptGenerationError {
  constructor(message: string) {
    super(message);
    this.name = "AptConflictError";
  }
}

// ── Paths ───────────────────────────────────────────────────────────────────

const SCHEME_RE = /^[a-z][a-z0-9+.-]*:/i;
const CONTROL_RE = /[\u0000-\u001f\u007f]/;

/**
 * Validate a repository-relative path from `Release`, `Packages.Filename` or
 * `Sources.Directory` + file name.
 *
 * The path is used for directory synthesis and as a relay hash expectation, so
 * anything that could resolve outside the repository root — an absolute path, a
 * `..` segment, a backslash (a separator on Windows WebDAV clients), a scheme,
 * a control character, a `?`/`#` (which would change the URL's structure), or a
 * percent-encoded form of any of those — rejects the generation instead of
 * being listed.
 */
export function normalizeRepoPath(raw: string): string {
  const path = raw.trim();
  if (!path) throw new AptGenerationError("empty path");
  if (CONTROL_RE.test(path)) throw new AptGenerationError(`control character in path: ${JSON.stringify(raw)}`);
  if (path.startsWith("/")) throw new AptGenerationError(`absolute path: ${path}`);
  if (path.includes("\\")) throw new AptGenerationError(`backslash in path: ${path}`);
  if (SCHEME_RE.test(path)) throw new AptGenerationError(`scheme in path: ${path}`);
  if (path.includes("?") || path.includes("#")) {
    throw new AptGenerationError(`query or fragment in path: ${path}`);
  }

  checkSegments(path);

  // A percent escape can hide a separator or a dot segment from the checks
  // above (`%2e%2e%2f`). The name is also used verbatim as a URL path and as a
  // relay cache key, so the decoded form must stay equally safe.
  if (path.includes("%")) {
    let decoded: string;
    try {
      decoded = decodeURIComponent(path);
    } catch {
      throw new AptGenerationError(`invalid percent-encoding in path: ${path}`);
    }
    if (decoded !== path) {
      if (decoded.startsWith("/") || decoded.includes("\\")) {
        throw new AptGenerationError(`encoded separator in path: ${path}`);
      }
      // An encoded `/` would invent a directory level the raw path does not have.
      if (decoded.split("/").length !== path.split("/").length) {
        throw new AptGenerationError(`encoded separator in path: ${path}`);
      }
      if (/[\u0000-\u001f\u007f?#]/.test(decoded)) {
        throw new AptGenerationError(`encoded control character in path: ${path}`);
      }
      checkSegments(decoded, path);
    }
  }

  return path;
}

/** Reject empty, `.` and `..` segments. */
function checkSegments(path: string, raw = path): void {
  for (const segment of path.split("/")) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new AptGenerationError(`unsafe path segment in: ${raw}`);
    }
  }
}

// ── Hashes ──────────────────────────────────────────────────────────────────

const SHA1_RE = /^[0-9a-f]{40}$/;
const SHA256_RE = /^[0-9a-f]{64}$/;

/** Lower-case a hex digest when it has the expected shape, else `undefined`. */
function normalizeHash(value: string | undefined, re: RegExp): string | undefined {
  if (!value) return undefined;
  const hash = value.trim().toLowerCase();
  return re.test(hash) ? hash : undefined;
}

export const normalizeSha1 = (value?: string): string | undefined => normalizeHash(value, SHA1_RE);
export const normalizeSha256 = (value?: string): string | undefined => normalizeHash(value, SHA256_RE);

export async function digestHex(
  algorithm: "SHA-1" | "SHA-256",
  bytes: Uint8Array,
): Promise<string> {
  const digest = await crypto.subtle.digest(algorithm, bytes);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

// ── File table ──────────────────────────────────────────────────────────────

export type AptFileKind = "metadata" | "package";

export interface AptSnapshotFile {
  /** Repo-root-relative path, no leading slash. */
  path: string;
  size: number;
  sha1?: string;
  sha256?: string;
  kind: AptFileKind;
}

export interface AptFileSpec {
  size: number;
  sha1?: string;
  sha256?: string;
}

/**
 * Deduplicated path → size/hash table for one generation.
 *
 * The same path legitimately appears in several suites (a shared `pool/` entry)
 * or several checksum blocks. Equal values merge, a missing hash is filled in,
 * and a differing size or digest is a hard conflict: the generation could not
 * describe one file consistently, so it is rejected rather than guessing.
 */
export class AptFileTable {
  private readonly files = new Map<string, AptSnapshotFile>();

  constructor(private readonly limits: AptSnapshotLimits) {}

  get size(): number {
    return this.files.size;
  }

  add(rawPath: string, spec: AptFileSpec, kind: AptFileKind): void {
    const path = normalizeRepoPath(rawPath);
    if (!Number.isSafeInteger(spec.size) || spec.size < 0) {
      throw new AptGenerationError(`invalid size for ${path}: ${spec.size}`);
    }

    const existing = this.files.get(path);
    if (!existing) {
      if (this.files.size >= this.limits.maxFiles) {
        throw new AptLimitError(
          `too many files: ${this.files.size + 1} > ${this.limits.maxFiles}`,
        );
      }
      const file: AptSnapshotFile = { path, size: spec.size, kind };
      if (spec.sha1) file.sha1 = spec.sha1;
      if (spec.sha256) file.sha256 = spec.sha256;
      this.files.set(path, file);
      return;
    }

    if (existing.kind !== kind) {
      throw new AptConflictError(
        `kind conflict for ${path}: ${existing.kind} vs ${kind}`,
      );
    }
    if (existing.size !== spec.size) {
      throw new AptConflictError(
        `size conflict for ${path}: ${existing.size} vs ${spec.size}`,
      );
    }
    if (spec.sha1) {
      if (existing.sha1 && existing.sha1 !== spec.sha1) {
        throw new AptConflictError(`sha1 conflict for ${path}`);
      }
      existing.sha1 = spec.sha1;
    }
    if (spec.sha256) {
      if (existing.sha256 && existing.sha256 !== spec.sha256) {
        throw new AptConflictError(`sha256 conflict for ${path}`);
      }
      existing.sha256 = spec.sha256;
    }
  }

  has(path: string): boolean {
    return this.files.has(path);
  }

  get(path: string): AptSnapshotFile | undefined {
    return this.files.get(path);
  }

  /** Entries sorted by path, so the serialised document is deterministic. */
  entries(): AptSnapshotFile[] {
    return [...this.files.values()].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  }
}

// ── Stanza consumers ────────────────────────────────────────────────────────

function parseSize(raw: string | undefined): number | null {
  if (!raw) return null;
  const text = raw.trim();
  if (!/^\d+$/.test(text)) return null;
  const size = Number(text);
  return Number.isSafeInteger(size) ? size : null;
}

/**
 * `Packages` stanza → one package file entry.
 *
 * `Filename` is repo-root-relative (never joined with suite or component).
 * A stanza without a usable `Filename`/`Size` cannot be listed and is skipped;
 * a path that escapes the repository rejects the generation.
 */
export function packagesConsumer(
  table: AptFileTable,
): (stanza: Stanza) => void {
  return (stanza) => {
    const filename = stanza.get("filename");
    if (!filename) return;
    const size = parseSize(stanza.get("size"));
    if (size === null) return;
    table.add(
      filename,
      {
        size,
        sha1: normalizeSha1(stanza.get("sha1")),
        sha256: normalizeSha256(stanza.get("sha256")),
      },
      "package",
    );
  };
}

interface SourceFileRecord {
  size?: number;
  sha1?: string;
  sha256?: string;
}

/** `Checksums-*`/`Files` blocks: whitespace-separated `hash size name` lines. */
function collectSourceBlock(
  stanza: Stanza,
  field: string,
  apply: (name: string, size: number, hash: string) => void,
): void {
  const block = stanza.get(field);
  if (!block) return;
  for (const line of block.split("\n")) {
    const parts = line.trim().split(/\s+/);
    if (parts.length < 3) continue;
    const size = parseSize(parts[1]);
    if (size === null) continue;
    apply(parts[2]!, size, parts[0]!);
  }
}

/**
 * `Sources` stanza → source file entries.
 *
 * `Directory` plus each name in `Files` / `Checksums-Sha1` / `Checksums-Sha256`
 * gives the repo-root-relative path; the three blocks are merged per name so a
 * file ends up with size + SHA-1 + SHA-256 from whichever blocks declare it.
 */
export function sourcesConsumer(
  table: AptFileTable,
): (stanza: Stanza) => void {
  return (stanza) => {
    const rawDirectory = stanza.get("directory");
    if (!rawDirectory) return;
    // `Directory` is repository-relative; a trailing slash is common and would
    // otherwise produce `pool/x//name`, which normalizeRepoPath rejects.
    const directory = rawDirectory.trim().replace(/\/+$/, "");
    if (!directory) return;

    const records = new Map<string, SourceFileRecord>();
    const record = (name: string): SourceFileRecord => {
      let rec = records.get(name);
      if (!rec) {
        rec = {};
        records.set(name, rec);
      }
      return rec;
    };
    const applySize = (name: string, size: number, path: string): void => {
      const rec = record(name);
      if (rec.size !== undefined && rec.size !== size) {
        throw new AptConflictError(`size conflict for ${path}: ${rec.size} vs ${size}`);
      }
      rec.size = size;
    };

    collectSourceBlock(stanza, "files", (name, size) => applySize(name, size, `${directory}/${name}`));
    collectSourceBlock(stanza, "checksums-sha1", (name, size, hash) => {
      const sha1 = normalizeSha1(hash);
      if (!sha1) return;
      applySize(name, size, `${directory}/${name}`);
      const rec = record(name);
      if (rec.sha1 && rec.sha1 !== sha1) {
        throw new AptConflictError(`sha1 conflict for ${directory}/${name}`);
      }
      rec.sha1 = sha1;
    });
    collectSourceBlock(stanza, "checksums-sha256", (name, size, hash) => {
      const sha256 = normalizeSha256(hash);
      if (!sha256) return;
      applySize(name, size, `${directory}/${name}`);
      const rec = record(name);
      if (rec.sha256 && rec.sha256 !== sha256) {
        throw new AptConflictError(`sha256 conflict for ${directory}/${name}`);
      }
      rec.sha256 = sha256;
    });

    for (const [name, rec] of records) {
      if (rec.size === undefined) continue;
      const spec: AptFileSpec = { size: rec.size };
      if (rec.sha1) spec.sha1 = rec.sha1;
      if (rec.sha256) spec.sha256 = rec.sha256;
      table.add(`${directory}/${name}`, spec, "package");
    }
  };
}

// ── Streaming decode ────────────────────────────────────────────────────────

/**
 * Decode an index body (optionally gzip) into stanzas while enforcing the
 * decompressed-size cap. `onDecodedBytes` accounts the shared per-generation
 * budget; it may throw to abort the build.
 */
export async function parseIndexStream(
  bytes: Uint8Array,
  gzip: boolean,
  limits: AptSnapshotLimits,
  onDecodedBytes: (count: number) => void,
  onStanza: (stanza: Stanza) => void,
): Promise<void> {
  // A zero-byte index is a legal empty index. For a `.gz` that means the file
  // itself is empty, not a valid gzip stream — do not hand it to the decoder.
  const body = new Response(bytes).body!;
  const reader = (gzip && bytes.byteLength > 0
    ? body.pipeThrough(new DecompressionStream("gzip"))
    : body
  ).getReader();

  const decoder = new TextDecoder();
  const parser = new StanzaStreamParser(onStanza);
  let decoded = 0;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      decoded += value.byteLength;
      if (decoded > limits.maxFileBytes) {
        await reader.cancel().catch(() => {});
        throw new AptLimitError(
          `index exceeds ${limits.maxFileBytes} bytes after decompression`,
        );
      }
      onDecodedBytes(value.byteLength);
      parser.push(decoder.decode(value, { stream: true }));
    }
    parser.push(decoder.decode());
    parser.end();
  } catch (e) {
    if (e instanceof AptGenerationError) throw e;
    throw new AptGenerationError(`index decode failed: ${(e as Error)?.message ?? String(e)}`);
  }
}

// ── Snapshot ────────────────────────────────────────────────────────────────

export interface AptSnapshotSuite {
  suite: string;
  /** Path of the file actually fetched, e.g. `dists/bookworm/InRelease`. */
  releasePath: string;
  date?: Date;
  validUntil?: Date;
  sha1?: string;
  sha256?: string;
}

/**
 * One immutable generation. Publishing is a single reference swap in
 * `AptSnapshotIndex`: readers see either the old generation or the new one.
 */
export class AptSnapshot {
  private readonly dirCache = new Map<string, WebListEntry[]>();
  private readonly sizes = new Map<string, number>();
  private readonly tree: string[];
  /** Every path this generation can answer for (declared files + release names). */
  readonly knownPaths: ReadonlySet<string>;
  /** Every digest named by a pinned release, any algorithm, lower-case hex. */
  readonly knownHashes: ReadonlySet<string>;
  readonly contentLength: number;
  /** Facts shared by proxy, listing and status consumers of this generation. */
  readonly repositoryFacts: AptRepositoryFacts;

  constructor(
    readonly mirror: string,
    readonly generatedAt: Date,
    readonly expiresAt: number,
    readonly suites: readonly AptSnapshotSuite[],
    readonly files: readonly AptSnapshotFile[],
    knownPaths: readonly string[],
    readonly body: string,
    readonly etag: string,
    knownHashes: readonly string[] = [],
    status: AptMirrorStatus = { health: "healthy" },
  ) {
    for (const file of files) this.sizes.set(file.path, file.size);
    this.tree = [...new Set([...files.map((f) => f.path), ...knownPaths])].sort();
    this.knownPaths = new Set(this.tree);
    this.knownHashes = new Set(knownHashes);
    this.contentLength = new TextEncoder().encode(body).byteLength;
    this.repositoryFacts = {
      paths: this.knownPaths,
      hashes: this.knownHashes,
      suites: new Set(suites.map((suite) => suite.suite)),
      validUntil: expiresAt,
      coverage: "tree",
      status,
    };
  }

  /** True when any known path sits under `prefix` (i.e. `prefix` is a directory). */
  hasChildren(prefix: string): boolean {
    for (const path of this.tree) if (path.startsWith(prefix)) return true;
    return false;
  }

  /**
   * Synthesise a directory listing from the file paths. Missing directories
   * throw a 404 `WebListError` — a failed or partial index must never render as
   * an empty directory.
   */
  readdir(rel = ""): WebListEntry[] {
    const dir = rel === "" ? "" : rel.endsWith("/") ? rel : `${rel}/`;
    const cached = this.dirCache.get(dir);
    if (cached) return cached;

    // name → isDirectory; a child that is both wins as a directory.
    const children = new Map<string, boolean>();
    for (const path of this.tree) {
      if (!path.startsWith(dir)) continue;
      const rest = path.slice(dir.length);
      if (!rest) continue;
      const slash = rest.indexOf("/");
      const name = slash === -1 ? rest : rest.slice(0, slash);
      const isDir = slash !== -1;
      if (isDir || children.get(name) !== true) children.set(name, isDir);
    }

    if (children.size === 0 && dir !== "") {
      throw new WebListError(`no such directory: ${rel}`, 404);
    }

    const entries: WebListEntry[] = [];
    for (const [name, isDir] of children) {
      entries.push({
        name,
        href: isDir ? `${name}/` : name,
        type: isDir ? "directory" : "file",
        lastModified: null,
        size: isDir ? null : this.sizes.get(dir + name) ?? null,
      });
    }
    entries.sort((a, b) => {
      if (a.type !== b.type) return a.type === "directory" ? -1 : 1;
      return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
    });

    this.dirCache.set(dir, entries);
    return entries;
  }
}

// ── Fixed endpoint document ─────────────────────────────────────────────────

function iso(epochMs: number): string {
  return new Date(epochMs).toISOString();
}

function isoOrUndefined(date: Date | undefined): string | undefined {
  return date && !Number.isNaN(date.getTime()) ? date.toISOString() : undefined;
}

/**
 * Serialise the generation to the `schema: 1` document. Key order is fixed and
 * both arrays are sorted, so the same metadata always yields the same bytes.
 *
 * The ETag deliberately hashes {@link buildAptIndexStable} instead: the body's
 * `generated_at`/`expires_at` move on every rebuild, the validator must not.
 */
export function buildAptIndexDocument(
  mirror: string,
  generatedAt: Date,
  expiresAt: number,
  suites: readonly AptSnapshotSuite[],
  files: readonly AptSnapshotFile[],
): string {
  return JSON.stringify({
    schema: 1,
    mirror,
    generated_at: iso(generatedAt.getTime()),
    expires_at: iso(expiresAt),
    suites: indexSuites(suites),
    files: indexFiles(files),
  });
}

/**
 * The stable part of the document: the upstream metadata without the times this
 * generation happened to be built at.
 *
 * The ETag hashes this, not {@link buildAptIndexDocument}: re-reading an
 * unchanged upstream must keep the validator, otherwise every rebuild would
 * invalidate every relay's copy for no content change. `generated_at` and
 * `expires_at` stay in the served body.
 */
export function buildAptIndexStable(
  mirror: string,
  suites: readonly AptSnapshotSuite[],
  files: readonly AptSnapshotFile[],
): string {
  return JSON.stringify({
    schema: 1,
    mirror,
    suites: indexSuites(suites),
    files: indexFiles(files),
  });
}

/** Strong ETag over {@link buildAptIndexStable}. */
export async function aptIndexEtag(
  mirror: string,
  suites: readonly AptSnapshotSuite[],
  files: readonly AptSnapshotFile[],
): Promise<string> {
  const stable = buildAptIndexStable(mirror, suites, files);
  return `"${await digestHex("SHA-256", new TextEncoder().encode(stable))}"`;
}

function indexSuites(suites: readonly AptSnapshotSuite[]) {
  return [...suites]
    .sort((a, b) => (a.suite < b.suite ? -1 : a.suite > b.suite ? 1 : 0))
    .map((suite) => {
      const date = isoOrUndefined(suite.date);
      const validUntil = isoOrUndefined(suite.validUntil);
      return {
        suite: suite.suite,
        release_path: suite.releasePath,
        ...(date ? { date } : {}),
        ...(validUntil ? { valid_until: validUntil } : {}),
        ...(suite.sha256 ? { sha256: suite.sha256 } : {}),
        ...(suite.sha1 ? { sha1: suite.sha1 } : {}),
      };
    });
}

function indexFiles(files: readonly AptSnapshotFile[]) {
  return [...files]
    .sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0))
    .map((file) => ({
      path: file.path,
      size: file.size,
      ...(file.sha256 ? { sha256: file.sha256 } : {}),
      ...(file.sha1 ? { sha1: file.sha1 } : {}),
      kind: file.kind,
    }));
}

function ifNoneMatch(header: string | null, etag: string): boolean {
  if (!header) return false;
  return header.split(",").some((raw) => {
    const value = raw.trim();
    return value === "*" || value === etag || value === `W/${etag}`;
  });
}

/** `GET`/`HEAD` answer for `/<mount>/.mdb/apt-index.json`. */
export function aptIndexResponse(snapshot: AptSnapshot, request: Request): Response {
  const method = request.method.toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    return new Response("Method Not Allowed", {
      status: 405,
      headers: { allow: "GET, HEAD", "cache-control": "no-store" },
    });
  }

  const maxAge = Math.max(0, Math.floor((snapshot.expiresAt - Date.now()) / 1000));
  const headers = new Headers({
    "content-type": "application/json",
    etag: snapshot.etag,
    "cache-control": `public, max-age=${maxAge}`,
  });

  if (ifNoneMatch(request.headers.get("if-none-match"), snapshot.etag)) {
    return new Response(null, { status: 304, headers });
  }
  if (method === "HEAD") {
    headers.set("content-length", String(snapshot.contentLength));
    return new Response(null, { status: 200, headers });
  }
  return new Response(snapshot.body, { status: 200, headers });
}

/** Explicit failure for the endpoint when no generation could ever be built. */
export function aptIndexUnavailable(reason: string | null): Response {
  return new Response(
    `APT metadata index unavailable${reason ? `: ${reason}` : ""}\n`,
    {
      status: 502,
      headers: {
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "no-store",
      },
    },
  );
}
