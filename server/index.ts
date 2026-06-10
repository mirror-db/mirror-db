import { hosts } from "./hosts";

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
    if (url.pathname.startsWith("/api/")) {
      return Response.json({
        name: subdomain,
      });
    }
    return notfound();
  },
} satisfies ExportedHandler<Env>;
