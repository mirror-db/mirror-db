/**
 * Pigsty APT mirror — the whole of `https://repo.pigsty.io/apt/` at `/pigsty/`.
 *
 * Upstream is one APT *root* holding two independent layouts, not one repo:
 *
 * - `infra/dists/generic/…` + `infra/pool/main/…`
 * - `pgsql/<codename>/dists/<codename>/…` + `pgsql/<codename>/pool/main/…`
 *
 * Each `pgsql/<codename>` is its own repository root, so `AptRepo` (which models
 * `base` + `dists/<suite>`) cannot address both from a single mount. A
 * path-preserving passthrough can: the client's path is forwarded verbatim
 * after the mount prefix, and `Range` travels through the shared proxy chain.
 *
 * Upstream answers `Cache-Control: no-store` on everything, so `cachedfetch`
 * declines to store these responses. That is expected: this mirror is a relay
 * for packages, not a cache.
 */

import { upstreamProxy } from "@server/mirrors/proxy";
import type { Mirror } from "@server/types";

const UPSTREAM = "https://repo.pigsty.io/apt/";
const PATH = "pigsty";
const PREFIX = `/${PATH}`;

const proxy = upstreamProxy({ url: UPSTREAM, prefix: PREFIX });

export const pigsty: Mirror = {
  name: PATH,
  path: PATH,
  keepHTTP: true,
  policyTags: ["apt"],
  fetch: (request) => proxy.apply(request),
};
