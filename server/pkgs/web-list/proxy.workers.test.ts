import { describe, expect, it, vi } from "vitest";

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
  return [
    new Request(url, { method, headers }),
    new URL(url).pathname.slice(1),
  ] as const;
}

// ── JSON API ────────────────────────────────────────────────────────────────

describe("WebListProxy — JSON API", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET /?format=json returns structured entries", async () => {
    const res = await proxy.fetch(...req("http://localhost/?format=json"));
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
    const res = await proxy2.fetch(...req("http://localhost/dists/?format=json"));
    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.path).toBe("dists/");
  });
});

// ── Web listing (render) ────────────────────────────────────────────────────

describe("WebListProxy — Web listing", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET / calls the render callback", async () => {
    const res = await proxy.fetch(...req("http://localhost/"));
    expect(res.status).toBe(200);
    const body: any = await res.json();
    expect(body.rendered).toBe(true);
    expect(body.path).toBe("");
    expect(body.count).toBeGreaterThan(5);
  });

  it("GET /pool/ renders subdirectory", async () => {
    const res = await proxy.fetch(...req("http://localhost/pool/"));
    const body: any = await res.json();
    expect(body.rendered).toBe(true);
    expect(body.path).toBe("pool/");
  });

  it("GET /pool (no trailing slash) still lists pool, not its parent", async () => {
    const seen: string[] = [];
    const tracking = new WebListProxy({
      baseURL: "http://ftp.us.debian.org/debian/",
      fetchImpl: async (input: string | URL | Request) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        seen.push(url);
        return new Response(debianHtml, { headers: { "content-type": "text/html" } });
      },
      render(entries: WebListEntry[], path: string) {
        return new Response(JSON.stringify({ rendered: true, path, count: entries.length }));
      },
    });

    const res = await tracking.fetch(...req("http://localhost/pool?format=json"));
    expect(res.status).toBe(200);
    expect(seen).toEqual(["http://ftp.us.debian.org/debian/pool/"]);
  });
});

// ── File passthrough ────────────────────────────────────────────────────────

describe("WebListProxy — File passthrough", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("GET /README proxies the file from upstream", async () => {
    const res = await proxy.fetch(...req("http://localhost/README"));
    expect(res.status).toBe(200);
    const body = await res.text();
    expect(body).toContain("file-content-of:");
    expect(body).toContain("/README");
  });

  it("sanitises a file delivered through the default cachedfetch", async () => {
    const upstream = vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      new Response("file", {
        status: 200,
        headers: {
          "www-authenticate": 'Bearer realm="upstream"',
          "set-cookie": "session=secret",
          "set-cookie2": "legacy=secret",
          "x-mdb-relay": "no-store",
          age: "12",
          connection: "keep-alive",
          "keep-alive": "timeout=5",
          "transfer-encoding": "chunked",
          "content-length": "4",
          "x-upstream": "kept",
          vary: "Accept",
        },
      }),
    );
    const proxy = new WebListProxy({
      baseURL: `https://web-list-security.invalid/${Math.random()}/`,
      render: () => new Response("unused"),
    });

    try {
      const res = await proxy.fetch(...req("http://localhost/README"));
      for (const name of [
        "www-authenticate",
        "set-cookie",
        "set-cookie2",
        "x-mdb-relay",
        "age",
        "connection",
        "keep-alive",
        "transfer-encoding",
      ]) {
        expect(res.headers.has(name), name).toBe(false);
      }
      expect(res.headers.get("x-upstream")).toBe("kept");
      expect(res.headers.get("vary")).toBe("Accept");
      expect(await res.text()).toBe("file");
    } finally {
      upstream.mockRestore();
    }
  });
});

// ── WebDAV ──────────────────────────────────────────────────────────────────

