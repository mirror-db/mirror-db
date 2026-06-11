import { env, waitUntil } from "cloudflare:workers";

import type { Mirror } from "@server/types";

import { type Credentials, type KvLike } from "./creds";
import { defaultCache, proxyRegistryRequest } from "./registry-proxy";

export interface OciHostOptions {
  host: string;
  /** Upstream registry host, e.g. `"ghcr.io"`. */
  upstream: string;
  /** Optional path rewrite (e.g. Docker Hub `library/` normalization). */
  rewritePath?: (path: string) => string;
  /** Optional credential resolver for authenticated upstreams. */
  getCredentials?: () => Promise<Credentials | null>;
}

/**
 * Build a "no login required" mirror host for a standard OCI registry. The
 * proxy follows each registry's own token-auth challenge, so anonymous public
 * pulls work out of the box; pass `getCredentials` for authenticated upstreams.
 */
export function createRegistryHost(opts: OciHostOptions): Mirror {
  const tokenCachePrefix = `token:${opts.host}:`;

  const proxy = (request: Request): Promise<Response> =>
    proxyRegistryRequest(request, {
      upstream: opts.upstream,
      getCredentials: opts.getCredentials,
      rewritePath: opts.rewritePath,
      kv: env.kv as unknown as KvLike,
      tokenCachePrefix,
      cache: defaultCache(),
      waitUntil,
    });

  // Proxy every path, not just `/v2/`: some registries (e.g. gcr.io) answer a
  // blob request with a *relative* redirect to an on-host download path like
  // `/artifacts-downloads/...`, which the client resolves against this mirror.
  // Those follow-up requests still need upstream auth, so they must be proxied.
  return { name: opts.host, host: opts.host, fetch: proxy };
}
