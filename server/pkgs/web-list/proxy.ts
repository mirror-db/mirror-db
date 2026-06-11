/**
 * Unified proxy for an upstream directory-listing site (Apache/nginx autoindex).
 *
 * Exposes three access modes from the same upstream data:
 * 1. JSON API: `?format=json` on any directory path.
 * 2. Web listing: directory paths rendered via an injected `render` callback.
 * 3. WebDAV (anonymous, read-only): PROPFIND + standard GET/HEAD.
 *
 * Usage:
 * ```ts
 * const proxy = new WebListProxy({
 *   baseURL: "https://cloudflaremirrors.com/debian/",
 *   render(entries, path) { return new Response(html) },
 * });
 * // Mount under a path prefix; strip the prefix before handing to fetch:
 * return proxy.fetch(request);
 * ```
 */

import type { WebListEntry } from "./parse";
import { WebListFs, type WebListFetch } from "./web-list";
import { methodNotAllowed, optionsResponse, propfindResponse } from "./webdav";

/**
 * Hook that can intercept any request before default handling runs.
 * Return a `Response` to short-circuit; return `null`/`undefined` to fall
 * through to the normal proxy logic.
 */
export type WebListFetchHook = (
  path: string,
  request: Request,
) => Response | null | undefined | Promise<Response | null | undefined>;

export interface WebListProxyOptions {
  /** Upstream base URL, e.g. `"https://cloudflaremirrors.com/debian/"`. */
  baseURL: string;
  /**
   * Render structured entries into an HTML Response for the web listing mode.
   * Receives the parsed entries and the directory path (relative, with trailing
   * slash; root is `""`).
   */
  render(entries: WebListEntry[], path: string): Response | Promise<Response>;
  /** Optional: inject fetch impl (for tests / custom caching). */
  fetchImpl?: WebListFetch;
  /**
   * Optional hook to intercept requests before normal handling.
   * Called with the relative path and original request. Return a `Response` to
   * short-circuit; return `null`/`undefined` to continue with default behavior.
   */
  fetchHook?: WebListFetchHook;
  /**
   * Base href prefix for WebDAV `<D:href>` values (e.g. `/debian/`).
   * Must end with `/`. Defaults to `"/"`.
   * Windows WebClient matches hrefs against the mount point, so this must
   * reflect the actual URL prefix the proxy is served under.
   */
  baseHref?: string;
}

const ALLOWED_METHODS = new Set(["OPTIONS", "GET", "HEAD", "PROPFIND"]);

export class WebListProxy {
  private readonly fs: WebListFs;
  private readonly render: WebListProxyOptions["render"];
  private readonly fetchHook: WebListFetchHook | undefined;
  /** The base href used in WebDAV responses (derived from upstream URL path). */
  private readonly baseHref: string;

  constructor(opts: WebListProxyOptions) {
    this.fs = new WebListFs(opts.baseURL, { fetchImpl: opts.fetchImpl });
    this.render = opts.render;
    this.fetchHook = opts.fetchHook;
    const href = opts.baseHref ?? "/";
    this.baseHref = href.endsWith("/") ? href : `${href}/`;
  }

  /**
   * Handle an incoming request. The caller is responsible for stripping any
   * mount prefix from the URL — this handler sees the **relative** path within
   * the listing root.
   *
   * The request URL's `pathname` is used as the relative path: `""` or `"/"`
   * means root, `"/dists/"` means the `dists/` subdirectory, etc.
   */
  async fetch(request: Request): Promise<Response> {
    const method = request.method.toUpperCase();

    if (!ALLOWED_METHODS.has(method)) {
      return methodNotAllowed();
    }

    if (method === "OPTIONS") {
      return optionsResponse();
    }

    const url = new URL(request.url);
    // Relative path within the listing root (strip leading `/`).
    const rel = url.pathname.replace(/^\/+/, "");

    // PROPFIND is always a directory operation — handle before the hook so
    // domain-specific file logic doesn't accidentally intercept it.
    if (method === "PROPFIND") {
      return this.handlePropfind(request, rel);
    }

    // Give the hook a chance to intercept before default handling.
    if (this.fetchHook) {
      const hooked = await this.fetchHook(rel, request);
      if (hooked) return hooked;
    }

    // GET / HEAD
    const isDir = rel === "" || rel.endsWith("/");
    const wantJson = url.searchParams.get("format") === "json";

    if (isDir || wantJson) {
      return this.handleDirectory(rel, wantJson);
    }

    // File passthrough.
    return this.fs.fetch(rel);
  }

  private async handleDirectory(rel: string, json: boolean): Promise<Response> {
    const entries = await this.fs.readdir(rel);

    if (json) {
      return Response.json({ path: rel, entries });
    }

    return this.render(entries, rel);
  }

  private async handlePropfind(request: Request, rel: string): Promise<Response> {
    // Depth header: 0 = self only, 1 = self + children, infinity not supported.
    const depthHeader = request.headers.get("depth") ?? "1";
    const depth = depthHeader === "0" ? 0 : 1;

    // Ensure the path is treated as a directory for readdir.
    const dirRel = rel === "" || rel.endsWith("/") ? rel : `${rel}/`;
    const entries = await this.fs.readdir(dirRel);

    return propfindResponse({ path: dirRel, baseHref: this.baseHref, entries }, depth);
  }
}
