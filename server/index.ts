import { env } from "cloudflare:workers";

import {
  mirrorsBySubdomain,
  mirrorsByPath,
} from "./mirrors";
import { relayMiddleware } from "./relay";
import { api } from "./api";
import { handleSpeedtest } from "./speedtest";
import type { MirrorContext } from "./types";

/** Header the relay client sends to declare its public hostname. */
const RELAY_HOST_HEADER = "x-mdb-relay-host";

const notfound = () => new Response(null, { status: 404 });

export default {
  async fetch(incoming) {
    const request = relayMiddleware(incoming);

    const host = request.headers.get("host");
    if (!host) return notfound();
    if (!host.endsWith(env.BASE_DOMAIN)) return notfound();
    const subdomain = host.slice(0, -(env.BASE_DOMAIN.length + 1)) || "@";

    const relayHost = request.headers.get(RELAY_HOST_HEADER);
    const mctx: MirrorContext = relayHost ? { relay: { host: relayHost } } : {};

    // Subdomain-routed mirrors (OCI registries) win first.
    const mirror = mirrorsBySubdomain.get(subdomain);
    if (mirror) return mirror.fetch(request, mctx);

    const url = new URL(request.url);

    // Path-routed mirrors + API on the bare domain.
    if (subdomain === "@") {
      const [, firstSeg, ...rest] = url.pathname.split("/");

      if (firstSeg === "speedtest") return handleSpeedtest(request, rest[0]);

      const pathMirror = firstSeg && mirrorsByPath.get(firstSeg);
      if (pathMirror) return pathMirror.fetch(request, mctx);

      if (firstSeg === "api") return api.fetch(request);
    }

    // Nothing matched — serve the static site (SPA fallback).
    return env.ASSETS.fetch(request);
  },
} satisfies ExportedHandler<Env>;
