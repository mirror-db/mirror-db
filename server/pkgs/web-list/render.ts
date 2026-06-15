/**
 * Default HTML listing renderer for {@link WebListProxy}.
 *
 * Fetches the static HTML shell from assets and injects entry data as JSON
 * into a `<script>` block. Client-side Vue app picks it up and renders.
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

  const meta = JSON.stringify({ path, entries });
  const title = `/${path}`;

  // Fallback: plain list for no-JS / crawlers
  const fallbackHtml = entries
    .map((e) => {
      const href = e.type === "directory" ? `${e.name}/` : e.name;
      return `<a href="${href}">${href}</a>`;
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
