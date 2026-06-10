import { env } from "cloudflare:workers";
import { Hono } from "hono";

// env.DOCKER_HUB_USER;
// env.DOCKER_HUB_PASSWORD;

const app = new Hono();

app.get("/api/", (c) => {
  return c.json({ name: "Hello Docker!" });
});

export default app;
