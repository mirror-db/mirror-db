/**
 * Relay middleware — rewrites `/@relay/...` into a normal request.
 *
 * Routing convention:
 * - `/@relay/@<subdomain>/<path>` → rewrite host to `<subdomain>.<BASE_DOMAIN>`
 * - `/@relay/<path>`              → strip prefix, keep same host
 *
 * After rewrite the request looks identical to a direct request, so downstream
 * routing (subdomain check → path match) works unchanged.
 *
 * Returns the request unchanged if not a relay request.
 */

import { env } from "cloudflare:workers";

const RELAY_KEYWORD = "@relay";

export function relayMiddleware(request: Request): Request {
  const url = new URL(request.url);
  const [, keyword, target, ...rest] = url.pathname.split("/");

  if (keyword !== RELAY_KEYWORD) return request;
  if (!target) return request;

  const newUrl = target.startsWith("@")
    ? new URL("/" + rest.join("/") + url.search, `${url.protocol}//${target.slice(1)}.${env.BASE_DOMAIN}`)
    : new URL("/" + [target, ...rest].join("/") + url.search, url.origin);

  const headers = new Headers(request.headers);
  headers.set("host", newUrl.host);

  return new Request(newUrl.toString(), {
    method: request.method,
    headers,
    body: request.body,
    redirect: "manual",
  });
}
