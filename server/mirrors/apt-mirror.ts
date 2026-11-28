/**
 * Factory for simple APT mirrors that follow the common pattern: an
 * AptRepository selected by mode + createAptProxy + mount context + status.
 *
 * Mirrors needing extra logic (curated suites that deserve individual files,
 * custom auth) still live under their own subdirectory.
 */

import { AptRepo } from "@server/pkgs/apt/repo";
import type { AptRepository } from "@server/pkgs/apt/repository";
import { createAptProxy } from "@server/pkgs/apt/proxy";
import {
  AptSnapshotIndex,
  SnapshotListSource,
  type AptSnapshotIndexOptions,
} from "@server/pkgs/apt/snapshot";
import { aptIndexResponse, aptIndexUnavailable } from "@server/pkgs/apt/metadata";
import { WebListFs, type WebListFetch } from "@server/pkgs/web-list";
import type { Mirror } from "@server/types";

export interface AptSnapshotConfig {
  /** Override the design limits (tests; or a repository measured larger). */
  limits?: AptSnapshotIndexOptions["limits"];
  /** Injectable upstream fetch (tests). */
  fetchImpl?: WebListFetch;
}

export interface AptMirrorConfig {
  /** Mirror name — also determines the path as `/<name>/`. */
  name: string;
  /** Full upstream URL (trailing `/` optional). */
  base: string;
  /**
   * Suite list; omit to auto-discover from `dists/`.
   *
   * Required (non-empty) with `snapshot`: the repositories that need a snapshot
   * are exactly the ones whose upstream serves no directory listing, so there is
   * nothing to discover from. Configuration is validated when the mirror is
   * built, not on every request.
   */
  suites?: string[];
  /**
   * Upstream serves no directory listing: synthesise browsable directories and
   * `/<name>/.mdb/apt-index.json` from the repository's own metadata.
   */
  snapshot?: boolean | AptSnapshotConfig;
}

/** Path of the fixed metadata index endpoint, relative to the mount. */
const INDEX_PATH = ".mdb/apt-index.json";

/**
 * Create a path-routed APT mirror from a minimal config. Each mirror gets its
 * own isolate-scoped `AptRepository`: `AptRepo` for directory discovery or
 * `AptSnapshotIndex` in snapshot mode, initialized lazily on first use.
 */
export function createAptMirror(config: AptMirrorConfig): Mirror {
  const prefix = `/${config.name}/`;

  const snapshotConfig: AptSnapshotConfig | null =
    typeof config.snapshot === "object" ? config.snapshot : config.snapshot ? {} : null;

  // A snapshot cannot discover its suites: these upstreams serve no `dists/`
  // listing. Fail at construction with a clear message instead of running a
  // mount that would answer 502 forever.
  if (snapshotConfig && (config.suites?.length ?? 0) === 0) {
    throw new Error(
      `APT mirror "${config.name}": snapshot mode requires an explicit non-empty suites list`,
    );
  }

  const index = snapshotConfig
    ? new AptSnapshotIndex({
        mirror: config.name,
        base: config.base,
        suites: config.suites!,
        fetchImpl: snapshotConfig.fetchImpl,
        limits: snapshotConfig.limits,
      })
    : null;
  const repository: AptRepository = index ?? new AptRepo({
    base: config.base,
    suites: config.suites,
  });

  // A snapshot mirror synthesises listings but still streams file bodies from
  // upstream through the same WebListFs the proxy would have built.
  const source = index
    ? new SnapshotListSource(
        index,
        new WebListFs(repository.base, { fetchImpl: snapshotConfig?.fetchImpl }),
      )
    : undefined;

  const proxy = createAptProxy(repository, {
    baseHref: prefix,
    source,
    prefetchRelease: index !== null,
  });

  return {
    name: config.name,
    path: config.name,
    keepHTTP: true,
    policyTags: ["apt"],
    // An APT path names its bytes: `/dists/<suite>/Release` and
    // `/pool/.../pkg.deb` do not change with request headers, so the relay can
    // keep one variant per URL. `Vary` is still delivered unchanged.
    relayDirectives: ["no-vary"],
    async fetch(request, { relativePath: rel }) {
      const url = new URL(request.url);

      // The mount itself is a directory; a request without the trailing slash
      // would render the root listing under a URL its links cannot resolve from.
      // Snapshot mounts only: the other APT mounts keep their existing routing.
      if (index && url.pathname === `/${config.name}`) {
        return new Response(null, {
          status: 308,
          headers: { location: `${url.pathname}/` },
        });
      }

      if (index && (rel === INDEX_PATH || rel.startsWith(".mdb/"))) {
        if (rel !== INDEX_PATH) {
          return new Response("Not Found", {
            status: 404,
            headers: { "cache-control": "no-store" },
          });
        }
        const snapshot = await index.awaitSnapshot();
        return snapshot ? aptIndexResponse(snapshot, request) : aptIndexUnavailable(index.lastError);
      }

      return proxy.fetch(request, rel);
    },
    async status() {
      const facts = await repository.facts();
      return facts?.status ?? { health: "initializing" as const };
    },
  };
}
