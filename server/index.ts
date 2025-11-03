import { Hono } from "hono";
import { cache } from "hono/cache";
import type { Context } from "hono";
import { EndpointTag, EndpointTagList } from "../data/endpoints-tags";
import { Endpoint, EndpointsList } from "../data/endpoints";
import { ServerTag, ServerTagList } from "../data/servers-tags";
import { Server, ServerList } from "../data/servers";

import { StatServer } from "./server-analytics/api";

const app = new Hono();

const scache = (minutes: number = 42) =>
  cache({
    cacheName: "default",
    cacheControl: `public, max-age=${minutes * 60}`,
  });

app.get("/api/", (c) => {
  return c.json({ name: "Cloudflare" });
});

app.get("/api/endpoint-tags", async (c) => c.json(EndpointTagList));
app.get("/api/endpoint-tags/:tag", async (c) => {
  const epTag = EndpointTagList.find((t) => t.tag === c.req.param("tag"));
  return epTag
    ? c.json(epTag)
    : c.json({ error: "Endpoint tag not found" }, 404);
});

app.get("/api/endpoints", async (c) => c.json(EndpointsList));
const epByIdName = (c: Context): Endpoint | undefined =>
  EndpointsList.find((e) => e.id === c.req.param("id_name")) ||
  EndpointsList.find((e) => e.name === c.req.param("id_name"));
app.get("/api/endpoints/:id_name", async (c) => {
  const ep = epByIdName(c);
  return ep ? c.json(ep) : c.json({ error: "Endpoint not found" }, 404);
});

app.get("/api/servers", async (c) => c.json(ServerList));
app.get("/api/servers/:id", async (c) => {
  const server = ServerList.find((s) => s.id === c.req.param("id"));
  return server ? c.json(server) : c.json({ error: "Server not found" }, 404);
});

app.get("/api/endpoints/:id_name/stats", async (c) => {
  const ep = epByIdName(c);
  if (!ep) return c.json({ error: "Endpoint not found" }, 404);

  const servers = ServerList.filter((s) => s.endpoint_name === ep.name);
  const server =
    servers?.find((s) => s.tags.includes(ServerTag.Origin)) || servers?.[0];

  if (!server) return c.json({ error: "Server not found" }, 404);

  return c.redirect(`/api/servers/${server.id}/stats`);
});

app.get("/api/servers/:id/stats", scache(), async (c) => {
  const server = ServerList.find((s) => s.id === c.req.param("id"));
  if (!server) return c.json({ error: "Server not found" }, 404);
  const result = await StatServer(server, c.req.query("detailed") === "true");
  return result ? c.json(result) : c.json({ error: result }, 400);
});

export default app;
