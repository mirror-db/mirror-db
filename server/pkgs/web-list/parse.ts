/**
 * Parse an Apache/nginx-style autoindex directory listing into structured
 * entries.
 *
 * Supports multiple listing formats via pluggable parsers (see `parsers/`).
 * The default set covers:
 * - Table (Apache/nginx FancyIndexing HTMLTable)
 * - UL (NVIDIA-style `<ul class="directorycontents">`)
 * - Pre (Apache legacy `<pre>` block)
 *
 * All parsers are installed on a single HTMLRewriter; the first to produce
 * results wins. Custom parser sets can be passed to `parseListing` or to
 * `WebListFs` for per-site tuning.
 *
 * Shared helpers (`guessColumns`, `rowsToEntries`, `cleanName`, etc.) remain
 * exported for use by individual parsers and for unit-testing under Node.
 */

import { allParsers, type WebListParserFactory } from "./parsers";

// ── Public types ────────────────────────────────────────────────────────────

export type WebListEntryType = "file" | "directory";

export interface WebListEntry {
  /** Display name with any trailing `/` stripped (`dists/` → `dists`). */
  name: string;
  /** Raw `href` from the row's anchor (`dists/`, `README`). */
  href: string;
  type: WebListEntryType;
  /** Parsed mtime, or `null` when the column is blank / unparseable. */
  lastModified: Date | null;
  /** Size in bytes, or `null` for directories / unknown. */
  size: number | null;
}

/** A single parsed table cell: its text plus the first anchor `href`, if any. */
export interface Cell {
  text: string;
  href?: string;
}

export interface ListingRows {
  headerRows: Cell[][];
  dataRows: Cell[][];
}

// ── Column detection (used by TableParser) ──────────────────────────────────

interface ColumnMap {
  name: number;
  lastModified: number;
  size: number;
  type: number;
}

/** Which logical property each `<th>` label maps to. */
const HEADER_MATCHERS: Record<keyof ColumnMap, RegExp> = {
  name: /\bname\b/i,
  lastModified: /last\s*mod(?:ified)?|date/i,
  size: /\bsize\b/i,
  type: /^\s*type\s*$/i,
};

/** Walk the header rows, picking the row with the most matched columns. */
export function guessColumns(headerRows: Cell[][]): ColumnMap {
  let best: ColumnMap = { name: -1, lastModified: -1, size: -1, type: -1 };
  let bestHits = -1;

  for (const r of headerRows) {
    const map: ColumnMap = { name: -1, lastModified: -1, size: -1, type: -1 };
    let hits = 0;
    r.forEach((c, i) => {
      for (const key of Object.keys(HEADER_MATCHERS) as (keyof ColumnMap)[]) {
        if (map[key] === -1 && HEADER_MATCHERS[key].test(c.text)) {
          map[key] = i;
          hits++;
        }
      }
    });
    if (hits > bestHits) {
      bestHits = hits;
      best = map;
    }
  }
  return best;
}

// ── Cell cleaners ───────────────────────────────────────────────────────────

const DATE_RE = /(\d{4})-(\d{2})-(\d{2})\s+(\d{2}):(\d{2})(?::(\d{2}))?/;

/** `2026-05-16 10:14` → `Date` (interpreted as UTC for determinism). */
export function cleanLastModified(text: string): Date | null {
  const m = DATE_RE.exec(text);
  if (!m) return null;
  const [, y, mo, d, h, mi, s] = m;
  return new Date(Date.UTC(+y, +mo - 1, +d, +h, +mi, s ? +s : 0));
}

const SIZE_RE = /^([\d.]+)\s*([KMGTP]?)(?:i?B)?$/i;
const UNIT_POW: Record<string, number> = { "": 0, K: 1, M: 2, G: 3, T: 4, P: 5 };

