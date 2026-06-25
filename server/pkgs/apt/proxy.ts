/**
 * APT repository reverse proxy built on {@link WebListProxy}.
 *
 * The upstream listing (Apache/nginx autoindex) is served via WebListProxy's
 * three modes (JSON, web listing, WebDAV), while APT-specific intelligence
 * lives in the `fetchHook`:
 *   - `by-hash/<algo>/<hash>` files are served only if the hash is known, with
 *     an immutable one-year cache (the hash pins the bytes — clients self-verify);
 *   - other `dists/` files are served only if the index lists them, cached until
 *     the source's `Valid-Until`;
 *   - `pool/` and other namespaces aren't listed in `Release`, so they pass
 *     through unjudged (the actual `.deb` files live there).
 *
 * Upstream fetches go through `cachedfetch`, which owns the read-through cache.
 */

import { cachedfetch, ezfetch, sanitizeResponse } from "@server/pkgs/fetch";
import { WebListProxy, defaultRender } from "@server/pkgs/web-list";

import type { AptRepo } from "./repo";

/** Matches a content-addressed `by-hash/<algo>/<hash>` path; group 1 is the hash. */
const BY_HASH = /\/by-hash\/(?:MD5Sum|SHA1|SHA256|SHA512)\/([0-9a-f]{32,128})$/i;

const IMMUTABLE_CACHE_CONTROL = "public, max-age=31536000, immutable";

const notFound = (): Response => new Response(null, { status: 404 });

export interface CreateAptProxyOptions {
  /** Mount prefix for WebDAV hrefs (e.g. `/debian/`). Defaults to `/`. */
  baseHref?: string;
}

/**
 * Create a {@link WebListProxy} wired to an APT repo's upstream, with the
 * APT-specific enforcement logic as its `fetchHook`.
 */
export function createAptProxy(repo: AptRepo, opts?: CreateAptProxyOptions): WebListProxy {
  return new WebListProxy({
    baseURL: repo.base,
    baseHref: opts?.baseHref,
    render: defaultRender,
    fetchHook(rel, request) {
      return aptFetchHook(rel, repo, request);
    },
  });
}

/**
 * APT-specific fetch hook: enforces existence checks from the Release index
 * and applies appropriate cache-control. Returns a Response to short-circuit,
 * or null to let WebListProxy handle the request normally (directory listings).
 */
async function aptFetchHook(
  rel: string,
  repo: AptRepo,
  request: Request,
): Promise<Response | null> {
  // Directories are handled by WebListProxy's readdir → render / JSON / WebDAV.
  if (rel === "" || rel.endsWith("/")) return null;

  // Block until the index is populated so enforcement decisions use real data.
  await repo.awaitResolved();

  const hash = rel.match(BY_HASH)?.[1]?.toLowerCase() ?? null;

  // Enforce existence only against a populated index.
  if (repo.ready) {
    if (hash !== null) {
      if (!repo.knownHashes.has(hash)) return notFound();
    } else if (rel === "dists" || isInKnownSuite(repo, rel)) {
      // Curated mirrors only resolve a subset of suites; the index has no
      // knowledge of the rest. Enforcement applies only to the `dists` root and
      // to suites we actually indexed — paths under an un-indexed suite pass
      // through unjudged so a curated suite list never blocks reachable suites.
      if (!repo.knownPaths.has(rel)) {
        // A directory addressed without a trailing slash → redirect so the
        // proxy serves it as a proper directory listing.
        if (hasChildren(repo, `${rel}/`)) {
          const url = new URL(request.url);
          url.pathname += "/";
          return Response.redirect(url.toString(), 301);
        }
        return notFound();
      }
    }
  }

  // By-hash files are content-addressed: safe to cache indefinitely.
  // Mutable dists/ files (Packages.gz, etc.) must be fetched fresh — the CF
  // Cache API can hold a stale copy after upstream rotates its index, causing
  // size/hash mismatches for APT clients validating against the new Release.
  const upstream = hash !== null
    ? await cachedfetch(repo.url(rel))
    : await ezfetch(repo.url(rel));
  return sanitizeResponse(upstream, {
    cacheControl: hash !== null ? IMMUTABLE_CACHE_CONTROL : sourceCacheControl(repo),
  });
}

/** `Cache-Control` derived from how long the source index stays valid. */
function sourceCacheControl(repo: AptRepo): string {
  const seconds = Math.max(0, Math.floor((repo.validUntil - Date.now()) / 1000));
  return `public, max-age=${seconds}`;
}

/** True if `rel` (a `dists/...` path) sits inside a suite the index resolved. */
function isInKnownSuite(repo: AptRepo, rel: string): boolean {
  if (!rel.startsWith("dists/")) return false;
  const suite = rel.slice("dists/".length).split("/")[0];
  return repo.knownSuites.has(suite);
}

/** True if any known path sits under `prefix` (i.e. `prefix` is a directory). */
function hasChildren(repo: AptRepo, prefix: string): boolean {
  for (const path of repo.knownPaths) {
    if (path.startsWith(prefix)) return true;
  }
  return false;
}
