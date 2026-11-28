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
import { withRelayDirectives, type RelayDirective } from "@server/relay-control";
import { contentAddressedPathFact, declarePathFact } from "@server/relay-path";
import {
  WebListProxy,
  defaultRender,
  type WebListSource,
} from "@server/pkgs/web-list";

import type { AptRepository } from "./repository";

/** Matches a content-addressed `by-hash/<algo>/<hash>` path; group 1 is the algorithm, group 2 the hash. */
const BY_HASH = /\/by-hash\/(MD5Sum|SHA1|SHA256|SHA512)\/([0-9a-f]{32,128})$/i;

/**
 * `dists/<suite>/InRelease|Release|Release.gpg` — the three signed names of one
 * suite. APT falls back between them, so a suite whose snapshot only proved one
 * of them must still be able to fetch the others upstream.
 */
const SIGNED_SIBLING = /^dists\/[^/]+\/(?:InRelease|Release|Release\.gpg)$/;

const IMMUTABLE_CACHE_CONTROL = "public, max-age=31536000, immutable";

/**
 * `X-MDB-Relay` response control. Mutable APT indexes must never enter a relay
 * cache: the relay would hold `Release`/`Packages` for its max-age/Valid-Until
 * window while upstream may already have rotated the index, reintroducing the
 * Hash Sum mismatch this proxy avoids by not caching them itself.
 */
const RELAY_NO_STORE = "no-store";
/** Asks the relay to refresh this resource while it sees demand for it. */
const RELAY_PREFETCH = "prefetch";

/** `dists/<suite>/InRelease` or `dists/<suite>/Release` — the signed index. */
const RELEASE_PATH_RE = /^dists\/[^/]+\/(?:InRelease|Release)$/;

const notFound = (): Response => new Response(null, { status: 404 });

export interface CreateAptProxyOptions {
  /** Mount prefix for WebDAV hrefs (e.g. `/debian/`). Defaults to `/`. */
  baseHref?: string;
  /** Directory source override; omitted mirrors read upstream autoindex. */
  source?: WebListSource;
  /**
   * Add `X-MDB-Relay: prefetch` to `Release`/`InRelease` responses. Only
   * mirrors whose metadata the Worker keeps a snapshot of need this; pool
   * payloads and ordinary `Packages` indexes are never marked.
   */
  prefetchRelease?: boolean;
}

/**
 * Create a {@link WebListProxy} wired to an APT repo's upstream, with the
 * APT-specific enforcement logic as its `fetchHook`.
 */
