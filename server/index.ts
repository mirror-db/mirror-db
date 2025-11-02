import { Hono } from "hono";
import type { Bindings } from "./env.d";
import { EndpointTags } from "../data/endpoints-tags";
import { Endpoints } from "../data/endpoints";

const app = new Hono<{ Bindings: Bindings }>();

app.get("/api/", (c) => {
  return c.json({ name: "Cloudflare" });
});

app.get("/api/endpoint-tags", async (c) => c.json(EndpointTags));
app.get("/api/endpoints", async (c) => c.json(Endpoints));

export default app;
