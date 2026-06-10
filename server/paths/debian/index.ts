import { AptRepo } from "@server/pkgs/apt/repo";
import { defaultCache, proxyAptRequest } from "@server/pkgs/apt/proxy";

import type { PathHost } from "../index";

const PREFIX = "/debian/";

/**
 * Fallback suites parsed when the upstream `dists/` listing can't be read
 * (directory indexing disabled, fetch error). The resolver prefers the live
 * listing; this just keeps the index useful otherwise.
 */
const DEFAULT_DEBIAN_SUITES = [
  "stable",
  "testing",
  "unstable",
  "sid",
  "trixie",
  "trixie-updates",
  "trixie-backports",
  "bookworm",
  "bookworm-updates",
  "bookworm-backports",
];

// One index per isolate, resolved lazily on the first request.
const repo = new AptRepo({
  base: "https://cloudflaremirrors.com/debian/",
  suites: DEFAULT_DEBIAN_SUITES,
});

/** Debian APT mirror, served at `mirs.uk/debian/`. */
export default {
  prefix: PREFIX,
  fetch(request, ctx) {
    repo.ensureResolved(ctx ? ctx.waitUntil.bind(ctx) : undefined);

    const rel = new URL(request.url).pathname.slice(PREFIX.length);
    return proxyAptRequest(request, {
      rel,
      repo,
      cache: defaultCache(),
      waitUntil: ctx ? ctx.waitUntil.bind(ctx) : undefined,
    });
  },
} satisfies PathHost;
