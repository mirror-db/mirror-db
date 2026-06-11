/**
 * Parse an Apache/nginx-style autoindex directory listing (the `<table>` of
 * `Name` / `Last modified` / `Size` columns) into structured entries.
 *
 * Two stages, split so the second is testable without the Workers runtime:
 *
 * 1. {@link collectRows} streams the HTML through {@link HTMLRewriter},
 *    producing two 2D cell arrays — one for `<th>` (header) rows, one for
 *    `<td>` (data) rows. HTMLRewriter only exists inside workerd, so this stage
 *    is exercised by the workers-pool test.
 * 2. {@link rowsToEntries} (pure) reads the `<th>` row to guess which column is
 *    `name` / `last modified` / `size`, then cleans each `<td>` cell into a
 *    typed value. The per-property regex matchers + cleaners are plain functions
 *    unit-tested directly under Node.
 */

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

interface ColumnMap {
  name: number;
  lastModified: number;
  size: number;
  type: number;
}

/** Which logical property each `<th>` label maps to. `type` has no column in
 * standard listings — it is derived from the name/href, so its matcher is
 * deliberately strict and normally matches nothing. */
const HEADER_MATCHERS: Record<keyof ColumnMap, RegExp> = {
  name: /\bname\b/i,
  lastModified: /last\s*mod(?:ified)?|date/i,
  size: /\bsize\b/i,
  type: /^\s*type\s*$/i,
};

// ── stage 1: HTML → rows (HTMLRewriter, workerd only) ───────────────────────

/** Strip the entities autoindex listings use; collapse whitespace; trim. */
function cleanText(raw: string): string {
  return raw
    .replace(/&nbsp;/gi, " ")
    .replace(/&amp;/gi, "&")
    .replace(/&lt;/gi, "<")
    .replace(/&gt;/gi, ">")
    .replace(/&#(\d+);/g, (_, n) => String.fromCharCode(+n))
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * Stream a listing page through HTMLRewriter into header/data row arrays.
 *
 * Tables are walked statefully in document order: `<tr>` opens a row, `<th>` /
 * `<td>` opens a cell (the row's kind follows its cells), and text chunks —
 * which arrive split per the streaming parser — are concatenated onto the open
 * cell. Anchor text lives under `<a>`, not directly under the cell, so it is
 * captured by a dedicated `a` handler that also records the first `href`.
 */
export async function collectRows(input: Response | string): Promise<ListingRows> {
  if (typeof HTMLRewriter === "undefined") {
    throw new Error("collectRows requires HTMLRewriter (Workers runtime)");
  }
  const res = typeof input === "string" ? new Response(input) : input;

  const headerRows: Cell[][] = [];
  const dataRows: Cell[][] = [];

  let row: Cell[] | null = null;
  let rowKind: "th" | "td" | null = null;
  let cell: Cell | null = null;
  // HTMLRewriter fires text handlers for all text inside a matched element,
  // including descendants. `<th><a>Name</a></th>` fires text("Name") on BOTH
  // the `th` and `a` handlers. To avoid double-counting, track anchor depth:
  // when inside an `<a>`, only the `a` handler writes text.
  let inAnchor = 0;

  const startCell = (kind: "th" | "td", el: { onEndTag(h: () => void): void }) => {
    if (!row) row = [];
    rowKind = kind; // rows are homogeneous; last cell's kind classifies the row
    cell = { text: "" };
    row.push(cell);
    const c = cell;
    el.onEndTag(() => {
      c.text = cleanText(c.text);
      if (cell === c) cell = null;
    });
  };

  await new HTMLRewriter()
    .on("tr", {
      element(el) {
        row = [];
        rowKind = null;
        cell = null;
        el.onEndTag(() => {
          if (row && rowKind === "th") headerRows.push(row);
          else if (row && rowKind === "td") dataRows.push(row);
          row = null;
          rowKind = null;
          cell = null;
        });
      },
    })
    .on("th", {
      element(el) {
        startCell("th", el);
      },
      text(t) {
        if (cell && !inAnchor) cell.text += t.text;
      },
    })
    .on("td", {
      element(el) {
        startCell("td", el);
      },
      text(t) {
        if (cell && !inAnchor) cell.text += t.text;
      },
    })
    .on("a", {
      element(el) {
        inAnchor++;
        if (cell && cell.href === undefined) {
          const h = el.getAttribute("href");
          if (h != null) cell.href = h;
        }
        el.onEndTag(() => { inAnchor--; });
      },
      text(t) {
        if (cell) cell.text += t.text;
      },
    })
    .transform(res)
    .arrayBuffer();

  return { headerRows, dataRows };
}

// ── stage 2: rows → entries (pure) ──────────────────────────────────────────

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

/** `href` that points outside the current dir (parent, sort links, absolute). */
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

  // A real data row must reach the furthest column we read; this drops
  // ancillary tables (e.g. Debian's "Other directories" 2-cell rows).
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

/**
 * Parse a directory-listing page (a `Response` to stream, or an HTML string)
 * into entries. Requires the Workers runtime for the HTMLRewriter stage.
 */
export async function parseListing(input: Response | string): Promise<WebListEntry[]> {
  return rowsToEntries(await collectRows(input));
}
