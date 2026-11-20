/**
 * API routes — served under `/api/`.
 */

import { Hono } from "hono";
import { mirrorsByName } from "@server/mirrors";

import { mirrorManifestResponse } from "./mirror-manifest";

export const api = new Hono().basePath("/api");

/**
 * Mirror manifest — all mirrors plus ordered relay policies. GET and HEAD
 * share one deterministic ETag and honour `If-None-Match` with a 304.
 */
api.on(["GET", "HEAD"], "/mirrors", (c) => mirrorManifestResponse(c.req.raw));

/** Per-mirror status snapshot. */
api.get("/status/:name", async (c) => {
  const mirror = mirrorsByName.get(c.req.param("name"));
  if (!mirror?.status) {
    return c.json({ error: "status unavailable", name: c.req.param("name") }, 404);
  }
  return c.json(await mirror.status());
});
