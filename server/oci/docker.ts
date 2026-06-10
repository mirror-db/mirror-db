import { Hono } from "hono";

const app = new Hono();

app.get("/api/", (c) => {
  return c.json({ name: "Hello Docker!" });
});

export default app;
