/**
 * Helm chart repository mirror — mounts every repo in `repos.ts` under the
 * bare-domain `/charts/` path. This is the HTTP edge: it routes, resolves the
 * request's mirror base, and wraps `HelmRepoKeeper` data in `Response`s.
 *
 * Routes (under `/charts/`):
 * - `<repo>`                 → repo metadata JSON
 * - `<repo>/index.yaml`      → rewritten Helm index (urls + dep repos → mirror urls)
 * - `<repo>/generated`       → upstream `generated` timestamp (cache marker)
 * - `<repo>/res/<uuid>/info` → original upstream url for that resource
 * - `<repo>/res/<uuid>/*`    → tarball, streamed + cached from upstream
 *
 * `<repo>` is a seed from `repos.ts` or a slug auto-mounted from a mirrored
 * chart's dependency `repository` (see `HelmRegistry`).
 *
 * Usage: `helm repo add <repo> https://mirs.uk/charts/<repo>`
 */

import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";
import type { Mirror, MirrorContext } from "@server/types";

import { registry } from "./registry";

const PATH = "charts";
const PREFIX = `/${PATH}/`;

const notFound = () => new Response("Not Found", { status: 404 });

export const charts: Mirror = {
  name: "charts",
  path: PATH,
  async fetch(request, ctx?: MirrorContext) {
    const url = new URL(request.url);
    if (!url.pathname.startsWith(PREFIX)) return notFound();

    // Origin the client actually reached us on: the relay's public host when
    // proxied, else this request's own origin. Rewritten urls point here so
    // they route back through the same host (never straight to the upstream).
    const mirrorBase = ctx?.relay?.host ? `https://${ctx.relay.host}` : url.origin;

    const rel = url.pathname.slice(PREFIX.length);
    const [repoName, ...rest] = rel.split("/");

    const keeper = repoName ? await registry.ensure(repoName) : undefined;
    if (!keeper) return notFound();

    // `<repo>` (or `<repo>/`) → repo metadata + chart list.
    if (rest.length === 0 || (rest.length === 1 && rest[0] === "")) {
      await keeper.waitInitialized();
      return Response.json({
        ...keeper.repoInfo,
        charts: keeper.chartSummaries(),
      });
    }

    const sub = rest.join("/");

    try {
      if (sub === "index.yaml") {
        await registry.refreshAndMount(keeper);
        // Browsers see text/plain so they render as text instead of
        // triggering a download (YAML has no registered MIME type, so
        // application/yaml makes some UAs offer a "save as" dialog).
        const isBrowser = (request.headers.get("accept") ?? "").includes("text/html");
        return new Response(keeper.render(mirrorBase), {
          headers: { "content-type": isBrowser ? "text/plain; charset=utf-8" : "application/yaml" },
        });
      }

      if (sub === "generated") {
        await keeper.waitInitialized();
        return new Response(keeper.generated ?? "");
      }

      // res/<uuid>/info → original upstream url
      const infoMatch = sub.match(/^res\/([^/]+)\/info$/);
      if (infoMatch) {
        await keeper.waitInitialized();
        return new Response(keeper.resolveRes(infoMatch[1]) ?? "not found");
      }

      // res/<uuid>/* → tarball, streamed + cached from upstream
      const resMatch = sub.match(/^res\/([^/]+)\//);
      if (resMatch) {
        await keeper.waitInitialized();
        const target = keeper.resolveRes(resMatch[1]);
        if (!target) return notFound();
        return sanitizeResponse(await cachedfetch(target));
      }
    } catch {
      // Upstream index.yaml unreachable or unparseable.
      return new Response("Bad Gateway", { status: 502 });
    }

    return notFound();
  },
};
