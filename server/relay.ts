import { Context } from "hono";
import { env } from "cloudflare:workers";

/** Header the relay client sends to declare its public hostname. */
const RELAY_HOST_HEADER = "x-mdb-relay-host";

/**
 * A relay host is a bare DNS name: lowercase letters, digits, dots and hyphens.
 * No scheme, no port, no path, no userinfo.
 */
const RELAY_HOST_PATTERN = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$/;
const MAX_HOST_LENGTH = 253;

/**
 * Accept the header only when it is a bare hostname. The project is open, so
 * anyone can send this header; its value may shape URLs the client asked for,
 * but must never be able to change a URL's structure (no `/`, no `:`, no `@`).
 */
export function normalizeRelayHost(raw: string | null): string | undefined {
  if (!raw) return;
  const host = raw.trim().toLowerCase().replace(/\.$/, "");
  if (!host || host.length > MAX_HOST_LENGTH) return;
  return RELAY_HOST_PATTERN.test(host) ? host : undefined;
}

/**
 * Resolve the host the client actually reached us on.
 *
 * `request.url` is always the Worker's own origin — the host the user dialled
 * only travels in `x-mdb-relay-host` when the request came through a relay. Any
 * absolute reference in a response (npm tarball URLs, Helm chart URLs, install
 * script endpoints) must be built from `origin`, or the client leaves the relay
 * on its next request.
 */
export const relayInfo = (reqOrCtx: Request | Context) => {
  const request = reqOrCtx instanceof Request ? reqOrCtx : reqOrCtx.req.raw;
  const relayHost = normalizeRelayHost(request.headers.get(RELAY_HOST_HEADER));
  const baseDomain = relayHost || env.BASE_DOMAIN;

  const origin = relayHost
    ? `https://${relayHost}`
    : new URL(request.url).origin;

  return { baseDomain, origin };
};

export type RelayContext = ReturnType<typeof relayInfo>;