describe("WebListProxy — WebDAV", () => {
  const proxy = makeProxy("http://ftp.us.debian.org/debian/", debianHtml);

  it("OPTIONS returns DAV:1 and correct Allow", async () => {
    const res = await proxy.fetch(...req("http://localhost/", "OPTIONS"));
    expect(res.status).toBe(204);
    expect(res.headers.get("dav")).toBe("1");
    expect(res.headers.get("allow")).toContain("PROPFIND");
    expect(res.headers.get("allow")).toContain("GET");
  });

  it("PROPFIND / Depth:1 returns 207 with entries", async () => {
    const res = await proxy.fetch(
      ...req("http://localhost/", "PROPFIND", { depth: "1" }),
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
      ...req("http://localhost/", "PROPFIND", { depth: "0" }),
    );
    expect(res.status).toBe(207);
    const xml = await res.text();
    // Only one <D:response> (the directory itself)
    const responseCount = (xml.match(/<D:response>/g) || []).length;
    expect(responseCount).toBe(1);
  });

  it("combines the configured baseHref with the explicit relative path", async () => {
    const mounted = new WebListProxy({
      baseURL: "http://ftp.us.debian.org/debian/",
      baseHref: "/debian-cd/",
      fetchImpl: stubFetch(debianHtml),
      render: () => new Response("unused"),
    });
    const [request] = req(
      "https://mirs.uk/debian-cd/dists/?format=not-json",
      "PROPFIND",
      { depth: "0" },
    );

    const res = await mounted.fetch(request, "dists/");

    expect(res.status).toBe(207);
    expect(await res.text()).toContain("<D:href>/debian-cd/dists/</D:href>");
  });

  it("PUT returns 405 Method Not Allowed", async () => {
    const res = await proxy.fetch(...req("http://localhost/test", "PUT"));
    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toContain("GET");
  });

  it("DELETE returns 405", async () => {
    const res = await proxy.fetch(...req("http://localhost/test", "DELETE"));
    expect(res.status).toBe(405);
  });
});

// ── Listing recognition ─────────────────────────────────────────────────────

describe("WebListProxy — listing recognition", () => {
  const recognizedEmpty = "<table><tr><th>Name</th><th>Last modified</th><th>Size</th></tr></table>";
  const unrecognized = "<html><body><h1>Welcome</h1><pre>Sign in to continue.</pre></body></html>";
  const unrecognizedPages = [
    { name: "navigation-only bare ul", html: '<ul><li><a href="/login">Sign in</a></li></ul>' },
    {
      name: "single-column product table",
      html: "<table><tr><th>Product Name</th></tr><tr><td>Keyboard</td></tr></table>",
    },
  ];

  it("renders a recognized empty HTML listing", async () => {
    const proxy = makeProxy("https://empty.invalid/", recognizedEmpty);
    const res = await proxy.fetch(...req("http://localhost/"));

    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ rendered: true, count: 0 });
  });

  it("returns an empty entries array for a recognized empty JSON listing", async () => {
    const proxy = makeProxy("https://empty.invalid/", recognizedEmpty);
    const res = await proxy.fetch(...req("http://localhost/?format=json"));

    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ path: "", entries: [] });
  });

  it("answers 502/no-store instead of rendering a successful non-listing page", async () => {
    const proxy = makeProxy("https://portal.invalid/", unrecognized);
    for (const path of ["/", "/?format=json"]) {
      const res = await proxy.fetch(...req(`http://localhost${path}`));

      expect(res.status).toBe(502);
      expect(res.headers.get("cache-control")).toBe("no-store");
      expect(await res.text()).toBe("Bad Gateway");
    }
  });

  it.each(unrecognizedPages)(
    "answers 502/no-store for a $name with the default parsers",
    async ({ html }) => {
      const proxy = makeProxy("https://portal.invalid/", html);
      for (const path of ["/", "/?format=json"]) {
        const res = await proxy.fetch(...req(`http://localhost${path}`));

        expect(res.status).toBe(502);
        expect(res.headers.get("cache-control")).toBe("no-store");
        expect(await res.text()).toBe("Bad Gateway");
      }
    },
  );

  it("answers PROPFIND for a recognized empty listing with only itself", async () => {
    const proxy = makeProxy("https://empty.invalid/", recognizedEmpty);
    const res = await proxy.fetch(
      ...req("http://localhost/empty/", "PROPFIND", { depth: "1" }),
    );

    expect(res.status).toBe(207);
    const xml = await res.text();
    expect((xml.match(/<D:response>/g) || []).length).toBe(1);
    expect(xml).toContain("<D:collection/>");
  });

  it("answers 502/no-store for PROPFIND on a successful non-listing page", async () => {
    const proxy = makeProxy("https://portal.invalid/", unrecognized);
    const res = await proxy.fetch(
      ...req("http://localhost/dists/", "PROPFIND", { depth: "1" }),
    );

    expect(res.status).toBe(502);
    expect(res.headers.get("cache-control")).toBe("no-store");
  });
});

