/**
 * Mirror manifest served at `/api/mirrors` for relay discovery.
 *
 * The body is deterministic for an unchanged mirror set and policy table, so a
 * SHA-256 digest of the serialized JSON is a stable strong ETag. Relays fetch
 * once with GET, then poll with HEAD + `If-None-Match`.
 */

import { mirrors } from "@server/mirrors";
import { relayPolicies, type RelayPolicy } from "@server/relay-policy";

export interface ManifestMirror {
  name: string;
  host?: string;
  path?: string;
  keepHTTP?: true;
  policy_tags: string[];
}

export interface MirrorManifest {
  mirrors: ManifestMirror[];
  policies: RelayPolicy[];
}

export function buildMirrorManifest(): MirrorManifest {
  return {
    mirrors: mirrors.map((m) => ({
      name: m.name,
      ...(m.host && { host: m.host }),
      ...(m.path && { path: m.path }),
      ...(m.keepHTTP && { keepHTTP: true }),
      policy_tags: m.policyTags ?? [],
    })),
    policies: relayPolicies,
  };
}

/** Quoted lowercase hex SHA-256 of the manifest body. */
export async function manifestEtag(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  const hex = Array.from(new Uint8Array(digest), (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
  return `"${hex}"`;
}

/**
 * Whether an `If-None-Match` header selects `etag`. Handles the comma-separated
 * list form, the `W/` weak prefix, and `*`.
 */
export function etagMatches(header: string | null, etag: string): boolean {
  if (!header) return false;
  return header.split(",").some((part) => {
    const candidate = part.trim().replace(/^W\//, "");
    return candidate === "*" || candidate === etag;
  });
}

export async function mirrorManifestResponse(request: Request): Promise<Response> {
  const body = JSON.stringify(buildMirrorManifest());
  const bytes = new TextEncoder().encode(body);
  const etag = await manifestEtag(bytes);

  if (etagMatches(request.headers.get("if-none-match"), etag)) {
    return new Response(null, { status: 304, headers: { etag } });
  }

  const headers: Record<string, string> = {
    "content-type": "application/json",
    etag,
  };

  if (request.method === "HEAD") {
    headers["content-length"] = String(bytes.byteLength);
    return new Response(null, { status: 200, headers });
  }

  return new Response(body, { status: 200, headers });
}