export function createAptProxy(repo: AptRepository, opts?: CreateAptProxyOptions): WebListProxy {
  return new WebListProxy({
    baseURL: repo.base,
    baseHref: opts?.baseHref,
    render: defaultRender,
    source: opts?.source,
    fetchHook(rel, request) {
      return aptFetchHook(rel, repo, request, opts);
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
  repo: AptRepository,
  request: Request,
  opts?: CreateAptProxyOptions,
): Promise<Response | null> {
  // Directories are handled by WebListProxy's readdir → render / JSON / WebDAV.
  if (rel === "" || rel.endsWith("/")) return null;

  const byHash = rel.match(BY_HASH);
  const hash = byHash?.[2]?.toLowerCase() ?? null;
  const hashAlgorithm = byHash?.[1]?.toUpperCase() ?? null;

  // One call yields one coherent generation for existence, freshness and relay
  // decisions. No generation means the request stays unjudged.
  const facts = await repo.facts();

  if (facts) {
    if (hash !== null) {
      // Content-addressed: unknown means the release never named it.
      if (!facts.hashes.has(hash)) return notFound();
    } else if (facts.coverage === "tree") {
      // Snapshot mounts know the whole tree, including `pool/`. Any directory
      // addressed without its trailing slash redirects, so a browser or WebDAV
      // client sees one URL per directory. Unknown paths stay unjudged: the
      // generation only lists metadata it verified, and a real package body is
      // not something it can prove absent.
      if (!facts.paths.has(rel)) {
        if (hasChildren(facts.paths, `${rel}/`)) return directoryRedirect(rel, opts?.baseHref);
        // A known suite's other signed names are not a 404: apt chooses between
        // `InRelease`, `Release` and `Release.gpg`, and a generation that only
        // verified one must not hide the fallbacks.
        const signedSibling = SIGNED_SIBLING.test(rel) && isInKnownSuite(facts.suites, rel);
        if (!signedSibling && (rel === "dists" || isInKnownSuite(facts.suites, rel))) {
          return notFound();
        }
      }
    } else if (rel === "dists" || isInKnownSuite(facts.suites, rel)) {
      // Curated mirrors only resolve a subset of suites; the index has no
      // knowledge of the rest. Enforcement applies only to the `dists` root and
      // to suites we actually indexed — paths under an un-indexed suite pass
      // through unjudged so a curated suite list never blocks reachable suites.
      if (!facts.paths.has(rel)) {
        if (hasChildren(facts.paths, `${rel}/`)) return directoryRedirect(rel, opts?.baseHref);
        return notFound();
      }
    }
  }

  // By-hash files are content-addressed: safe to cache indefinitely.
  // Mutable dists/ files (Packages.gz, etc.) must be fetched fresh — the CF
  // Cache API can hold a stale copy after upstream rotates its index, causing
  // size/hash mismatches for APT clients validating against the new Release.
  //
  // The inbound method travels with the request: a HEAD must reach upstream as
  // a HEAD (a GET would transfer the body a probe exists to avoid). Range is
  // forwarded only for by-hash objects: those are content-addressed, and
  // `cachedfetch` then bypasses its read-through cache so a 206 can never be
  // stored under the full-URL key. A pool file keeps the known limitation of
  // not resuming (its Range would be answered as a 200 from byte 0); mutable
  // `dists/` files are never ranged either.
  const isHead = request.method.toUpperCase() === "HEAD";
  const range = isHead || hash === null ? null : request.headers.get("range");
  // A by-hash name pins one byte sequence, and the relay's immutable active
  // cache only admits an identity coding, so a content-addressed fetch asks for
  // one — for a HEAD as well as a GET. `cachedfetch` keeps bypassing its own
  // read-through cache for a Range request, so a 206 can never be stored under
  // the full-URL key.
  const upstreamHeaders: Record<string, string> = {};
  if (hash !== null) upstreamHeaders["accept-encoding"] = "identity";
  if (range !== null) upstreamHeaders.range = range;
  const hasUpstreamHeaders = Object.keys(upstreamHeaders).length > 0;
  const init: RequestInit | undefined = isHead
    ? { method: "HEAD", ...(hasUpstreamHeaders ? { headers: upstreamHeaders } : {}) }
    : hasUpstreamHeaders
      ? { headers: upstreamHeaders }
      : undefined;
  const upstream = hash !== null
    ? await cachedfetch(repo.url(rel), init)
    : await ezfetch(repo.url(rel), init);
  const sanitized = sanitizeResponse(upstream, {
    cacheControl:
      hash !== null
        ? IMMUTABLE_CACHE_CONTROL
        : sourceCacheControl(facts?.validUntil ?? 0),
  });

  // A by-hash object the generation proved exists is content-addressed: the
  // relay may keep it and answer from local bytes. Only a *verified* hash
  // qualifies — an unjudged pass-through (no index, or a snapshot generation
  // that has not loaded yet) declares nothing, and mutable `dists/` indexes and
  // `pool/` payloads are never called immutable.
  const pathFact =
    hash !== null && hashAlgorithm !== null && facts !== null && facts.hashes.has(hash)
      ? contentAddressedPathFact(hashAlgorithm, hash)
      : null;
  const declared = declarePathFact(sanitized, pathFact);

  // The same generation hazard applies to the relay's default request cache:
  // tell it not to store a mutable dists index. by-hash objects are
  // content-addressed and pool payloads are not indexes, so neither gets this.
  const directives: RelayDirective[] = [];
  if (hash === null && rel.startsWith("dists/")) directives.push(RELAY_NO_STORE);
  // Release/InRelease is the entry point a relay can refresh by itself; the
  // Worker's own metadata snapshot is rebuilt from it.
  if (
    opts?.prefetchRelease &&
    hash === null &&
    (sanitized.status === 200 || sanitized.status === 304) &&
    RELEASE_PATH_RE.test(rel)
  ) {
    directives.push(RELAY_PREFETCH);
  }

  return withRelayDirectives(declared, directives);
}

/** `Cache-Control` derived from how long the source index stays valid. */
function sourceCacheControl(validUntil: number): string {
  const seconds = Math.max(0, Math.floor((validUntil - Date.now()) / 1000));
  return `public, max-age=${seconds}`;
}

/** True if `rel` (a `dists/...` path) sits inside a suite the index resolved. */
function isInKnownSuite(suites: ReadonlySet<string>, rel: string): boolean {
  if (!rel.startsWith("dists/")) return false;
  const suite = rel.slice("dists/".length).split("/")[0];
  return suites.has(suite);
}

/** True if any known path sits under `prefix` (i.e. `prefix` is a directory). */
function hasChildren(paths: ReadonlySet<string>, prefix: string): boolean {
  for (const path of paths) {
    if (path.startsWith(prefix)) return true;
  }
  return false;
}

/**
 * A directory addressed without a trailing slash → redirect so the proxy serves
 * it as a proper directory listing.
 *
 * The `Location` is root-relative and includes the mount prefix: an absolute URL
 * built from `request.url` would point at the Worker's origin and take the
 * client off the relay permanently, and a bare relative path would land outside
 * the mount. `308` keeps the method and body (APT only GETs here, but a
 * permanent `301` would also be cached by clients forever, outliving a relay
 * move).
 */
function directoryRedirect(rel: string, baseHref?: string): Response {
  const prefix = !baseHref ? "/" : baseHref.endsWith("/") ? baseHref : `${baseHref}/`;
  return new Response(null, {
    status: 308,
    headers: { location: `${prefix}${rel}/` },
  });
}
