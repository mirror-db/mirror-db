import { Hono } from "hono";

export const scripts = new Hono().basePath("/scripts");

import { regOciRoute as addOciScripts } from "./oci";

addOciScripts(scripts);
