/**
 * `/conf/apt/` — render APT source files from explicit presets.
 *
 * Route surface:
 * - `GET /conf/apt/`               → registered paths (JSON, or linked HTML)
 * - `GET /conf/apt/<name>.sources` → the rendered deb822 file
 *
 * Only GET/HEAD; anything else on a known path is 405. An unknown path is a
 * real 404, never a directory guess.
 */

import { Hono } from "hono";

import { relayInfo } from "@server/relay";

import { aptPresetRegistry } from "./presets";
import { APT_SOURCE_SUFFIX, aptSourcePath } from "./registry";
import { AptSourceError, renderAptSource } from "./sources";
import { AptQueryError } from "./params";
import type { AptPresetEntry } from "./types";

/** Browser user agents get the linked listing; everything else gets JSON. */
export function isBrowserUserAgent(userAgent: string): boolean {
  return /Mozilla|Chrome|Safari|Firefox|Edg/i.test(userAgent);
}

/** `debian-12.sources` line with a link, one per registered path. */
export function renderAptIndexHtml(paths: string[]): string {
  const lines = paths.map((path) => {
    const name = path.slice(path.lastIndexOf("/") + 1);
    return `<a href="${path}">${name}</a>`;
  });
  return [
    "<!doctype html>",
    '<html lang="en">',
    "<head>",
    '<meta charset="utf-8">',
    "<title>APT sources</title>",
    "</head>",
    "<body>",
    "<pre>",
    ...lines,
    "</pre>",
    "</body>",
    "</html>",
    "",
  ].join("\n");
}

/** JSON listing: name plus the path to fetch it from. */
export function aptIndexJson(entries: AptPresetEntry[]) {
  return {
    sources: entries.map((entry) => ({
      name: entry.name,
      path: aptSourcePath(entry.name),
    })),
  };
}

export const apt = new Hono({ strict: false });

apt.use("*", async (c, next) => {
  const method = c.req.method.toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    return c.json({ error: "method not allowed" }, 405);
  }
  await next();
});

/** Registered paths, stable-sorted. Aliases are separate entries. */
apt.get("/", (c) => {
  const entries = aptPresetRegistry.entries();
  const userAgent = c.req.header("user-agent") ?? "";
  if (isBrowserUserAgent(userAgent)) {
    const paths = entries.map((entry) => aptSourcePath(entry.name));
    return c.html(renderAptIndexHtml(paths));
  }
  return c.json(aptIndexJson(entries));
});

/** Render one preset; query parameters override its defaults. */
apt.get("/:file", async (c) => {
  const file = c.req.param("file");
  const name = file.endsWith(APT_SOURCE_SUFFIX)
    ? file.slice(0, -APT_SOURCE_SUFFIX.length)
    : undefined;
  const entry = name ? aptPresetRegistry.get(name) : undefined;
  if (!entry) {
    return c.json({ error: "unknown apt source preset", name: file }, 404);
  }

  const { origin } = relayInfo(c.req.raw);
  try {
    const body = await renderAptSource(entry, {
      origin,
      query: new URL(c.req.url).searchParams,
    });
    return c.body(body, 200, {
      // The exact media type `.sources` consumers expect; no charset guessing.
      "content-type": "text/plain; charset=utf-8",
    });
  } catch (err) {
    // Bad input is the client's mistake; everything else (an unreachable key
    // publisher) must not be papered over with an unsigned source.
    if (err instanceof AptQueryError || err instanceof AptSourceError) {
      return c.json({ error: err.message }, 400);
    }
    return c.json(
      {
        error: "signing key unavailable",
        detail: err instanceof Error ? err.message : String(err),
      },
      502,
    );
  }
});

apt.notFound((c) => c.json({ error: "not found" }, 404));
