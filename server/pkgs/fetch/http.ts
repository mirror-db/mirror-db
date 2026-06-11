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
 * Headers stripped from upstream responses before serving to clients. Shared by
 * both OCI and APT proxies.
 *
 * - `www-authenticate`: never expose auth challenges to anonymous clients.
 * - `set-cookie` / `set-cookie2`: digest-shared cache would replay one client's
 *   cookie to all others.
 * - `vary`: digest content is request-header-independent; dropping avoids
 *   spurious cache misses.
 * - `age`: origin-relative freshness, meaningless once re-served.
 * - hop-by-hop: `connection`, `keep-alive`, `transfer-encoding`.
 */
export const STRIP_RESPONSE_HEADERS = new Set([
  "www-authenticate",
  "set-cookie",
  "set-cookie2",
  "vary",
  "age",
  "connection",
  "keep-alive",
  "transfer-encoding",
]);

export interface SanitizeOptions {
  /** Override `Cache-Control` on 200 responses. */
  cacheControl?: string;
}

/**
 * Re-wrap an upstream response with dangerous headers stripped and optional
 * cache-control override. The body is passed through without buffering.
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
    headers.set("cache-control", opts.cacheControl);
  }
  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers,
  });
}
