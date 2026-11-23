import { beforeEach, describe, expect, it, vi } from "vitest";

import { env } from "@server/test-stubs/cloudflare-workers";

/**
 * Routing at the front door.
 *
 * The behaviour under test is what an *outer* client sees for a path that is
 * not a mount: a real 404 (browser: an HTML page with a redirect home; anything
 * else: plain text). Falling through to the SPA used to answer `200 text/html`,
 * which every script reads as success.
 */

const assets = {
  fetch: vi.fn(),
};

async function loadWorker() {
  env.BASE_DOMAIN = "mirs.uk";
  // `ASSETS` is a real binding in production; here it is a stub that knows only
  // the SPA shell and one static file.
  (env as any).ASSETS = assets;
  const mod = await import("./index");
  return mod.default;
}

const req = (path: string, headers: Record<string, string> = {}) =>
  new Request(`https://mirs.uk${path}`, { headers }) as unknown as
    Parameters<Awaited<ReturnType<typeof loadWorker>>["fetch"]>[0];

beforeEach(() => {
  assets.fetch.mockReset();
  assets.fetch.mockImplementation(async (input: any) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/index.html")) {
      return new Response("<!doctype html><title>app</title>", {
        headers: { "content-type": "text/html" },
      });
    }
    if (url.endsWith("/favicon.ico")) {
      return new Response("ico", { headers: { "content-type": "image/x-icon" } });
    }
    return new Response(null, { status: 404 });
  });
});

describe("front door — host routing", () => {
  it("routes a subdomain mirror even when the host carries a port", async () => {
    const worker = await loadWorker();
    // Upstream is stubbed: this test is about routing, not about Docker Hub.
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation(async () => new Response("{}", { status: 200 }));
    try {
      // A relay or a dev server on a non-default port must not lose the mirror.
      const res = await worker.fetch(
        new Request("http://dcr.mirs.uk:8788/v2/", {
          headers: { host: "dcr.mirs.uk:8788" },
        }) as any,
      );
      const body = await res.text();
      expect(body).not.toContain("is not a mirror on this service");
      expect(fetchSpy).toHaveBeenCalled();
    } finally {
      fetchSpy.mockRestore();
    }
  });

  it("does not serve the site on a subdomain that is not a mirror", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(
      new Request("https://nope.mirs.uk/", { headers: { host: "nope.mirs.uk" } }) as any,
    );
    expect(res.status).toBe(404);
    expect(assets.fetch).not.toHaveBeenCalled();
  });

  it("rejects a host outside BASE_DOMAIN", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(
      new Request("http://evil.example/v2/", { headers: { host: "evil.example" } }) as any,
    );
    expect(res.status).toBe(404);
    expect(await res.text()).toContain("is not a mirror on this service");
  });
});

describe("front door — unknown paths", () => {
  it("answers a plain-text 404 for an unknown mount (non-browser)", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(req("/cargo/index.json", { "user-agent": "cargo/1.80" }));

    expect(res.status).toBe(404);
    expect(res.headers.get("content-type")).toContain("text/plain");
    expect(res.headers.get("cache-control")).toBe("no-store");
    const body = await res.text();
    expect(body).toContain("404");
    expect(body).toContain("/cargo");
    // The page lists what does exist, so a typo is self-correcting.
    expect(body).toContain("/npm/");
  });

  it("answers an HTML page with a redirect home for a browser", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(
      req("/goproxy/", { accept: "text/html", "user-agent": "Mozilla/5.0" }),
    );

    expect(res.status).toBe(404);
    expect(res.headers.get("content-type")).toContain("text/html");
    const html = await res.text();
    expect(html).toContain("meta http-equiv=\"refresh\"");
    expect(html).toContain('location.replace("/")');
    expect(html).toContain("/api/mirrors");
  });

  it("serves the SPA shell only on routes the client app owns", async () => {
    const worker = await loadWorker();
    for (const path of ["/", "/about"]) {
      const res = await worker.fetch(req(path, { accept: "text/html" }));
      expect(res.status).toBe(200);
      expect(await res.text()).toContain("<title>app</title>");
    }
  });

  it("still serves real static assets", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(req("/favicon.ico"));
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("image/x-icon");
  });

  it("routes /conf/ to the configuration app, not the SPA", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(req("/conf/apt/debian-12.sources"));
    expect(assets.fetch).not.toHaveBeenCalled();
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(await res.text()).toContain("URIs: https://mirs.uk/debian/");

    // A typo below /conf/ is still a real 404, never the SPA shell.
    const miss = await worker.fetch(req("/conf/aptx"));
    expect(miss.status).toBe(404);
    expect(assets.fetch).not.toHaveBeenCalled();
  });

  it("routes /relaytest/ to the deterministic test upstream, not the SPA", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(req("/relaytest/fresh"));

    expect(res.status).toBe(200);
    expect(res.headers.get("etag")).toBe('"fresh-v1"');
    expect(await res.text()).toBe("relaytest: fresh v1\n");
    expect(assets.fetch).not.toHaveBeenCalled();
  });

  it("does not let the SPA swallow a known-but-missing mirror path", async () => {
    const worker = await loadWorker();
    // `debian` is a mount, so the request must reach the mirror, never ASSETS.
    const res = await worker.fetch(req("/debian/does-not-exist"));
    expect(assets.fetch).not.toHaveBeenCalled();
    expect(res.status).not.toBe(200);
  });
});
