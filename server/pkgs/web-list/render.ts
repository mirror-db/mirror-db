/**
 * Default HTML listing renderer for {@link WebListProxy}.
 *
 * Produces a minimal `<ul>` page from structured entries. Mirrors that don't
 * need custom rendering can use this directly as the `render` callback.
 */

import type { WebListEntry } from "./parse";

/** Minimal HTML entity escaping. */
function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/**
 * Render a directory listing as a simple HTML `<ul>` page.
 *
 * Usable as the `render` option for {@link WebListProxy}. The signature matches
 * `WebListProxyOptions["render"]`.
 */
export function defaultRender(entries: WebListEntry[], path: string): Response {
  const rows = entries
    .map((e) => {
      const href = esc(e.type === "directory" ? `${e.name}/` : e.name);
      return `<li><a href="${href}">${href}</a></li>`;
    })
    .join("\n");
  const title = esc(`/${path}`);
  const html = `<!doctype html><meta charset="utf-8"><title>${title}</title><h1>${title}</h1><ul>\n${rows}\n</ul>`;
  return new Response(html, {
    status: 200,
    headers: { "content-type": "text/html; charset=utf-8" },
  });
}
