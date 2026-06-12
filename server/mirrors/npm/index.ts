/**
 * npm registry mirror — proxies registry.npmjs.org with tarball URL rewriting.
 *
 * Routes (under `/npm/`):
 * - `<pkg>` or `@<scope>/<pkg>` → package metadata JSON (tarball URLs rewritten)
 * - `<pkg>/-/<tarball>.tgz`     → tarball passthrough
 * - `-/v1/search?text=...`      → search API passthrough
 *
 * Configuration: `npm config set registry https://mirs.uk/npm/`
 *
 * The rewriting replaces `https://registry.npmjs.org/` in JSON responses with
 * the mirror's own origin + prefix, so npm/yarn/pnpm fetch tarballs through
 * us rather than hitting the origin directly.
 */

import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";
import type { Mirror } from "@server/types";

const UPSTREAM = "https://registry.npmjs.org";
const PREFIX = "/npm/";

export const npm: Mirror = {
  name: "npm",
  path: PREFIX,
  async fetch(request) {
    const url = new URL(request.url);
    const rel = url.pathname.slice(PREFIX.length);
    const target = `${UPSTREAM}/${rel}${url.search}`;

    const upstream = await cachedfetch(target);

    // Only rewrite JSON metadata responses — tarballs and other content pass through.
    const ct = upstream.headers.get("content-type") ?? "";
    if (!ct.includes("json")) {
      return sanitizeResponse(upstream);
    }

    // Rewrite tarball URLs from registry.npmjs.org to our mirror prefix.
    const origin = new URL(request.url).origin;
    const mirrorBase = `${origin}${PREFIX}`;
    const body = await upstream.text();
    const rewritten = body.replaceAll(`${UPSTREAM}/`, mirrorBase);

    return new Response(rewritten, {
      status: upstream.status,
      headers: sanitizeResponse(upstream).headers,
    });
  },
};
