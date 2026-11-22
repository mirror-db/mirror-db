/**
 * `/conf/` — generated client configuration.
 *
 * Only `/conf/apt/` exists today. Anything else under `/conf/` is a real 404,
 * so a mistyped configuration URL fails instead of serving the SPA.
 */

import { Hono } from "hono";

import { apt } from "./apt";

export const conf = new Hono({ strict: false }).basePath("/conf");

conf.route("/apt", apt);

conf.notFound((c) => c.json({ error: "not found" }, 404));
