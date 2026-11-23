/**
 * Bare-domain `/relaytest/<case>` — a deterministic upstream for exercising the
 * relay cache from the outside.
 *
 * Each case returns a fixed status, header set and body, so a test relay can
 * point a temporary mirror at it and observe fresh hits, revalidation, variants,
 * encoding, ranges, no-store and digest handling. These endpoints are **not**
 * mirrors: they never appear in the manifest, are never cached by the Worker,
 * and carry no production policy.
 *
 * GET and HEAD share headers and status; HEAD has no body and never evaluates
 * `Range` (there is nothing to slice).
 */

const CASES = [
  "fresh",
  "revalidate",
  "ttl",
  "vary",
  "encoding",
  "range",
  "no-store",
  "digest",
] as const;

type RelayTestCase = (typeof CASES)[number];

const TEXT = { "content-type": "text/plain; charset=utf-8" } as const;
const BINARY = { "content-type": "application/octet-stream" } as const;

const encoder = new TextEncoder();

const caseBody = (name: RelayTestCase): string => `relaytest: ${name} v1\n`;

/** 256 deterministic bytes, for single-range requests. */
const RANGE_BODY = Uint8Array.from({ length: 256 }, (_, i) => i);

/** SHA-256 of `relaytest: digest v1\n`, base64 and lowercase hex. */
const DIGEST_BASE64 = "mAO21+d/Mj5+Dq2tDVZ7TAp+jOAAIkZAF6OyzfDMsus=";
const DIGEST_HEX =
  "9803b6d7e77f323e7e0eadad0d567b4c0a7e8ce00022464017a3b2cdf0ccb2eb";

/** Return `body`, or no body for HEAD, with identical status and headers. */
function respond(
  request: Request,
  body: BodyInit | null,
  init: ResponseInit,
): Response {
  const headers = new Headers(init.headers);
  const status = init.status ?? 200;
  if (request.method === "HEAD") {
    return new Response(null, { status, statusText: init.statusText, headers });
  }
  return new Response(body, { status, statusText: init.statusText, headers });
}

function textResponse(
  request: Request,
  body: string,
  headers: Record<string, string>,
  status = 200,
): Response {
  const bytes = encoder.encode(body);
  return respond(request, body, {
    status,
    headers: {
      ...TEXT,
      "content-length": String(bytes.byteLength),
      ...headers,
    },
  });
}

