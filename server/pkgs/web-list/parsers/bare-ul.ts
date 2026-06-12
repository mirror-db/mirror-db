import { cleanName, cleanType, type WebListEntry } from "../parse";
import type { WebListParser } from "./types";

interface BareEntry {
  href: string;
  name: string;
}

/**
 * Parser for bare Apache autoindex `<ul>/<li>/<a>` listings (no class).
 *
 * Apache's mod_autoindex can output plain `<ul>` without metadata when
 * `IndexOptions` omits `FancyIndexing`. Format:
 * ```html
 * <ul><li><a href="name/"> name/</a></li>...</ul>
 * ```
 * No date, no size — entries carry only name/href/type.
 *
 * Only activates on `<ul>` elements without a `class` attribute to avoid
 * matching navigational `<ul>` (which always have framework classes).
 */
export class BareUlParser implements WebListParser {
  private entries: BareEntry[] = [];
  private active = false;
  private current: BareEntry | null = null;

  install(rewriter: HTMLRewriter): void {
    rewriter
      .on("ul", {
        element: (el) => {
          // Only activate on classless <ul> — avoids nav menus and NVIDIA's
          // `ul.directorycontents` (handled by UlParser).
          if (el.getAttribute("class") != null) return;
          this.active = true;
          el.onEndTag(() => { this.active = false; });
        },
      })
      .on("li", {
        element: (el) => {
          if (!this.active) return;
          this.current = null;
          el.onEndTag(() => {
            if (this.current) this.entries.push(this.current);
            this.current = null;
          });
        },
      })
      .on("li a", {
        element: (el) => {
          if (!this.active) return;
          const href = el.getAttribute("href");
          if (href != null) {
            this.current = { href, name: "" };
          }
        },
        text: (t) => {
          if (this.current) this.current.name += t.text;
        },
      });
  }

  result(): WebListEntry[] {
    const results: WebListEntry[] = [];
    for (const e of this.entries) {
      if (!e.href || isNavLink(e.href)) continue;

      const name = cleanName(e.name.trim() || e.href);
      if (!name || name === "Parent Directory") continue;

      results.push({
        name,
        href: e.href,
        type: cleanType(e.name, e.href),
        lastModified: null,
        size: null,
      });
    }
    return results;
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

export const createBareUlParser = (): WebListParser => new BareUlParser();
