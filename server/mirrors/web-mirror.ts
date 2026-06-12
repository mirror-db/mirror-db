/**
 * Factories for non-APT path-routed mirrors:
 *
 * - **Web list mirrors** — upstream serves Apache/nginx autoindex; we expose
 *   directory browsing (HTML, JSON, WebDAV) + file passthrough via WebListProxy.
 * - **Passthrough mirrors** — no directory listing, just forward all requests
 *   to upstream with shared caching + header sanitization.
 */

import { WebListProxy } from "@server/pkgs/web-list/proxy";
import { defaultRender } from "@server/pkgs/web-list/render";
import type { WebListParserFactory } from "@server/pkgs/web-list/parsers";
import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";
import type { Mirror } from "@server/types";
import urlJoin from "url-join";

export interface WebMirrorConfig {
  /** Mirror name — determines the path as `/<name>/`. */
  name: string;
  /** Full upstream URL (trailing `/` recommended). */
  base: string;
  /**
   * Parser factories for listing parsing. Can be a static array or a function
   * `(path) => factories[]` for per-path selection. Omit to use all known parsers.
   */
  parsers?: WebListParserFactory[] | ((path: string) => WebListParserFactory[]);
}

/**
 * Create a path-routed mirror backed by WebListProxy. The upstream must serve
 * directory listings (autoindex) for browsing to work.
 */
export function createWebListMirror(config: WebMirrorConfig): Mirror {
  const prefix = `/${config.name}/`;

  const proxy = new WebListProxy({
    baseURL: config.base,
    render: defaultRender,
    baseHref: prefix,
    parsers: config.parsers,
  });

  return {
    name: config.name,
    path: config.name,
    fetch(request) {
      const url = new URL(request.url);
      const rel = url.pathname.slice(prefix.length);
      const inner = new Request(new URL(`/${rel}${url.search}`, url.origin), request);
      return proxy.fetch(inner);
    },
  };
}

/**
 * Create a path-routed mirror that transparently proxies all requests to the
 * upstream. No directory listing — just cached fetch + header sanitization.
 */
export function createPassthroughMirror(config: WebMirrorConfig): Mirror {
  const prefix = `/${config.name}/`;

  return {
    name: config.name,
    path: config.name,
    async fetch(request) {
      const url = new URL(request.url);
      const rel = url.pathname.slice(prefix.length);
      const target = urlJoin(config.base, rel) + url.search;
      const upstream = await cachedfetch(target);
      return sanitizeResponse(upstream);
    },
  };
}
