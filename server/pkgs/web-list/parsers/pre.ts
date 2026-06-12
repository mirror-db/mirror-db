import {
  cleanLastModified,
  cleanName,
  cleanSize,
  cleanType,
  type WebListEntry,
} from "../parse";
import type { WebListParser } from "./types";

interface PreEntry {
  href: string;
  name: string;
  trailing: string;
}

/**
 * Parser for Apache legacy autoindex `<pre>`-based listings (no HTMLTable).
 *
 * Format: `<pre>` block where each line is:
 * ```
 * <img ...> <a href="name/">name/</a>           2025-07-31 15:39    -   Description
 * ```
 *
 * The header line contains sort links (`?C=N;O=D`) and is filtered out.
 */
export class PreParser implements WebListParser {
  private active = false;
  private inAnchor = false;
  private current: PreEntry | null = null;
  private entries: PreEntry[] = [];
  /** Trailing text accumulator — text after </a> until next \n or <a>. */
  private trailing = "";

  install(rewriter: HTMLRewriter): void {
    rewriter
      .on("pre", {
        element: (el) => {
          this.active = true;
          el.onEndTag(() => {
            this.flush();
            this.active = false;
          });
        },
        text: (t) => {
          if (!this.active || this.inAnchor) return;
          // Text outside anchors: trailing metadata for the current entry,
          // or inter-entry whitespace. Split by newlines to detect line ends.
          const parts = t.text.split("\n");
          for (let i = 0; i < parts.length; i++) {
            if (i > 0) {
              // Newline boundary — flush current entry
              this.flush();
            }
            this.trailing += parts[i];
          }
        },
      })
      .on("pre a", {
        element: (el) => {
          if (!this.active) return;
          this.inAnchor = true;
          const href = el.getAttribute("href");
          // Start a new candidate entry (previous one's trailing is complete
          // because the <a> starts a new logical token on the same line).
          // But first check if we need to flush a pending entry — a second <a>
          // on the same line (e.g. sort links on the header) just replaces.
          if (href != null && !isNavLink(href)) {
            // Flush any previous entry that was still accumulating trailing text
            this.flush();
            this.current = { href, name: "", trailing: "" };
          }
          el.onEndTag(() => {
            this.inAnchor = false;
          });
        },
        text: (t) => {
          if (!this.active) return;
          if (this.current) {
            this.current.name += t.text;
          }
        },
      });
  }

  result(): WebListEntry[] {
    const results: WebListEntry[] = [];
    for (const e of this.entries) {
      const name = cleanName(e.name.trim());
      if (!name || name === "Parent Directory") continue;

      results.push({
        name,
        href: e.href,
        type: cleanType(e.name, e.href),
        lastModified: cleanLastModified(e.trailing),
        size: cleanSize(extractSize(e.trailing)),
      });
    }
    return results;
  }

  private flush() {
    if (this.current) {
      this.current.trailing = this.trailing;
      this.entries.push(this.current);
      this.current = null;
    }
    this.trailing = "";
  }
}

/** Extract size token from trailing metadata text.
 * Format: `2025-07-31 15:39    -   Description` or `2026-02-12 14:46  594 `
 * or `2026-06-11 12:11:08      3.7KB`
 * The size is the token after the datetime (YYYY-MM-DD HH:MM[:SS]). */
const TRAILING_RE = /\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}(?::\d{2})?\s+([\S]+)/;

function extractSize(trailing: string): string {
  const m = TRAILING_RE.exec(trailing);
  return m ? m[1] : "";
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

export const createPreParser = (): WebListParser => new PreParser();
