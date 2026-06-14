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

import { upstreamProxy, rewriteBody } from "@server/mirrors/proxy";
import type { Mirror } from "@server/types";

const UPSTREAM = "https://registry.npmjs.org";
const PATH = "npm";
const PREFIX = `/${PATH}`;

const proxy = upstreamProxy({ url: UPSTREAM, prefix: PREFIX })
  .post(rewriteBody((body, req) => {
    const origin = new URL(req.url).origin;
    const mirrorBase = `${origin}${PREFIX}/`;
    return body.replaceAll(`${UPSTREAM}/`, mirrorBase);
  }));

export const npm: Mirror = {
  name: "npm",
  path: PATH,
  fetch: (request) => proxy.apply(request),
};
