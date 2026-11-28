interface RangeInfo {
  start?: number;
  end?: number;
  length?: number;
  suffix: boolean;
}

export function parseRange(headersInit?: HeadersInit): RangeInfo | undefined {
  if (!headersInit) return;
  const headers = new Headers(headersInit);

  const encoded = headers.get("range");

  if (encoded === null) return;
  if (!encoded.startsWith("bytes=")) throw new Error("range units must be bytes");
  if (encoded.includes(",")) throw new Error("only single range supported");

  const parts = encoded.split("bytes=")[1]?.split("-") ?? [];
  if (parts.length !== 2)
    throw new Error(
      "Not supported to skip specifying the beginning/ending byte at this time",
    );

  const start = Number(parts[0]);
  const end = Number(parts[1]);

  if (Number.isNaN(start) && Number.isNaN(end)) return;
  if (Number.isNaN(end)) return { start, suffix: false };
  if (Number.isNaN(start)) return { length: end, suffix: true };
  if (start > end) return;

  return { start, end, length: end + 1 - start, suffix: false };
}

export function parseContentLength(headersInit?: HeadersInit): number | undefined {
  if (!headersInit) return;
  const headers = new Headers(headersInit);

  const contentLength = headers.get("content-length");
  if (!contentLength || Number.isNaN(Number(contentLength))) return;
  return Number(contentLength);
}

// ── Response sanitization ───────────────────────────────────────────────────

/**
 * Headers stripped from upstream responses before serving to clients. Applied
 * by `cachedfetch` on every delivery and by mirror-specific proxy layers.
 *
 * - `www-authenticate`: never expose auth challenges to anonymous clients.
 * - `set-cookie` / `set-cookie2`: digest-shared cache would replay one client's
 *   cookie to all others.
 * - `age`: origin-relative freshness, meaningless once re-served.
 * - `x-mdb-relay`: a response control meant for our own relay. A third-party
 *   upstream must not be able to inject a directive; the front-door middleware
 *   adds the mirror's own directives after sanitising.
 * - hop-by-hop: `connection`, `keep-alive`, `transfer-encoding`.
 *
 * `vary` is **not** stripped: a relay needs it to build cache variants, and a
 * direct client must see the same representation negotiation the source asked
 * for. Responses carrying `Vary` bypass the Worker's read-through cache rather
 * than being stored without it (see {@link STRIP_STORED_HEADERS}).
 */
export const STRIP_RESPONSE_HEADERS = new Set([
  "www-authenticate",
  "set-cookie",
  "set-cookie2",
  "x-mdb-relay",
  "age",
  "connection",
  "keep-alive",
  "transfer-encoding",
]);

/**
 * Headers additionally dropped before writing a copy into the Cache API.
 *
 * Only the OCI digest cache uses this: content-addressed data must not vary, so
 * a `Vary` on a blob is dropped rather than disabling a digest hit. The shared
 * read-through cache and the snapshot mirror skip a response carrying `Vary`
 * entirely, so a later hit cannot deliver a different header set than the miss.
 */
export const STRIP_STORED_HEADERS = new Set([
  ...STRIP_RESPONSE_HEADERS,
  "vary",
]);

export interface SanitizeOptions {
  /** Override `Cache-Control` on 200 responses, merging with the upstream value. */
  cacheControl?: string;
}

/**
 * Directives that describe cacheability or lifetime. An override owns these:
 * the mirror's value replaces whatever the source said. Everything else — most
 * importantly `no-transform` — is preserved, because it constrains how the
 * response may be re-encoded rather than how long it may be stored.
 */
const CACHEABILITY_DIRECTIVES = new Set([
  "public",
  "private",
  "no-store",
  "no-cache",
  "max-age",
  "s-maxage",
  "must-revalidate",
  "proxy-revalidate",
  "immutable",
  "stale-while-revalidate",
  "stale-if-error",
]);

/** Split a `Cache-Control`-style value on commas outside quoted strings. */
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
 * Apply `override` to an upstream `Cache-Control`, replacing the directives that
 * express cacheability while keeping orthogonal ones such as `no-transform`.
 * Order is upstream-preserved directives first, then the override.
 */
export function mergeCacheControl(
  upstream: string | null,
  override: string,
): string {
  const over = splitDirectives(override);
  const overNames = new Set(over.map(directiveName));
  const preserved = splitDirectives(upstream ?? "").filter((directive) => {
    const name = directiveName(directive);
    return !CACHEABILITY_DIRECTIVES.has(name) && !overNames.has(name);
  });
  return [...preserved, ...over].join(", ");
}

/**
 * Re-wrap an upstream response with dangerous headers stripped and an optional
 * cache-control override merged into the upstream value. The body is passed
 * through without buffering.
 */
export function sanitizeResponse(
  upstream: Response,
  opts?: SanitizeOptions,
): Response {
  const headers = new Headers();
  upstream.headers.forEach((value, key) => {
    if (!STRIP_RESPONSE_HEADERS.has(key.toLowerCase())) {
      headers.set(key, value);
    }
  });
  if (opts?.cacheControl && upstream.status === 200) {
    headers.set(
      "cache-control",
      mergeCacheControl(upstream.headers.get("cache-control"), opts.cacheControl),
    );
  }
  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers,
  });
}
