import debian from "./debian";

/**
 * A path-prefix host served on the bare `@` subdomain (`mirs.uk/<prefix>...`).
 *
 * Analogous to an `OciHost`, but routed by URL path prefix instead of
 * subdomain. Hosts needing their own logic (the APT repos) live in a
 * subdirectory; the entry router matches by longest prefix.
 */
export interface PathHost {
  /** Path prefix this host serves, leading + trailing slash, e.g. `"/debian/"`. */
  prefix: string;
  fetch: (
    request: Request,
    ctx?: ExecutionContext,
  ) => Promise<Response> | Response;
}

/** All path-prefix hosts, sorted longest-prefix-first for unambiguous matching. */
export const paths: PathHost[] = [debian].sort(
  (a, b) => b.prefix.length - a.prefix.length,
);

/** Find the path host whose prefix matches `pathname`, or undefined. */
export function matchPath(pathname: string): PathHost | undefined {
  return paths.find((p) => pathname.startsWith(p.prefix));
}
