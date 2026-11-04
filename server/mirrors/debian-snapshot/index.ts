/**
 * Debian snapshot mirror — read-through access to `snapshot.debian.org`.
 *
 * The upstream is the Debian archive's time machine: `/archive/<archive>/
 * <YYYYMMDDTHHMMSSZ>/…` is a full `dists/ + pool/` tree pinned to a moment.
 * Mirroring it wholesale is not an option (the history is PB-scale), so this is
 * a **read-through cache**: whatever a client asks for is fetched once and then
 * served from the Cache API.
 *
 * Two upstream behaviours shape the design:
 *
 * - Every file answers a `302` to `/file/<sha1>/<name>`, which is immutable and
 *   cacheable for a year. Redirects are followed here so clients only ever see
 *   `mirs.uk` URLs.
 * - A timestamp between two snapshots snaps to the nearest earlier one, and a
 *   timestamp **in the future** resolves to the newest snapshot. That makes the
 *   response for a fixed archive+timestamp+path stable over time — except for
 *   future timestamps, which keep moving, so those are never cached.
 *
 * Only `archive/<known archive>/<timestamp>/…` and `mr/…` are exposed: the
 * calendar, package search and `/binary/` pages are browsing UI for the upstream
 * site, and proxying them would turn this mirror into a crawler.
 */

import { waitUntil as workerWaitUntil } from "cloudflare:workers";

import {
  CFCacheLimit,
  mdbfetch,
  parseContentLength,
  sanitizeResponse,
} from "@server/pkgs/fetch";
import type { Mirror } from "@server/types";

export const UPSTREAM = "https://snapshot.debian.org/";
export const PREFIX = "/debian-snapshot/";

/** Every archive the upstream serves. */
export const ARCHIVES = new Set([
  "debian",
  "debian-archive",
  "debian-backports",
  "debian-debug",
  "debian-ports",
  "debian-security",
  "debian-security-debug",
  "debian-volatile",
]);

/** Snapshot timestamps are `YYYYMMDDTHHMMSSZ` (always UTC). */
const TIMESTAMP = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/;

/** Content-addressed files: immutable, safe to cache forever. */
const FILE_PATH = /^\/file\/[0-9a-f]{40}\//;

/** Cached snapshot files never change under their archive+timestamp path. */
const IMMUTABLE_CACHE_CONTROL = "public, max-age=31536000, immutable";

/**
 * Everything else (the `mr` API, future timestamps) may change: keep the
 * upstream's own five-minute lifetime rather than inventing one.
 */
const SHORT_CACHE_CONTROL = "public, max-age=600";

export interface SnapshotCache {
  match(request: Request): Promise<Response | undefined>;
  put(request: Request, response: Response): Promise<void>;
}

export interface DebianSnapshotOptions {
  fetchImpl?: (input: Request) => Promise<Response>;
  cache?: SnapshotCache;
  waitUntil?: (promise: Promise<unknown>) => void;
  /** Clock override for tests (decides whether a timestamp is in the future). */
  now?: () => number;
  maxCacheBytes?: number;
}

/** Parse a snapshot timestamp, or undefined when it is not one. */
export function parseTimestamp(value: string): Date | undefined {
  const m = TIMESTAMP.exec(value);
  if (!m) return;
  const [, y, mo, d, h, mi, s] = m.map(Number) as unknown as number[];
  const date = new Date(Date.UTC(y, mo - 1, d, h, mi, s));
  // `Date.UTC` rolls February 30th into March; compare the fields back so an
  // impossible date is rejected instead of silently becoming another one.
  if (
    date.getUTCFullYear() !== y ||
    date.getUTCMonth() !== mo - 1 ||
    date.getUTCDate() !== d ||
    date.getUTCHours() !== h ||
    date.getUTCMinutes() !== mi ||
    date.getUTCSeconds() !== s
  ) {
    return;
  }
  return date;
}

