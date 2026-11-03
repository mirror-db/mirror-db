import { env } from "cloudflare:workers";

import { mirrorsBySubdomain, mirrorsByPath, mirrors } from "./mirrors";
import { api } from "./api";
import { keys } from "./keys";
import { scripts } from "./scripts";
import { handleSpeedtest } from "./speedtest";
import { notFoundResponse } from "./not-found";

/** Client-side routes the SPA owns (see `src/router/`). */
const SPA_ROUTES = new Set(["/", "/about"]);

/** Mount names advertised on the 404 page, in a stable order. */
const MOUNTS = mirrors
  .filter((m) => m.path)
  .map((m) => m.path as string)
  .sort();

export default {
  async fetch(request) {
    // Route on the host, without any port: a relay or a local dev server on a
    // non-default port still has to resolve to the same mirror.
    const rawHost = request.headers.get("host") || new URL(request.url).host;
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
    if (mirror) return mirror.fetch(request);

    const url = new URL(request.url);

    // A subdomain that is not a mirror is not this site either: the web UI and
    // the path mounts live on the bare domain only. Serving the SPA shell here
    // would answer `200 text/html` for a typo in a registry hostname.
    if (subdomain !== "@") return notFound(request);

    // Path-routed mounts on the bare domain.
    const [, firstSeg, ...rest] = url.pathname.split("/");

    if (firstSeg === "speedtest") return handleSpeedtest(request, rest[0]);

    const pathMirror = firstSeg && mirrorsByPath.get(firstSeg);
    if (pathMirror) return pathMirror.fetch(request);

    if (firstSeg === "") return serveStatic(request);
    if (firstSeg === "keys") return keys.fetch(request);
    if (firstSeg === "api") return api.fetch(request);
    if (firstSeg === "scripts") return scripts.fetch(request);

    // Static assets (favicon, /weblist/, SPA chunks) and the SPA's own routes.
    // Anything else is a genuine miss: falling through to the SPA answered
    // `200 text/html` for a misspelled mount, which reads as success to every
    // script that checks the status code.
    if (SPA_ROUTES.has(url.pathname)) return serveStatic(request);
    return serveStaticOr404(request, url.pathname);
  },
} satisfies ExportedHandler<Env>;

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
