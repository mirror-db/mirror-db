import {
  mirrorsBySubdomain,
  mirrorsByName,
  matchMirrorPath,
} from "./mirrors";

const notfound = () => new Response(null, { status: 404 });

export default {
  async fetch(request, env) {
    const host = request.headers.get("host");
    if (!host) return notfound();
    if (!host.endsWith(env.BASE_DOMAIN)) return notfound();
    const subdomain = host.slice(0, -(env.BASE_DOMAIN.length + 1)) || "@";

    // Subdomain-routed mirrors (OCI registries) win first.
    const mirror = mirrorsBySubdomain.get(subdomain);
    if (mirror) return mirror.fetch(request);

    const url = new URL(request.url);

    // Control plane + path-routed mirrors live on the bare domain.
    if (subdomain === "@") {
      const name = url.pathname.match(/^\/status\/([^/]+)\/?$/)?.[1];
      if (name) return mirrorStatus(name);

      const pathMirror = matchMirrorPath(url.pathname);
      if (pathMirror) return pathMirror.fetch(request);

      // Handle path-mirror mount points without trailing slash (e.g. `/debian`).
      // Windows WebClient sends OPTIONS/PROPFIND here during DAV discovery.
      const withSlash = url.pathname + "/";
      const slashMirror = matchMirrorPath(withSlash);
      if (slashMirror) {
        // Redirect GETs so browsers get the canonical URL; forward other methods
        // (OPTIONS/PROPFIND) directly so WebDAV discovery works.
        if (request.method === "GET") {
          return Response.redirect(new URL(withSlash, url).toString(), 301);
        }
        const inner = new Request(new URL(withSlash + url.search, url.origin), request);
        return slashMirror.fetch(inner);
      }
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

/** `GET /status/<name>` → that mirror's status snapshot, or 404. */
async function mirrorStatus(name: string): Promise<Response> {
  const mirror = mirrorsByName.get(name);
  if (!mirror?.status) {
    return Response.json({ error: "status unavailable", name }, { status: 404 });
  }
  return Response.json(await mirror.status());
}
