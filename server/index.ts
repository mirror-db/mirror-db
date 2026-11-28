import { env } from "cloudflare:workers";

import { mirrorsBySubdomain, mirrorsByPath, mirrors } from "./mirrors";
import { api } from "./api";
import { conf } from "./conf";
import { keys } from "./keys";
import { scripts } from "./scripts";
import { handleSpeedtest } from "./speedtest";
import { handleRelaytest } from "./relaytest";
import { normalizeRelayRequest, withRelayDirectives } from "./relay-control";
import { notFoundResponse } from "./not-found";
import type { Mirror, MountContext } from "./types";

/** Client-side routes the SPA owns (see `src/router/`). */
const SPA_ROUTES = new Set(["/", "/about"]);

/** Mount names advertised on the 404 page, in a stable order. */
const MOUNTS = mirrors
  .filter((m) => m.path)
  .map((m) => m.path as string)
  .sort();

export default {
  async fetch(request) {
    // A direct client must not be able to inject relay response controls; the
    // valid relay-host marker is what makes the header trustworthy.
    request = normalizeRelayRequest(request);
    const url = new URL(request.url);

    // Route on the host, without any port: a relay or a local dev server on a
    // non-default port still has to resolve to the same mirror.
    const rawHost = request.headers.get("host") || url.host;
    const host = rawHost.replace(/:\d+$/, "").replace(/\.$/, "").toLowerCase();
    if (!host) return notFound(request);
    if (host !== env.BASE_DOMAIN && !host.endsWith(`.${env.BASE_DOMAIN}`)) {
      return notFound(request);
    }
    const subdomain = host === env.BASE_DOMAIN
      ? "@"
      : host.slice(0, -(env.BASE_DOMAIN.length + 1));

    // Subdomain-routed mirrors (OCI registries) win first.
    const mirror = mirrorsBySubdomain.get(subdomain);
    if (mirror) {
      return serveMirror(mirror, request, { relativePath: url.pathname.slice(1) });
    }

    // A subdomain that is not a mirror is not this site either: the web UI and
    // the path mounts live on the bare domain only. Serving the SPA shell here
    // would answer `200 text/html` for a typo in a registry hostname.
    if (subdomain !== "@") return notFound(request);

    // Path-routed mounts on the bare domain.
    const [, firstSeg, ...rest] = url.pathname.split("/");

    if (firstSeg === "speedtest") return handleSpeedtest(request, rest[0]);
    if (firstSeg === "relaytest") return handleRelaytest(request, rest[0]);

    const pathMirror = firstSeg && mirrorsByPath.get(firstSeg);
    if (pathMirror) {
      return serveMirror(pathMirror, request, { relativePath: rest.join("/") });
    }

    if (firstSeg === "") return serveStatic(request);
    if (firstSeg === "keys") return keys.fetch(request);
    if (firstSeg === "api") return api.fetch(request);
    if (firstSeg === "scripts") return scripts.fetch(request);
    if (firstSeg === "conf") return conf.fetch(request);

    // Static assets (favicon, /weblist/, SPA chunks) and the SPA's own routes.
    // Anything else is a genuine miss: falling through to the SPA answered
    // `200 text/html` for a misspelled mount, which reads as success to every
    // script that checks the status code.
    if (SPA_ROUTES.has(url.pathname)) return serveStatic(request);
    return serveStaticOr404(request, url.pathname);
  },
} satisfies ExportedHandler<Env>;

/**
 * Every mirror response passes through one shared relay-control middleware.
 * Only mirrors that declare directives are touched; the merge never overrides
 * a directive the mirror (or upstream) already set.
 *
 * Exported so the wiring itself can be tested without reaching an upstream.
 */
export async function serveMirror(
  mirror: Mirror,
  request: Request,
  context: MountContext,
): Promise<Response> {
  const response = await mirror.fetch(request, context);
  return withRelayDirectives(response, mirror.relayDirectives);
}

const notFound = (request: Request): Response =>
  notFoundResponse(request, { pathname: new URL(request.url).pathname, mounts: MOUNTS });

/** Serve from the static assets binding, falling back to the SPA shell. */
async function serveStatic(request: Request): Promise<Response> {
  const resp = await env.ASSETS.fetch(request);
  if (resp.status !== 404) return resp;
  return env.ASSETS.fetch("http://mirs.uk/index.html");
}

/** Serve a static asset if one exists, otherwise 404. */
async function serveStaticOr404(request: Request, pathname: string): Promise<Response> {
  const resp = await env.ASSETS.fetch(request);
  if (resp.status !== 404) return resp;
  return notFoundResponse(request, { pathname, mounts: MOUNTS });
}
