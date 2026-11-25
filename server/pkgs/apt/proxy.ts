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
import { mergeRelayDirectives, RELAY_CONTROL_HEADER } from "@server/relay-control";
import {
  WebListProxy,
  defaultRender,
  type WebListSource,
} from "@server/pkgs/web-list";

import type { AptRepo } from "./repo";

/** Matches a content-addressed `by-hash/<algo>/<hash>` path; group 1 is the hash. */
const BY_HASH = /\/by-hash\/(?:MD5Sum|SHA1|SHA256|SHA512)\/([0-9a-f]{32,128})$/i;

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

/** Path/hash facts one published APT metadata generation provides. */
export interface AptKnownSets {
  paths: ReadonlySet<string>;
  hashes: ReadonlySet<string>;
  /** Suites the generation pinned. */
  suites: ReadonlySet<string>;
  /** When the generation stops being fresh (`expiresAt`), for cache-control. */
  validUntil?: number;
}

export interface CreateAptProxyOptions {
  /** Mount prefix for WebDAV hrefs (e.g. `/debian/`). Defaults to `/`. */
  baseHref?: string;
  /** Directory source override; omitted mirrors read upstream autoindex. */
  source?: WebListSource;
  /**
   * Authoritative path/hash sets for a snapshot mirror. Resolved per request so
   * enforcement always uses the generation currently published, never a
   * separately resolved (and possibly older) `AptRepo` index. `null` means no
   * generation exists yet; the request is then unjudged.
   */
  known?: () => Promise<AptKnownSets | null>;
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
export function createAptProxy(repo: AptRepo, opts?: CreateAptProxyOptions): WebListProxy {
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
  repo: AptRepo,
  request: Request,
  opts?: CreateAptProxyOptions,
): Promise<Response | null> {
  // Directories are handled by WebListProxy's readdir → render / JSON / WebDAV.
  if (rel === "" || rel.endsWith("/")) return null;

  const hash = rel.match(BY_HASH)?.[1]?.toLowerCase() ?? null;

  // Whose facts govern existence: a snapshot mirror's current generation, or
  // the mirror's own resolved index. Nothing authoritative → unjudged passthrough.
  let known: AptKnownSets | null = null;
  if (opts?.known) {
    known = await opts.known();
  } else {
    // Block until the index is populated so enforcement decisions use real data.
    await repo.awaitResolved();
    if (repo.ready) {
      known = {
        paths: repo.knownPaths,
        hashes: repo.knownHashes,
        suites: repo.knownSuites,
      };
    }
  }

  if (known) {
    if (hash !== null) {
      // Content-addressed: unknown means the release never named it.
      if (!known.hashes.has(hash)) return notFound();
    } else if (opts?.known) {
      // Snapshot mounts know the whole tree, including `pool/`. Any directory
      // addressed without its trailing slash redirects, so a browser or WebDAV
      // client sees one URL per directory. Unknown paths stay unjudged: the
      // generation only lists metadata it verified, and a real package body is
      // not something it can prove absent.
      if (!known.paths.has(rel)) {
        if (hasChildren(known.paths, `${rel}/`)) return directoryRedirect(request, opts?.baseHref);
        // A known suite's other signed names are not a 404: apt chooses between
        // `InRelease`, `Release` and `Release.gpg`, and a generation that only
        // verified one must not hide the fallbacks.
        const signedSibling = SIGNED_SIBLING.test(rel) && isInKnownSuite(known.suites, rel);
        if (!signedSibling && (rel === "dists" || isInKnownSuite(known.suites, rel))) {
          return notFound();
        }
      }
    } else if (rel === "dists" || isInKnownSuite(known.suites, rel)) {
      // Curated mirrors only resolve a subset of suites; the index has no
      // knowledge of the rest. Enforcement applies only to the `dists` root and
      // to suites we actually indexed — paths under an un-indexed suite pass
      // through unjudged so a curated suite list never blocks reachable suites.
      if (!known.paths.has(rel)) {
        if (hasChildren(known.paths, `${rel}/`)) return directoryRedirect(request, opts?.baseHref);
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
  // deliberately not forwarded here.
  const isHead = request.method.toUpperCase() === "HEAD";
  const init: RequestInit | undefined = isHead ? { method: "HEAD" } : undefined;
  const upstream = hash !== null
    ? await cachedfetch(repo.url(rel), init)
    : await ezfetch(repo.url(rel), init);
  const sanitized = sanitizeResponse(upstream, {
    cacheControl:
      hash !== null
        ? IMMUTABLE_CACHE_CONTROL
        : sourceCacheControl(known?.validUntil ?? repo.validUntil),
  });

  // The same generation hazard applies to the relay's default request cache:
  // tell it not to store a mutable dists index. by-hash objects are
  // content-addressed and pool payloads are not indexes, so neither gets this.
  const directives: string[] = [];
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

  if (directives.length > 0) {
    const headers = new Headers(sanitized.headers);
    headers.set(
      RELAY_CONTROL_HEADER,
      mergeRelayDirectives(headers.get(RELAY_CONTROL_HEADER), directives),
    );
    return withoutBodyIfHead(
      new Response(sanitized.body, {
        status: sanitized.status,
        statusText: sanitized.statusText,
        headers,
      }),
      isHead,
    );
  }

  return withoutBodyIfHead(sanitized, isHead);
}

/**
 * A HEAD response must carry no body. Upstream already answers HEAD without
 * one, but the read-through cache can serve a stored GET (body included) for a
 * HEAD lookup, so the body is dropped here rather than trusted.
 */
function withoutBodyIfHead(response: Response, isHead: boolean): Response {
  if (!isHead || response.body === null) return response;
  return new Response(null, {
    status: response.status,
    statusText: response.statusText,
    headers: response.headers,
  });
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
function directoryRedirect(request: Request, baseHref?: string): Response {
  const path = new URL(request.url).pathname.replace(/^\/+/, "");
  const prefix = !baseHref ? "/" : baseHref.endsWith("/") ? baseHref : `${baseHref}/`;
  return new Response(null, {
    status: 308,
    headers: { location: `${prefix}${path}/` },
  });
}