/** `1.2K` → `1229`, `291` → `291`, `-`/blank → `null`. */
export function cleanSize(text: string): number | null {
  const t = text.trim();
  if (!t || t === "-") return null;
  const m = SIZE_RE.exec(t);
  if (!m) return null;
  const [, num, unit] = m;
  return Math.round(parseFloat(num) * 1024 ** UNIT_POW[unit.toUpperCase()]);
}

/** Trailing-slash name / href both mean a subdirectory. */
export function cleanType(name: string, href: string): WebListEntryType {
  return name.endsWith("/") || href.endsWith("/") ? "directory" : "file";
}

/** Strip a single trailing slash off the display name. */
export function cleanName(text: string): string {
  return text.replace(/\/+$/, "").trim();
}

// ── rowsToEntries (used by TableParser) ─────────────────────────────────────

/** `href` that points outside the current dir. */
function isNavLink(href: string): boolean {
  return (
    href.startsWith("/") ||
    href.startsWith("?") ||
    href.startsWith("#") ||
    href.startsWith("..") ||
    /^[a-z]+:\/\//i.test(href)
  );
}

/** Turn collected rows into entries: guess columns from headers, clean cells. */
export function rowsToEntries({ headerRows, dataRows }: ListingRows): WebListEntry[] {
  const cols = guessColumns(headerRows);
  if (cols.name === -1) return [];

  const needed = Math.max(cols.name, cols.lastModified, cols.size);

  const entries: WebListEntry[] = [];
  for (const cells of dataRows) {
    if (cells.length <= needed) continue;

    const nameCell = cells[cols.name];
    const href = nameCell?.href;
    if (!href || isNavLink(href)) continue;

    const name = cleanName(nameCell.text || href);
    if (!name || name === "Parent Directory") continue;

    entries.push({
      name,
      href,
      type: cleanType(nameCell.text, href),
      lastModified:
        cols.lastModified === -1
          ? null
          : cleanLastModified(cells[cols.lastModified]?.text ?? ""),
      size: cols.size === -1 ? null : cleanSize(cells[cols.size]?.text ?? ""),
    });
  }

  return entries;
}

// ── Legacy collectRows (kept for backward compat, delegates to TableParser) ─

/**
 * @deprecated Use `parseListing` with parser factories instead. This function
 * is kept only for tests that inspect raw rows.
 */
export async function collectRows(input: Response | string): Promise<ListingRows> {
  if (typeof HTMLRewriter === "undefined") {
    throw new Error("collectRows requires HTMLRewriter (Workers runtime)");
  }
  // Import inline to avoid circular — TableParser imports from this file.
  const { TableParser } = await import("./parsers/table");
  const parser = new TableParser();
  const res = typeof input === "string" ? new Response(input) : input;
  const rewriter = new HTMLRewriter();
  parser.install(rewriter);
  await rewriter.transform(res).arrayBuffer();
  // Access internal state — TableParser exposes result() but we need raw rows
  // for backward compat. Use a helper cast.
  return {
    headerRows: (parser as any).headerRows ?? [],
    dataRows: (parser as any).dataRows ?? [],
  };
}

// ── Main entry point ────────────────────────────────────────────────────────

/**
 * Parse a directory-listing page into entries. Requires the Workers runtime
 * (HTMLRewriter).
 *
 * @param input - A Response to stream, or an HTML string.
 * @param parsers - Parser factories to use (default: all known formats).
 */
export async function parseListing(
  input: Response | string,
  parsers?: WebListParserFactory[],
): Promise<WebListEntry[]> {
  if (typeof HTMLRewriter === "undefined") {
    throw new Error("parseListing requires HTMLRewriter (Workers runtime)");
  }

  const factories = parsers ?? allParsers;
  const instances = factories.map((f) => f());

  const rewriter = new HTMLRewriter();
  for (const p of instances) p.install(rewriter);

  const res = typeof input === "string" ? new Response(input) : input;
  await rewriter.transform(res).arrayBuffer();

  // Return results from the first parser that produced entries.
  for (const p of instances) {
    const r = p.result();
    if (r.length > 0) return r;
  }
  return [];
}
