/**
 * PyPI mirror — proxies the Simple API and package files.
 *
 * Routes (under `/pypi/`):
 * - `simple/`          → package index (links rewritten to relative)
 * - `simple/<pkg>/`    → per-package file list (links rewritten to local packages/)
 * - `pypi/<path>`      → JSON API passthrough to pypi.org
 * - `packages/<path>`  → file downloads from files.pythonhosted.org
 *
 * All responses are cached via `cachedfetch`. The Simple API pages are
 * streamed through HTMLRewriter to rewrite absolute hrefs into relative
 * paths so pip resolves them against our mirror instead of upstream.
 */

import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";
import type { Mirror } from "@server/types";

const PYPI_ORIGIN = "https://pypi.org";
const FILES_ORIGIN = "https://files.pythonhosted.org";

const PATH = "pypi";
const PREFIX = `/${PATH}/`;

export const pypi: Mirror = {
  name: "pypi",
  path: PATH,
  async fetch(request) {
    const url = new URL(request.url);
    const rel = url.pathname.slice(PREFIX.length);

    // simple/ — package index
    if (rel === "simple" || rel === "simple/") {
      return handleSimpleIndex();
    }

    // simple/<pkg>/ — per-package file list
    const pkgMatch = rel.match(/^simple\/([^/]+)\/?$/);
    if (pkgMatch) {
      return handleSimplePackage(pkgMatch[1]);
    }

    // pypi/* — JSON API
    if (rel.startsWith("pypi/") || rel === "pypi") {
      const upstream = await cachedfetch(`${PYPI_ORIGIN}/${rel}`);
      return sanitizeResponse(upstream);
    }

    // packages/* — file downloads
    if (rel.startsWith("packages/")) {
      const upstream = await cachedfetch(`${FILES_ORIGIN}/${rel}`);
      return sanitizeResponse(upstream);
    }

    return new Response("Not Found", { status: 404 });
  },
};

/** Fetch the Simple index and rewrite `/simple/pkg/` hrefs to relative `pkg/`. */
async function handleSimpleIndex(): Promise<Response> {
  const res = await cachedfetch(`${PYPI_ORIGIN}/simple/`);
  if (!res.ok) return sanitizeResponse(res);

  return new HTMLRewriter()
    .on("a", {
      element(el) {
        const href = el.getAttribute("href") ?? "";
        if (!href) return;
        // Upstream links are `/simple/pkg/` — strip the `/simple/` prefix.
        if (href.startsWith("/simple/")) {
          el.setAttribute("href", href.slice("/simple/".length));
        }
      },
    })
    .transform(sanitizeResponse(res));
}

/** Fetch a package's file list and rewrite file URLs to local `../../packages/`. */
async function handleSimplePackage(pkg: string): Promise<Response> {
  const res = await cachedfetch(`${PYPI_ORIGIN}/simple/${pkg}/`);
  if (!res.ok) return sanitizeResponse(res);

  const replaceFrom = `${FILES_ORIGIN}/packages/`;
  const replaceTo = "../../packages/";

  return new HTMLRewriter()
    .on("a", {
      element(el) {
        const href = el.getAttribute("href") ?? "";
        if (!href) return;
        el.setAttribute("href", href.replace(replaceFrom, replaceTo));
      },
    })
    .transform(sanitizeResponse(res));
}
