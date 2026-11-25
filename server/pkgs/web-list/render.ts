/**
 * Default HTML listing renderer for {@link WebListProxy}.
 *
 * Fetches the static HTML shell from assets and injects entry data as JSON
 * into a `<script>` block. Client-side Vue app picks it up and renders.
 *
 * Entry names come from upstream listings, so they are untrusted: a file called
 * `</script><script>…` must not become markup here. The injected JSON escapes
 * the characters that could close the script element, and the no-JS fallback
 * escapes text and attributes.
 */

import { env } from "cloudflare:workers";
import type { WebListEntry } from "./parse";

let shellCache: string | null = null;

async function getShell(): Promise<string> {
  if (shellCache) return shellCache;
  const res = await env.ASSETS.fetch("http://localhost/weblist/index.html");
  shellCache = await res.text();
  return shellCache;
}

const HTML_ESCAPES: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

/** Escape text for HTML content or a quoted attribute value. */
export function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (char) => HTML_ESCAPES[char]!);
}

/**
 * `JSON.stringify` with the code points that could terminate or confuse an
 * inline `<script>` escaped. Valid JSON either way, and safe in HTML.
 */
export function encodeInlineJson(value: unknown): string {
  return JSON.stringify(value)
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

/**
 * Render a directory listing page.
 *
 * Injects `{ path, entries }` as `PAGE_META` into the HTML shell.
 * The client-side Vue app hydrates from this data.
 */
export async function defaultRender(
  entries: WebListEntry[],
  path: string,
): Promise<Response> {
  const shell = await getShell();

  const meta = encodeInlineJson({ path, entries });
  const title = escapeHtml(`/${path}`);

  // Fallback: plain list for no-JS / crawlers
  const fallbackHtml = entries
    .map((e) => {
      const href = e.type === "directory" ? `${e.name}/` : e.name;
      const escaped = escapeHtml(href);
      return `<a href="${escaped}">${escaped}</a>`;
    })
    .join("\n");

  const html = shell
    .replace("<!--title-->", title)
    .replace("__PAGE_META__", meta)
    .replace("<!--app-html-->", fallbackHtml);

  return new Response(html, {
    status: 200,
    headers: { "content-type": "text/html; charset=utf-8" },
  });
}
