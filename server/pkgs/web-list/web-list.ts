/**
 * Read-only view over an upstream directory-listing site (Apache/nginx
 * autoindex), e.g. `new WebListFs("https://cloudflaremirrors.com/debian/")`.
 *
 * `readdir` fetches + parses a listing page into structured entries; `fetch`
 * grabs an individual file/path. Both go through {@link cachedfetch} so repeated
 * reads of the same URL are served from the upstream cache. The fetch impl is
 * injectable for unit tests (the Cache API is absent under Node).
 */

import { cachedfetch } from "@server/pkgs/fetch";
import urlJoin from "url-join";

import { parseListing, type WebListEntry } from "./parse";
import type { WebListParserFactory } from "./parsers";

export type { WebListEntry, WebListEntryType } from "./parse";

export type WebListFetch = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;

export interface WebListFsOptions {
  /** Injected fetch (defaults to {@link cachedfetch}). */
  fetchImpl?: WebListFetch;
  /**
   * Parser factories to use for `readdir`. Can be:
   * - An array of factories (static, used for all paths).
   * - A function `(path) => factories[]` (dynamic, per-path selection).
   *
   * Defaults to all known parsers (table, ul, pre).
   */
  parsers?: WebListParserFactory[] | ((path: string) => WebListParserFactory[]);
}

export class WebListFs {
  /** Upstream prefix, always ending in `/`. */
  readonly base: string;
  private readonly fetchImpl: WebListFetch;
  private readonly parsers:
    | WebListParserFactory[]
    | ((path: string) => WebListParserFactory[])
    | undefined;

  constructor(base: string, opts: WebListFsOptions = {}) {
    this.base = base.endsWith("/") ? base : `${base}/`;
    this.fetchImpl = opts.fetchImpl ?? (cachedfetch as WebListFetch);
    this.parsers = opts.parsers;
  }

  /** Resolve a repo-relative path against the base. */
  url(rel = ""): string {
    const joined = urlJoin(this.base, rel);
    // Preserve trailing slash — url-join strips it but directory semantics depend on it.
    const shouldSlash = rel ? rel.endsWith("/") : this.base.endsWith("/");
    return shouldSlash && !joined.endsWith("/") ? joined + "/" : joined;
  }

  /** Fetch a single path under the base (a file, or a raw listing page). */
  fetch(rel = "", init?: RequestInit): Promise<Response> {
    return this.fetchImpl(this.url(rel), init);
  }

  /**
   * Fetch the listing at `rel` (a trailing `/` is added so the server returns
   * the directory index) and parse it into entries. Throws on a non-OK
   * response.
   */
  async readdir(rel = ""): Promise<WebListEntry[]> {
    const dir = rel === "" || rel.endsWith("/") ? rel : `${rel}/`;
    const res = await this.fetch(dir);
    if (!res.ok) {
      throw new Error(`readdir ${this.url(dir)} failed: ${res.status}`);
    }
    const factories = typeof this.parsers === "function"
      ? this.parsers(dir)
      : this.parsers;
    return parseListing(res, factories);
  }
}