/** Parse `If-None-Match`, including weak prefixes and the `*` wildcard. */
function ifNoneMatch(request: Request, etag: string): boolean {
  const header = request.headers.get("if-none-match");
  if (!header) return false;
  return header.split(",").some((part) => {
    const candidate = part.trim().replace(/^W\//, "");
    return candidate === "*" || candidate === etag;
  });
}

function notModified(request: Request, etag: string, headers: Record<string, string>): Response {
  return respond(request, null, {
    status: 304,
    headers: { etag, ...headers },
  });
}

/** `q` value for a coding in `Accept-Encoding`; absent defaults to 1 for identity. */
function quality(header: string, coding: string): number {
  let wildcard: number | undefined;
  for (const raw of header.split(",")) {
    const [name, ...params] = raw.trim().toLowerCase().split(";");
    const qParam = params.find((p) => p.trim().startsWith("q="));
    const q = qParam ? Number(qParam.trim().slice(2)) : 1;
    const value = Number.isFinite(q) ? q : 1;
    if (name === coding) return value;
    if (name === "*") wildcard = value;
  }
  if (wildcard !== undefined) return wildcard;
  return coding === "identity" ? 1 : 0;
}

/** Gzip-encode bytes with the platform `CompressionStream`. */
async function gzip(bytes: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([bytes]).stream().pipeThrough(new CompressionStream("gzip"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

interface ByteRange {
  start: number;
  end: number;
}

/**
 * Parse one `bytes=` range. Returns `null` when there is no usable range (the
 * full representation should be served) and `"invalid"` for an unsatisfiable
 * one (416).
 */
function parseRange(
  header: string | null,
  size: number,
): ByteRange | "invalid" | null {
  if (!header) return null;
  if (!header.startsWith("bytes=")) return null;
  const spec = header.slice("bytes=".length).trim();
  // Multi-range is not part of this test upstream; ignore it as an unsupported
  // range form rather than answering a partial multipart body.
  if (spec.includes(",")) return null;

  // Validate the grammar first. A malformed specification is ignored (a 200
  // with the full body); only a well-formed range that selects no byte is 416.
  const match = /^(\d*)-(\d*)$/.exec(spec);
  if (!match) return null;
  const [, startText, endText] = match;
  if (startText === "" && endText === "") return null;

  if (startText === "") {
    const suffix = Number(endText);
    if (suffix <= 0) return "invalid";
    return { start: Math.max(0, size - suffix), end: size - 1 };
  }

  const start = Number(startText);
  if (start >= size) return "invalid";

  if (endText === "") return { start, end: size - 1 };

  const end = Number(endText);
  if (end < start) return "invalid";
  return { start, end: Math.min(end, size - 1) };
}

async function fresh(request: Request): Promise<Response> {
  const etag = '"fresh-v1"';
  const headers = {
    etag,
    "cache-control": "public, max-age=3600",
  };
  if (ifNoneMatch(request, etag)) return notModified(request, etag, headers);
  return textResponse(request, caseBody("fresh"), headers);
}

function revalidate(request: Request): Response {
  const etag = '"revalidate-v1"';
  const headers = {
    etag,
    "cache-control": "no-cache",
  };
  if (ifNoneMatch(request, etag)) return notModified(request, etag, headers);
  return textResponse(request, caseBody("revalidate"), headers);
}

function ttl(request: Request): Response {
  // Deliberately no `Cache-Control` and no `Expires`: only the relay's
  // `X-MDB-Relay: ttl=<seconds>` directive can supply fallback freshness.
  return textResponse(request, caseBody("ttl"), {});
}

function vary(request: Request): Response {
  const value = request.headers.get("x-relaytest-variant") ?? "";
  const etag = `"vary-${encodeURIComponent(value) || "default"}"`;
  const headers = {
    etag,
    vary: "x-relaytest-variant",
    "cache-control": "public, max-age=3600",
  };
  if (ifNoneMatch(request, etag)) return notModified(request, etag, headers);
  return textResponse(
    request,
    `relaytest: vary ${value || "default"}\n`,
    headers,
  );
}

async function encoding(request: Request): Promise<Response> {
  const body = encoder.encode(caseBody("encoding"));
  const accept = request.headers.get("accept-encoding");

  // No header: any coding is acceptable, and identity needs no transform.
  const gzipQ = accept ? quality(accept, "gzip") : 0;
  const identityQ = accept ? quality(accept, "identity") : 1;

  let selected: "identity" | "gzip";
  if (gzipQ > 0 && gzipQ >= identityQ) selected = "gzip";
  else if (identityQ > 0) selected = "identity";
  else {
    return respond(request, null, {
      status: 406,
      headers: { ...TEXT, vary: "accept-encoding" },
    });
  }

  const baseHeaders = {
    vary: "accept-encoding",
    "cache-control": "public, max-age=3600",
  };

  if (selected === "gzip") {
    const gz = await gzip(body);
    return respond(request, gz, {
      headers: {
        ...BINARY,
        ...baseHeaders,
        etag: '"encoding-gzip-v1"',
        "content-encoding": "gzip",
        "content-length": String(gz.byteLength),
      },
    });
  }

  return respond(request, body, {
    headers: {
      ...BINARY,
      ...baseHeaders,
      etag: '"encoding-identity-v1"',
      "content-length": String(body.byteLength),
    },
  });
}

function range(request: Request): Response {
  const etag = '"range-v1"';
  const headers = {
    ...BINARY,
    etag,
    "cache-control": "public, max-age=3600",
    "accept-ranges": "bytes",
  };

  // HEAD has no body, so there is nothing to slice; it always describes the
  // full representation.
  if (request.method === "HEAD") {
    return respond(request, null, {
      headers: { ...headers, "content-length": String(RANGE_BODY.byteLength) },
    });
  }

  // `If-Range` must name the current strong validator, or the range is ignored.
  const ifRange = request.headers.get("if-range");
  const usable = ifRange === null || ifRange === etag;
  const parsed = usable
    ? parseRange(request.headers.get("range"), RANGE_BODY.byteLength)
    : null;

  if (parsed === "invalid") {
    return respond(request, null, {
      status: 416,
      headers: {
        ...headers,
        "content-range": `bytes */${RANGE_BODY.byteLength}`,
      },
    });
  }

  if (parsed === null) {
    return respond(request, RANGE_BODY, {
      headers: { ...headers, "content-length": String(RANGE_BODY.byteLength) },
    });
  }

  const slice = RANGE_BODY.slice(parsed.start, parsed.end + 1);
  return respond(request, slice, {
    status: 206,
    headers: {
      ...headers,
      "content-range": `bytes ${parsed.start}-${parsed.end}/${RANGE_BODY.byteLength}`,
      "content-length": String(slice.byteLength),
    },
  });
}

function noStore(request: Request): Response {
  return textResponse(request, caseBody("no-store"), {
    // The relay directive alone: a standard no-store here would hide whether
    // the relay honours `X-MDB-Relay`.
    "x-mdb-relay": "no-store",
  });
}

function digest(request: Request): Response {
  const etag = '"digest-v1"';
  const headers = {
    etag,
    "cache-control": "public, max-age=3600",
    // Different digest spellings so every relay code path sees one it knows.
    digest: `sha-256=${DIGEST_BASE64}`,
    "content-digest": `sha-256=:${DIGEST_BASE64}:`,
    "docker-content-digest": `sha256:${DIGEST_HEX}`,
  };
  if (ifNoneMatch(request, etag)) return notModified(request, etag, headers);
  return textResponse(request, caseBody("digest"), headers);
}

const HANDLERS: Record<RelayTestCase, (request: Request) => Response | Promise<Response>> = {
  fresh,
  revalidate,
  ttl,
  vary,
  encoding,
  range,
  "no-store": noStore,
  digest,
};

const isCase = (name: string): name is RelayTestCase =>
  (CASES as readonly string[]).includes(name);

/** Handle `/relaytest/<case>`; unknown names are a real 404. */
export async function handleRelaytest(
  request: Request,
  name: string | undefined,
): Promise<Response> {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("Method Not Allowed\n", {
      status: 405,
      headers: { ...TEXT, allow: "GET, HEAD" },
    });
  }

  if (!name || !isCase(name)) {
    return new Response(`Unknown relaytest case: ${name ?? ""}\n`, {
      status: 404,
      headers: { ...TEXT, "cache-control": "no-store" },
    });
  }

  return HANDLERS[name as RelayTestCase](request);
}
