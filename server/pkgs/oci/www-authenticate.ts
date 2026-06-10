/**
 * Parser for the `WWW-Authenticate` challenge header returned by OCI
 * registries (RFC 7235). Registries such as Docker Hub answer unauthenticated
 * requests with e.g.:
 *
 *   WWW-Authenticate: Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"
 *
 * We parse it to learn where to fetch a bearer token and with which scope.
 */

export interface AuthChallenge {
  scheme: string;
  params: Record<string, string>;
}

/**
 * Parse a single `WWW-Authenticate` header value.
 *
 * Returns `null` when the header is missing, empty, or has no recognizable
 * `scheme key=value, ...` shape. Only the first scheme is parsed (registries
 * emit exactly one).
 */
export function parseWwwAuthenticate(
  header: string | null | undefined,
): AuthChallenge | null {
  if (!header) return null;

  const trimmed = header.trim();
  const spaceIdx = trimmed.indexOf(" ");
  if (spaceIdx === -1) return null;

  const scheme = trimmed.slice(0, spaceIdx);
  const rest = trimmed.slice(spaceIdx + 1).trim();
  if (!scheme || !rest) return null;

  const params: Record<string, string> = {};
  // Match `key="quoted value"` or `key=token`, separated by commas/whitespace.
  const re = /([A-Za-z0-9_-]+)\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^",\s]+))/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(rest)) !== null) {
    const key = match[1].toLowerCase();
    const value = match[2] !== undefined ? match[2].replace(/\\(.)/g, "$1") : match[3];
    params[key] = value ?? "";
  }

  if (Object.keys(params).length === 0) return null;

  return { scheme, params };
}
