import {
  cleanLastModified,
  cleanName,
  cleanSize,
  cleanType,
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

interface LiRow {
  name: string;
  href: string | undefined;
  size: string;
  date: string;
}

/**
 * Parser for NVIDIA-style `<ul class="directorycontents">` listings.
 *
 * Each `<li>` is one entry. `<span class="file|dir">` holds the name,
 * `<span class="size">` and `<span class="date">` hold metadata.
 */
export class UlParser implements WebListParser {
  private rows: LiRow[] = [];
  private active = false;
  private liRow: LiRow | null = null;
  private liCell: "name" | "size" | "date" | null = null;
  private inAnchor = 0;

  install(rewriter: HTMLRewriter): void {
    rewriter
      .on("ul.directorycontents", {
        element: (el) => {
          this.active = true;
          el.onEndTag(() => { this.active = false; });
        },
      })
      .on("ul.directorycontents li", {
        element: (el) => {
          if (!this.active) return;
          this.liRow = { name: "", href: undefined, size: "", date: "" };
          el.onEndTag(() => {
            if (this.liRow && this.liRow.href) this.rows.push(this.liRow);
            this.liRow = null;
            this.liCell = null;
          });
        },
      })
      .on("ul.directorycontents li span", {
        element: (el) => {
          if (!this.liRow) return;
          const cls = el.getAttribute("class") ?? "";
          if (cls.includes("file") || cls.includes("dir")) {
            this.liCell = "name";
          } else if (cls.includes("size")) {
            this.liCell = "size";
          } else if (cls.includes("date")) {
            this.liCell = "date";
          } else {
            this.liCell = null;
          }
          el.onEndTag(() => { this.liCell = null; });
        },
        text: (t) => {
          if (!this.liRow || !this.liCell || this.inAnchor) return;
          this.liRow[this.liCell] += t.text;
        },
      })
      .on("ul.directorycontents li a", {
        element: (el) => {
          this.inAnchor++;
          if (this.liRow && this.liRow.href === undefined) {
            const h = el.getAttribute("href");
            if (h != null) this.liRow.href = h;
          }
          el.onEndTag(() => { this.inAnchor--; });
        },
        text: (t) => {
          // Anchor text goes to the name field (it's always inside the name span).
          if (this.liRow && this.liCell === "name") {
            this.liRow.name += t.text;
          }
        },
      });
  }

  result(): WebListEntry[] {
    const entries: WebListEntry[] = [];
    for (const row of this.rows) {
      if (!row.href) continue;
      if (isNavLink(row.href)) continue;

      const name = cleanName(cleanText(row.name) || row.href);
      if (!name || name === "Parent Directory") continue;

      entries.push({
        name,
        href: row.href,
        type: cleanType(row.name, row.href),
        lastModified: cleanLastModified(cleanText(row.date)),
        size: cleanSize(cleanText(row.size)),
      });
    }
    return entries;
  }
}

function isNavLink(href: string): boolean {
  return (
    href.startsWith("/") ||
    href.startsWith("?") ||
    href.startsWith("#") ||
    href.startsWith("..") ||
    /^[a-z]+:\/\//i.test(href)
  );
}

export const createUlParser = (): WebListParser => new UlParser();
