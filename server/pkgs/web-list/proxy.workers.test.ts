import { describe, expect, it } from "vitest";

import debianHtml from "./__fixtures__/cloudflaremirrors-debian.html?raw";
import ubuntuHtml from "./__fixtures__/nl-archive-ubuntu.html?raw";
import type { WebListEntry } from "./parse";
import { WebListProxy } from "./proxy";

/**
 * Stub fetchImpl: returns the fixture HTML for any directory fetch, and a
 * plain-text body for file fetches (simulates upstream file content).
 */
function stubFetch(html: string) {
  return async (input: string | URL | Request): Promise<Response> => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    // If the URL ends with `/` it's a directory listing request.
    if (url.endsWith("/")) {
      return new Response(html, {
        headers: { "content-type": "text/html" },
      });
    }
    // File request — return a fake body.
    return new Response(`file-content-of:${url}`, {
      headers: { "content-type": "application/octet-stream" },
    });
  };
}

function makeProxy(base: string, html: string) {
  return new WebListProxy({
    baseURL: base,
    fetchImpl: stubFetch(html),
    render(entries: WebListEntry[], path: string) {
      return new Response(
        JSON.stringify({ rendered: true, path, count: entries.length }),
        { headers: { "content-type": "text/html" } },
      );
    },
  });
}

function req(url: string, method = "GET", headers?: Record<string, string>) {
  return new Request(url, { method, headers });
}

// ── JSON API ────────────────────────────────────────────────────────────────

describe("WebListProxy — JSON API", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET /?format=json returns structured entries", async () => {
    const res = await proxy.fetch(req("http://localhost/?format=json"));
    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.path).toBe("");
    expect(body.entries.length).toBeGreaterThan(5);
    expect(body.entries.find((e: WebListEntry) => e.name === "dists")).toMatchObject({
      type: "directory",
      href: "dists/",
    });
  });

  it("GET /dists/?format=json works for subdirectories", async () => {
    const proxy2 = makeProxy("https://nl.archive.ubuntu.com/ubuntu/", ubuntuHtml);
    const res = await proxy2.fetch(req("http://localhost/dists/?format=json"));
    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.path).toBe("dists/");
  });
});

// ── Web listing (render) ────────────────────────────────────────────────────

describe("WebListProxy — Web listing", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET / calls the render callback", async () => {
    const res = await proxy.fetch(req("http://localhost/"));
    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.rendered).toBe(true);
    expect(body.path).toBe("");
    expect(body.count).toBeGreaterThan(5);
  });

  it("GET /pool/ renders subdirectory", async () => {
    const res = await proxy.fetch(req("http://localhost/pool/"));
    const body: any = await res.json();
    expect(body.rendered).toBe(true);
    expect(body.path).toBe("pool/");
  });
});

// ── File passthrough ────────────────────────────────────────────────────────

describe("WebListProxy — File passthrough", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET /README proxies the file from upstream", async () => {
    const res = await proxy.fetch(req("http://localhost/README"));
    expect(res.status).toBe(200);
    const body = await res.text();
    expect(body).toContain("file-content-of:");
    expect(body).toContain("/README");
  });
});

// ── WebDAV ──────────────────────────────────────────────────────────────────

describe("WebListProxy — WebDAV", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("OPTIONS returns DAV:1 and correct Allow", async () => {
    const res = await proxy.fetch(req("http://localhost/", "OPTIONS"));
    expect(res.status).toBe(204);
    expect(res.headers.get("dav")).toBe("1");
    expect(res.headers.get("allow")).toContain("PROPFIND");
    expect(res.headers.get("allow")).toContain("GET");
  });

  it("PROPFIND / Depth:1 returns 207 with entries", async () => {
    const res = await proxy.fetch(
      req("http://localhost/", "PROPFIND", { depth: "1" }),
    );
    expect(res.status).toBe(207);
    const xml = await res.text();
    expect(xml).toContain("<D:multistatus");
    expect(xml).toContain("<D:collection/>");
    // Should contain hrefs for known entries
    expect(xml).toContain("dists/");
    expect(xml).toContain("README");
  });

  it("PROPFIND / Depth:0 returns only the directory itself", async () => {
    const res = await proxy.fetch(
      req("http://localhost/", "PROPFIND", { depth: "0" }),
    );
    expect(res.status).toBe(207);
    const xml = await res.text();
    // Only one <D:response> (the directory itself)
    const responseCount = (xml.match(/<D:response>/g) || []).length;
    expect(responseCount).toBe(1);
  });

  it("PUT returns 405 Method Not Allowed", async () => {
    const res = await proxy.fetch(req("http://localhost/test", "PUT"));
    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toContain("GET");
  });

  it("DELETE returns 405", async () => {
    const res = await proxy.fetch(req("http://localhost/test", "DELETE"));
    expect(res.status).toBe(405);
  });
});

// ── fetchHook ───────────────────────────────────────────────────────────────

describe("WebListProxy — fetchHook", () => {
  it("intercepts a request when hook returns a Response", async () => {
    const proxy = new WebListProxy({
      baseURL: "http://ftp.us.debian.org/debian/",
      fetchImpl: stubFetch(debianHtml),
      render(entries, path) {
        return new Response("default render");
      },
      fetchHook(path, _request) {
        if (path === "special") {
          return new Response("hooked!", { status: 299 });
        }
        return null;
      },
    });

    const hooked = await proxy.fetch(req("http://localhost/special"));
    expect(hooked.status).toBe(299);
    expect(await hooked.text()).toBe("hooked!");

    // Non-matched paths still fall through normally.
    const normal = await proxy.fetch(req("http://localhost/README"));
    expect(normal.status).toBe(200);
    expect(await normal.text()).toContain("file-content-of:");
  });

  it("hook receives the relative path without leading slash", async () => {
    let captured = "";
    const proxy = new WebListProxy({
      baseURL: "http://ftp.us.debian.org/debian/",
      fetchImpl: stubFetch(debianHtml),
      render() { return new Response(""); },
      fetchHook(path) {
        captured = path;
        return null;
      },
    });

    await proxy.fetch(req("http://localhost/dists/trixie/InRelease"));
    expect(captured).toBe("dists/trixie/InRelease");
  });
});
