/**
 * API routes — served under `/api/`.
 */

import { Hono } from "hono";
import { mirrors, mirrorsByName } from "@server/mirrors";

export const api = new Hono().basePath("/api");

/** Mirror manifest — lists all available mirrors for relay discovery. */
api.get("/mirrors", (c) => {
  const entries = mirrors.map((m) => ({
    name: m.name,
    ...(m.host && { host: m.host }),
    ...(m.path && { path: m.path }),
    ...(m.keepHTTP && { keepHTTP: true }),
  }));
  return c.json({ mirrors: entries });
});

/** Per-mirror status snapshot. */
api.get("/status/:name", async (c) => {
  const mirror = mirrorsByName.get(c.req.param("name"));
  if (!mirror?.status) {
    return c.json({ error: "status unavailable", name: c.req.param("name") }, 404);
  }
  return c.json(await mirror.status());
});
