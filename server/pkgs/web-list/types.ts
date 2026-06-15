/**
 * Shared types for the web-list package.
 * This file has no Workers-specific dependencies, so it can be imported
 * from client code via `@server/pkgs/web-list/types`.
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
