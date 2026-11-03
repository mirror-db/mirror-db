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
    // get subdomain from host header
    const host = request.headers.get("host") || new URL(request.url).host;
    if (!host) return notFound(request);
    if (!host.endsWith(env.BASE_DOMAIN)) return notFound(request);
    const subdomain = host.slice(0, -(env.BASE_DOMAIN.length + 1)) || "@";

    // Subdomain-routed mirrors (OCI registries) win first.
    const mirror = mirrorsBySubdomain.get(subdomain);
    if (mirror) return mirror.fetch(request);

    const url = new URL(request.url);

    // Path-routed mirrors + API on the bare domain.
    if (subdomain === "@") {
      const [, firstSeg, ...rest] = url.pathname.split("/");

      if (firstSeg === "speedtest") return handleSpeedtest(request, rest[0]);

      const pathMirror = firstSeg && mirrorsByPath.get(firstSeg);
      if (pathMirror) return pathMirror.fetch(request);

      if (firstSeg === "") return serveStatic(request);
      if (firstSeg === "keys") return keys.fetch(request);
      if (firstSeg === "api") return api.fetch(request);
      if (firstSeg === "scripts") return scripts.fetch(request);
    }

    // Static assets (favicon, /weblist/, SPA chunks) and the SPA's own routes.
    // Anything else is a genuine miss: falling through to the SPA answered
    // `200 text/html` for a misspelled mount, which reads as success to every
    // script that checks the status code.
    if (subdomain === "@" && SPA_ROUTES.has(url.pathname)) return serveStatic(request);
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
