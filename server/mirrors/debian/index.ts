import { AptRepo } from "@server/pkgs/apt/repo";
import { createAptProxy } from "@server/pkgs/apt/proxy";
import type { Mirror } from "@server/types";

const PREFIX = "/debian/";

/**
 * Suites to index. This curated set is intentional, not a fallback: the live
 * `dists/` listing enumerates 40+ suites (experimental, rc-buggy, numbered
 * aliases, …), and resolving them all in one Worker invocation overruns the
 * CPU/time budget so the index never goes ready. Keep this to what's worth
 * mirroring; the resolver only sniffs the listing when this list is empty.
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

const proxy = createAptProxy(repo, { baseHref: PREFIX });

/** Debian APT mirror, served at `mirs.uk/debian/`. */
export default {
  name: "debian",
  path: PREFIX,
  fetch(request) {
    // Strip the mount prefix so WebListProxy sees relative paths.
    const url = new URL(request.url);
    const rel = url.pathname.slice(PREFIX.length);
    const inner = new Request(new URL(`/${rel}${url.search}`, url.origin), request);
    return proxy.fetch(inner);
  },
  async status() {
    await repo.awaitResolved();
    return repo.status();
  },
} satisfies Mirror;