/**
 * Map a request path (relative to the mount) to the upstream URL, or undefined
 * when it is not something this mirror serves.
 */
export function resolveTarget(rel: string, search = ""): URL | undefined {
  if (rel.includes("..")) return;

  const [first, second, third] = rel.split("/");

  if (first === "mr") {
    const url = new URL(rel, UPSTREAM);
    url.search = search;
    return url;
  }

  if (first !== "archive") return;
  if (!second || !ARCHIVES.has(second)) return;
  if (!third || !parseTimestamp(third)) return;

  const url = new URL(rel, UPSTREAM);
  url.search = search;
  return url;
}

const notFound = () => new Response("Not Found", { status: 404 });

/** Only `Range` is forwarded: everything else is chosen by the upstream. */
function upstreamHeaders(request: Request): Headers {
  const headers = new Headers();
  const range = request.headers.get("range");
  if (range) headers.set("range", range);
  return headers;
}

/** True when the response is small enough for the Cache API to accept it. */
function withinCacheLimit(resp: Response, limit: number): boolean {
  const bytes = parseContentLength(resp.headers);
  return bytes !== undefined && bytes <= limit;
}

export function createDebianSnapshot(opts: DebianSnapshotOptions = {}): Mirror {
  const fetchImpl = opts.fetchImpl ?? ((input: Request) => mdbfetch(input));
  const waitUntil = opts.waitUntil ?? workerWaitUntil;
  const now = opts.now ?? Date.now;
  const maxCacheBytes = opts.maxCacheBytes ?? CFCacheLimit;
  const injectedCache = opts.cache;

  const cache = async (): Promise<SnapshotCache | undefined> =>
    injectedCache ??
    (typeof caches !== "undefined"
      ? (caches.default as unknown as SnapshotCache)
      : undefined);

  return {
    name: "debian-snapshot",
    path: "debian-snapshot",
    async fetch(request) {
      const url = new URL(request.url);
      if (!url.pathname.startsWith(PREFIX)) return notFound();

      const rel = url.pathname.slice(PREFIX.length);
      const target = resolveTarget(rel, url.search);
      if (!target) return notFound();

      // Timestamps in the future resolve to "newest snapshot", which keeps
      // moving — a year-long entry would freeze that answer.
      const timestamp = parseTimestamp(rel.split("/")[2] ?? "");
      const stable = rel.startsWith("mr/")
        ? false
        : !!timestamp && timestamp.getTime() <= now();

      const hasRange = request.headers.has("range");
      const cacheable =
        stable && !hasRange && request.method === "GET";

      const store = cacheable ? await cache() : undefined;
      const key = new Request(target.toString());

      if (store) {
        const hit = await store.match(key);
        if (hit) return hit;
      }

      let upstream: Response;
      try {
        upstream = await fetchImpl(
          new Request(target.toString(), {
            method: request.method,
            headers: upstreamHeaders(request),
            // The 302 to /file/<sha1>/… is followed here, so clients never see
            // an upstream origin.
            redirect: "follow",
          }),
        );
      } catch {
        return new Response("Bad Gateway", { status: 502 });
      }

      // A `302` that survived `follow` (upstream refused to follow) means the
      // path leads somewhere this mirror does not serve.
      if (upstream.status >= 300 && upstream.status < 400) {
        return new Response("Not Found", { status: 404 });
      }

      const final = sanitizeResponse(upstream, {
        cacheControl: cacheable ? IMMUTABLE_CACHE_CONTROL : SHORT_CACHE_CONTROL,
      });

      if (
        store &&
        final.status === 200 &&
        withinCacheLimit(final, maxCacheBytes)
      ) {
        // Store the sanitised copy: the Cache API refuses a response carrying
        // `Vary`, and only the sanitised headers are meant to reach clients.
        const write = store.put(key, final.clone()).catch(() => {});
        waitUntil(write);
      }

      return final;
    },
  };
}

export const debianSnapshot = createDebianSnapshot();