// ── Failed listings ─────────────────────────────────────────────────────────

describe("WebListProxy — upstream without a listing", () => {
  const proxyReturning = (status: number) =>
    new WebListProxy({
      baseURL: "https://no-autoindex.invalid/files/",
      fetchImpl: async () => new Response("nope", { status }),
      render() {
        return new Response("should not render");
      },
    });

  it("answers 404 when the upstream has no directory listing", async () => {
    // mise.jdx.dev and cli.github.com serve files but no autoindex. Before this
    // was handled, the thrown readdir error escaped the handler and the Worker
    // answered `500 error code: 1101`.
    const proxy = proxyReturning(404);
    for (const path of ["/dists/", "/dists/?format=json"]) {
      const res = await proxy.fetch(...req(`http://localhost${path}`));
      expect(res.status).toBe(404);
    }
  });

  it("answers 404 for PROPFIND on such a directory", async () => {
    const res = await proxyReturning(404).fetch(
      ...req("http://localhost/dists/", "PROPFIND", { depth: "0" }),
    );
    expect(res.status).toBe(404);
  });

  it("answers 502 when the upstream itself failed", async () => {
    const res = await proxyReturning(500).fetch(...req("http://localhost/dists/"));
    expect(res.status).toBe(502);
  });

  it("answers 502 when the fetch throws", async () => {
    const proxy = new WebListProxy({
      baseURL: "https://down.invalid/",
      fetchImpl: async () => {
        throw new TypeError("network down");
      },
      render() {
        return new Response("");
      },
    });
    expect((await proxy.fetch(...req("http://localhost/"))).status).toBe(502);
  });

  it("does not cache a failed listing", async () => {
    const res = await proxyReturning(404).fetch(...req("http://localhost/dists/"));
    expect(res.headers.get("cache-control")).toBe("no-store");
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

    const hooked = await proxy.fetch(...req("http://localhost/special"));
    expect(hooked.status).toBe(299);
    expect(await hooked.text()).toBe("hooked!");

    // Non-matched paths still fall through normally.
    const normal = await proxy.fetch(...req("http://localhost/README"));
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

    await proxy.fetch(...req("http://localhost/dists/trixie/InRelease"));
    expect(captured).toBe("dists/trixie/InRelease");
  });

  it("receives the original request and an independent, verbatim rel", async () => {
    const request = new Request(
      "https://mirs.uk/debian-cd/not-the-relative-path?download=a%2Fb",
      { headers: { "x-mdb-relay-host": "relay.example.com" } },
    );
    let seenPath: string | undefined;
    let seenRequest: Request | undefined;
    const proxy = new WebListProxy({
      baseURL: "http://ftp.us.debian.org/debian/",
      render: () => new Response("unused"),
      fetchHook(path, original) {
        seenPath = path;
        seenRequest = original;
        return new Response(null, { status: 204 });
      },
    });

    const res = await proxy.fetch(request, "/empty//segment/%2fCase%2F");

    expect(res.status).toBe(204);
    expect(seenPath).toBe("/empty//segment/%2fCase%2F");
    expect(seenRequest).toBe(request);
    expect(seenRequest!.headers.get("x-mdb-relay-host")).toBe("relay.example.com");
    expect(new URL(seenRequest!.url).search).toBe("?download=a%2Fb");
  });
});
