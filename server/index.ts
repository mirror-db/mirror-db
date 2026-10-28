import { env } from "cloudflare:workers";

import { mirrorsBySubdomain, mirrorsByPath } from "./mirrors";
import { api } from "./api";
import { keys } from "./keys";
import { scripts } from "./scripts";
import { handleSpeedtest } from "./speedtest";

const notfound = () => new Response(null, { status: 404 });

export default {
  async fetch(request) {
    // get subdomain from host header
    const host = request.headers.get("host") || new URL(request.url).host;
    if (!host) return notfound();
    if (!host.endsWith(env.BASE_DOMAIN)) return notfound();
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

      if (firstSeg === "keys") return keys.fetch(request);
      if (firstSeg === "api") return api.fetch(request);
      if (firstSeg === "scripts") return scripts.fetch(request);
    }

    // Nothing matched — serve the static site (SPA fallback).
    const resp = await env.ASSETS.fetch(request);
    if (resp.status !== 404) return resp;
    return env.ASSETS.fetch("http://mirs.uk/index.html");
  },
} satisfies ExportedHandler<Env>;
