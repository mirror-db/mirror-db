import { hosts } from "./hosts";
import { matchPath } from "./paths";

const notfound = () => new Response(null, { status: 404 });

export default {
  fetch(request, env, ctx) {
    const host = request.headers.get("host");
    if (!host) return notfound();
    if (!host.endsWith(env.BASE_DOMAIN)) return notfound();
    const subdomain = host.slice(0, -(env.BASE_DOMAIN.length + 1)) || "@";

    const handler = hosts.get(subdomain);
    if (handler) return handler.fetch(request, ctx);

    const url = new URL(request.url);

    // The bare domain routes mirrors (APT repos) by path prefix.
    if (subdomain === "@") {
      const pathHost = matchPath(url.pathname);
      if (pathHost) return pathHost.fetch(request, ctx);
    }

    if (url.pathname.startsWith("/api/")) {
      return Response.json({
        name: subdomain,
      });
    }

    // Nothing matched a mirror — serve the static site (SPA fallback).
    return env.ASSETS.fetch(request);
  },
} satisfies ExportedHandler<Env>;
