import { Hono } from "hono";
import type { Bindings } from "./env.d";

const app = new Hono<{ Bindings: Bindings }>();

app.get("/api/", (c) => {
  return c.json({ name: "Cloudflare" });
});

export default app;
