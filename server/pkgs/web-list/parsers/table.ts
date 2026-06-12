import {
  guessColumns,
  rowsToEntries,
  type Cell,
  type WebListEntry,
} from "../parse";
import type { WebListParser } from "./types";

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
 * Parser for Apache/nginx FancyIndexing `<table>` listings.
 *
 * Recognises `<tr>` rows with `<th>` (header) or `<td>` (data) cells and
 * delegates to `guessColumns` + `rowsToEntries` for column detection.
 */
export class TableParser implements WebListParser {
  private headerRows: Cell[][] = [];
  private dataRows: Cell[][] = [];

  private row: Cell[] | null = null;
  private rowKind: "th" | "td" | null = null;
  private cell: Cell | null = null;
  private inAnchor = 0;

  install(rewriter: HTMLRewriter): void {
    rewriter
      .on("tr", {
        element: (el) => {
          this.row = [];
          this.rowKind = null;
          this.cell = null;
          el.onEndTag(() => {
            if (this.row && this.rowKind === "th") this.headerRows.push(this.row);
            else if (this.row && this.rowKind === "td") this.dataRows.push(this.row);
            this.row = null;
            this.rowKind = null;
            this.cell = null;
          });
        },
      })
      .on("th", {
        element: (el) => {
          this.startCell("th", el);
        },
        text: (t) => {
          if (this.cell && !this.inAnchor) this.cell.text += t.text;
        },
      })
      .on("td", {
        element: (el) => {
          this.startCell("td", el);
        },
        text: (t) => {
          if (this.cell && !this.inAnchor) this.cell.text += t.text;
        },
      })
      .on("tr a", {
        element: (el) => {
          this.inAnchor++;
          if (this.cell && this.cell.href === undefined) {
            const h = el.getAttribute("href");
            if (h != null) this.cell.href = h;
          }
          el.onEndTag(() => { this.inAnchor--; });
        },
        text: (t) => {
          if (this.cell) this.cell.text += t.text;
        },
      });
  }

  result(): WebListEntry[] {
    if (this.headerRows.length === 0 && this.dataRows.length === 0) return [];
    return rowsToEntries({ headerRows: this.headerRows, dataRows: this.dataRows });
  }

  private startCell(kind: "th" | "td", el: { onEndTag(h: () => void): void }) {
    if (!this.row) this.row = [];
    this.rowKind = kind;
    this.cell = { text: "" };
    this.row.push(this.cell);
    const c = this.cell;
    el.onEndTag(() => {
      c.text = cleanText(c.text);
      if (this.cell === c) this.cell = null;
    });
  }
}

export const createTableParser = (): WebListParser => new TableParser();
