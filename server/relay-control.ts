/**
 * `X-MDB-Relay` response control header.
 *
 * The value is Cache-Control-style: comma-separated `keyword` or
 * `keyword=value` tokens. It carries response decisions the relay can only make
 * after seeing the upstream answer — currently `no-store`, `no-vary` and
 * `ttl=<seconds>` — and is deliberately absent from the mirror manifest policy
 * (see `server/relay-policy.ts`). The relay reads it and removes it before
 * delivering to a client.
 *
 * When a mirror has no explicit directive, the header is untouched. When it
 * does, the middleware merges: an existing directive always wins, so a response
 * that already said `no-store` keeps saying it.
 */

import { normalizeRelayHost } from "./relay";

export const RELAY_CONTROL_HEADER = "x-mdb-relay";

/** The relay's own hostname, the marker that a request came through a relay. */
export const RELAY_HOST_HEADER = "x-mdb-relay-host";

/**
 * Directives a mirror may declare. Unknown keywords are tolerated on the wire
 * (relays ignore them), but the Worker only emits these.
 */
export type RelayDirective = "no-store" | "no-vary" | `ttl=${number}`;

/** Split a directive list on commas outside quoted strings. */
function splitDirectives(value: string): string[] {
  const out: string[] = [];
  let current = "";
  let quoted = false;
  for (const char of value) {
    if (char === '"') quoted = !quoted;
    if (char === "," && !quoted) {
      out.push(current.trim());
      current = "";
      continue;
    }
    current += char;
  }
  out.push(current.trim());
  return out.filter(Boolean);
}

const directiveName = (directive: string): string =>
  directive.split("=", 1)[0]!.trim().toLowerCase();

/**
 * Merge `extra` into an existing directive list without overriding it.
 *
 * Existing directives keep their position and value; a keyword that is already
 * present is not added again, whatever its value. Result is deterministic and
 * suitable for an HTTP field value.
 */
export function mergeRelayDirectives(
  existing: string | null,
  extra: readonly string[],
): string {
  const seen = new Set<string>();
  const merged: string[] = [];

  for (const raw of [...splitDirectives(existing ?? ""), ...extra]) {
    const directive = raw.trim();
    if (!directive) continue;
    const name = directiveName(directive);
    if (!name || seen.has(name)) continue;
    seen.add(name);
    merged.push(directive);
  }

  return merged.join(", ");
}

/**
 * Add a mirror's relay directives to a response. Existing directives are never
 * overridden; a response without directives is returned untouched.
 */
export function withRelayDirectives(
  response: Response,
  directives?: readonly RelayDirective[],
): Response {
  if (!directives?.length) return response;

  const existing = response.headers.get(RELAY_CONTROL_HEADER);
  const merged = mergeRelayDirectives(existing, directives);
  if (merged === existing) return response;

  const headers = new Headers(response.headers);
  headers.set(RELAY_CONTROL_HEADER, merged);
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

/** True when the request arrived through a relay with a usable hostname. */
export function isRelayRequest(request: Request): boolean {
  return normalizeRelayHost(request.headers.get(RELAY_HOST_HEADER)) !== undefined;
}

/**
 * Drop a client-supplied `X-MDB-Relay` request header unless a valid relay host
 * marks the request as relay-originated.
 *
 * The header describes how the response may be cached by the relay. A direct
 * client must not be able to inject it into the mirror, and a non-relay request
 * has no relay response to control. This is the request header, never the
 * response header produced by {@link withRelayDirectives}.
 */
export function normalizeRelayRequest<T extends Request>(request: T): T {
  if (isRelayRequest(request)) return request;
  if (!request.headers.has(RELAY_CONTROL_HEADER)) return request;

  const headers = new Headers(request.headers);
  headers.delete(RELAY_CONTROL_HEADER);
  return new Request(request, { headers }) as T;
}
