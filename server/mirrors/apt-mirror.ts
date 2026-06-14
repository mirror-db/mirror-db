/**
 * Factory for simple APT mirrors that follow the common pattern:
 * AptRepo + createAptProxy + prefix stripping + status endpoint.
 *
 * Mirrors needing extra logic (curated suites that deserve individual files,
 * custom auth) still live under their own subdirectory.
 */

import { AptRepo } from "@server/pkgs/apt/repo";
import { createAptProxy } from "@server/pkgs/apt/proxy";
import type { Mirror } from "@server/types";

export interface AptMirrorConfig {
  /** Mirror name — also determines the path as `/<name>/`. */
  name: string;
  /** Full upstream URL (trailing `/` optional). */
  base: string;
  /** Suite list; omit to auto-discover from `dists/`. */
  suites?: string[];
}

/**
 * Create a path-routed APT mirror from a minimal config. Each mirror gets its
 * own isolate-scoped `AptRepo` resolved lazily on the first request.
 */
export function createAptMirror(config: AptMirrorConfig): Mirror {
  const prefix = `/${config.name}/`;

  const repo = new AptRepo({
    base: config.base,
    suites: config.suites,
  });

  const proxy = createAptProxy(repo, { baseHref: prefix });

  return {
    name: config.name,
    path: config.name,
    keepHTTP: true,
    fetch(request) {
      const url = new URL(request.url);
      const rel = url.pathname.slice(prefix.length);
      const inner = new Request(new URL(`/${rel}${url.search}`, url.origin), request);
      return proxy.fetch(inner);
    },
    async status() {
      await repo.awaitResolved();
      return repo.status();
    },
  };
}
