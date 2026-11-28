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

import { cachedfetch, mdbfetch, sanitizeResponse } from "@server/pkgs/fetch";
import { withRelayDirectives } from "@server/relay-control";
import type { Mirror } from "@server/types";

const PYPI_ORIGIN = "https://pypi.org";
const FILES_ORIGIN = "https://files.pythonhosted.org";

const PATH = "pypi";
const PREFIX = `/${PATH}/`;

/** Request headers a conditional revalidation needs to survive the hop. */
const CONDITIONAL_HEADERS = ["if-none-match", "if-modified-since"] as const;

export const pypi: Mirror = {
  name: "pypi",
  path: PATH,
  async fetch(request) {
    const url = new URL(request.url);
    const rel = url.pathname.slice(PREFIX.length);

    // simple/ — package index
    if (rel === "simple" || rel === "simple/") {
      // The root index is the one page whose refresh benefits every client;
      // per-package pages and file downloads are demand-driven only. A 304 is
      // repeated so a relay that lost its in-memory lease to a restart can
      // rebuild it from a revalidation. Errors advertise nothing.
      const res = await handleSimpleIndex(request);
      return res.status === 200 || res.status === 304
        ? withRelayDirectives(res, ["prefetch"])
        : res;
    }

    // simple/<pkg>/ — per-package file list
    const pkgMatch = rel.match(/^simple\/([^/]+)\/?$/);
    if (pkgMatch) {
      return handleSimplePackage(pkgMatch[1]);
    }

    // pypi/* — JSON API
    if (rel.startsWith("pypi/") || rel === "pypi") {
      return cachedfetch(PYPI_ORIGIN, [rel]);
    }

    // packages/* — file downloads
    if (rel.startsWith("packages/")) {
      return cachedfetch(FILES_ORIGIN, [rel]);
    }

    return new Response("Not Found", { status: 404 });
  },
};

/** Fetch the Simple index and rewrite `/simple/pkg/` hrefs to relative `pkg/`. */
async function handleSimpleIndex(request?: Request): Promise<Response> {
  // A conditional request must reach pypi.org: only it can answer `304`, and a
  // relay rebuilding its lease after a restart depends on being told "still
  // unchanged" without transferring the index again. The read-through cache
  // would answer from its own copy and ignore the client's validator, so this
  // path bypasses it.
  if (request && CONDITIONAL_HEADERS.some((name) => request.headers.has(name))) {
    return handleSimpleIndexConditional(request);
  }

  const res = await cachedfetch(PYPI_ORIGIN, ["simple/"]);
  if (!res.ok) return res;
  return rewriteSimpleIndex(res);
}

/**
 * Forward a conditional revalidation upstream. `identity` keeps the upstream
 * body in the same representation the cache would store, so the `304` the relay
 * receives really describes the bytes it holds.
 */
async function handleSimpleIndexConditional(request: Request): Promise<Response> {
  const headers = new Headers({ "accept-encoding": "identity" });
  for (const name of [...CONDITIONAL_HEADERS, "accept"] as const) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }

  const upstream = await mdbfetch(
    new Request(`${PYPI_ORIGIN}/simple/`, { headers, redirect: "follow" }),
  );
  const sanitized = sanitizeResponse(upstream);
  // 304: nothing to rewrite, and the empty body must stay empty.
  if (sanitized.status === 304 || !sanitized.ok) return sanitized;
  return rewriteSimpleIndex(sanitized);
}

/** Make `/simple/pkg/` links relative so pip stays on the mirror. */
function rewriteSimpleIndex(res: Response): Response {
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
    .transform(res);
}

/** Fetch a package's file list and rewrite file URLs to local `../../packages/`. */
async function handleSimplePackage(pkg: string): Promise<Response> {
  const res = await cachedfetch(PYPI_ORIGIN, ["simple", `${pkg}/`]);
  if (!res.ok) return res;

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
    .transform(res);
}
