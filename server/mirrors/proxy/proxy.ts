import urlJoin from "url-join";
import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";
import { MdbUserAgent } from "@server/pkgs/fetch/const";
import { GlobalPassthroughHeaders } from "./const";

export interface Upstream {
  /** Upstream base URL, e.g. "https://registry.npmjs.org" */
  url: string;
  /** Path prefix to strip from incoming request before forwarding */
  prefix?: string;
  /** Extra headers to pass through beyond GlobalPassthroughHeaders */
  passthroughHeaders?: string[];
}

export type PreProxyHook = (req: Request) => Request | Promise<Request>;
export type PostProxyHook = (req: Request, resp: Response) => Response | Promise<Response>;

// --- Pre hooks ---

export const rewritePath = (fn: (pathname: string) => string): PreProxyHook => (req) => {
  const url = new URL(req.url);
  url.pathname = fn(url.pathname);
  return new Request(url.toString(), req);
};

export const stripPrefix = (prefix: string): PreProxyHook =>
  rewritePath((p) => p.replace(prefix, ""));

export const setBaseUrl = (baseUrl: string | URL): PreProxyHook => (req) => {
  const base = typeof baseUrl === "string" ? baseUrl : baseUrl.toString();
  const { pathname, search } = new URL(req.url);
  return new Request(urlJoin(base, pathname) + search, req);
};

export const stripSearch: PreProxyHook = (req) => {
  const url = new URL(req.url);
  url.search = "";
  return new Request(url.toString(), req);
};

export const filterHeaders = (whitelist: string[]): PreProxyHook => (req) => {
  const lower = whitelist.map((h) => h.toLowerCase());
  const headers = new Headers();
  for (const h of lower) {
    const value = req.headers.get(h);
    if (value) headers.set(h, value);
  }
  return new Request(req.url, { ...req, headers });
};

export const standardizeUserAgent: PreProxyHook = (req) => {
  const headers = new Headers(req.headers);
  if (!headers.has("user-agent")) headers.set("user-agent", MdbUserAgent);
  return new Request(req, { headers });
};

export const followRedirects: PreProxyHook = (req) => {
  return new Request(req, { redirect: "follow" });
};

// --- Post hooks ---

/** Strip dangerous upstream headers (auth challenges, cookies, hop-by-hop). */
export const sanitize: PostProxyHook = (_req, resp) => sanitizeResponse(resp);

const TEXT_CONTENT_TYPES = [
  "application/json",
  "text/",
  "application/xml",
  "application/xhtml+xml",
  "application/javascript",
  "application/x-yaml",
  "application/toml",
];

function isTextContent(contentType: string): boolean {
  const ct = contentType.toLowerCase();
  return TEXT_CONTENT_TYPES.some((t) => ct.includes(t));
}

/**
 * Stream-based body rewriting for text responses.
 * Buffers internally (necessary for arbitrary transforms), but returns
 * a streaming Response immediately so the runtime handles backpressure.
 * Non-text responses pass through untouched.
 */
export const rewriteBody = (
  fn: (body: string, req: Request, resp: Response) => string,
): PostProxyHook => (req, resp) => {
  const ct = resp.headers.get("content-type") || "";
  if (!isTextContent(ct)) return resp;
  if (!resp.body) return resp;

  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  let buffer = "";

  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>({
    transform(chunk, _controller) {
      buffer += decoder.decode(chunk, { stream: true });
    },
    flush(controller) {
      buffer += decoder.decode(); // flush remaining
      controller.enqueue(encoder.encode(fn(buffer, req, resp)));
    },
  });

  resp.body.pipeTo(writable);

  const headers = new Headers(resp.headers);
  headers.delete("content-length"); // length may change after rewrite

  return new Response(readable, {
    status: resp.status,
    statusText: resp.statusText,
    headers,
  });
};

// --- ProxyRequest builder ---

export class ProxyRequest {
  prehooks: PreProxyHook[] = [];
  posthooks: PostProxyHook[] = [];

  clone() {
    const p = new ProxyRequest();
    p.prehooks = [...this.prehooks];
    p.posthooks = [...this.posthooks];
    return p;
  }

  pre(fn: PreProxyHook) {
    this.prehooks.push(fn);
    return this;
  }

  post(fn: PostProxyHook) {
    this.posthooks.push(fn);
    return this;
  }

  async apply(req: Request): Promise<Response> {
    const original = req;
    for (const hook of this.prehooks) {
      req = await hook(req);
    }

    let resp = await cachedfetch(req);

    for (const hook of this.posthooks) {
      resp = await hook(original, resp);
    }

    return resp;
  }
}

export const upstreamProxy = (upstream: Upstream) => {
  const p = new ProxyRequest();
  if (upstream.prefix) p.pre(stripPrefix(upstream.prefix));
  p.pre(setBaseUrl(upstream.url));
  p.pre(filterHeaders([...GlobalPassthroughHeaders, ...(upstream.passthroughHeaders ?? [])]));
  p.pre(standardizeUserAgent);
  p.pre(followRedirects);
  p.post(sanitize);
  return p;
};
