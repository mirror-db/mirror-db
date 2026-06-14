import { describe, expect, it, vi } from "vitest";

import {
  ProxyRequest,
  rewritePath,
  stripPrefix,
  setBaseUrl,
  stripSearch,
  filterHeaders,
  standardizeUserAgent,
  followRedirects,
  rewriteBody,
  upstreamProxy,
} from "./proxy";

// Mock cachedfetch — we test hooks in isolation, not the network layer.
vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: async (input: RequestInfo | URL) => {
    const req = new Request(input);
    return new Response(`upstream:${new URL(req.url).pathname}`, {
      status: 200,
      headers: { "content-type": "text/plain" },
    });
  },
  sanitizeResponse: (resp: Response) => resp,
}));

const req = (url: string, init?: RequestInit) => new Request(url, init);

describe("rewritePath", () => {
  it("transforms pathname via callback", () => {
    const hook = rewritePath((p) => `/library${p}`);
    const r = hook(req("https://example.com/nginx/latest")) as Request;
    expect(new URL(r.url).pathname).toBe("/library/nginx/latest");
  });

  it("preserves query string", () => {
    const hook = rewritePath((p) => p.replace("/old", "/new"));
    const r = hook(req("https://example.com/old/path?q=1")) as Request;
    const u = new URL(r.url);
    expect(u.pathname).toBe("/new/path");
    expect(u.search).toBe("?q=1");
  });
});

describe("stripPrefix", () => {
  it("removes prefix from pathname", () => {
    const hook = stripPrefix("/npm");
    const r = hook(req("https://mirs.uk/npm/react")) as Request;
    expect(new URL(r.url).pathname).toBe("/react");
  });

  it("only removes first occurrence", () => {
    const hook = stripPrefix("/ab");
    const r = hook(req("https://x.com/ab/cd/ab/ef")) as Request;
    expect(new URL(r.url).pathname).toBe("/cd/ab/ef");
  });

  it("preserves query string", () => {
    const hook = stripPrefix("/api");
    const r = hook(req("https://x.com/api/search?q=hello")) as Request;
    const u = new URL(r.url);
    expect(u.pathname).toBe("/search");
    expect(u.search).toBe("?q=hello");
  });
});

describe("setBaseUrl", () => {
  it("replaces origin, keeps pathname and query", () => {
    const hook = setBaseUrl("https://registry.npmjs.org");
    const r = hook(req("https://mirs.uk/react?version=latest")) as Request;
    const u = new URL(r.url);
    expect(u.origin).toBe("https://registry.npmjs.org");
    expect(u.pathname).toBe("/react");
    expect(u.search).toBe("?version=latest");
  });

  it("joins base path with request path", () => {
    const hook = setBaseUrl("https://upstream.com/prefix");
    const r = hook(req("https://mirs.uk/sub/path")) as Request;
    expect(new URL(r.url).pathname).toBe("/prefix/sub/path");
  });

  it("accepts URL object", () => {
    const hook = setBaseUrl(new URL("https://upstream.com"));
    const r = hook(req("https://mirs.uk/path")) as Request;
    expect(new URL(r.url).origin).toBe("https://upstream.com");
  });
});

describe("stripSearch", () => {
  it("removes query string", () => {
    const r = stripSearch(req("https://x.com/path?foo=bar&baz=1")) as Request;
    const u = new URL(r.url);
    expect(u.search).toBe("");
    expect(u.pathname).toBe("/path");
  });

  it("no-op when no query", () => {
    const r = stripSearch(req("https://x.com/path")) as Request;
    expect(new URL(r.url).search).toBe("");
  });
});

describe("filterHeaders", () => {
  it("only passes whitelisted headers", () => {
    const hook = filterHeaders(["Range", "Accept"]);
    const r = hook(
      req("https://x.com/", {
        headers: { Range: "bytes=0-99", Accept: "text/html", Cookie: "secret" },
      }),
    ) as Request;
    expect(r.headers.get("range")).toBe("bytes=0-99");
    expect(r.headers.get("accept")).toBe("text/html");
    expect(r.headers.has("cookie")).toBe(false);
  });

  it("case-insensitive matching", () => {
    const hook = filterHeaders(["RANGE"]);
    const r = hook(
      req("https://x.com/", { headers: { range: "bytes=0-10" } }),
    ) as Request;
    expect(r.headers.get("range")).toBe("bytes=0-10");
  });
});

describe("standardizeUserAgent", () => {
  it("adds user-agent if absent", () => {
    const r = standardizeUserAgent(req("https://x.com/")) as Request;
    expect(r.headers.get("user-agent")).toContain("mirror-db");
  });

  it("preserves existing user-agent", () => {
    const r = standardizeUserAgent(
      req("https://x.com/", { headers: { "user-agent": "custom/1.0" } }),
    ) as Request;
    expect(r.headers.get("user-agent")).toBe("custom/1.0");
  });
});

describe("followRedirects", () => {
  it("sets redirect to follow", () => {
    const r = followRedirects(req("https://x.com/")) as Request;
    expect(r.redirect).toBe("follow");
  });
});

describe("rewriteBody", () => {
  it("rewrites text response body", async () => {
    const hook = rewriteBody((body) => body.replace("foo", "bar"));
    const resp = new Response("hello foo world", {
      headers: { "content-type": "application/json" },
    });
    const result = hook(req("https://x.com/"), resp) as Response;
    expect(await result.text()).toBe("hello bar world");
  });

  it("removes content-length header", () => {
    const hook = rewriteBody((body) => body + " extra");
    const resp = new Response("body", {
      headers: { "content-type": "text/plain", "content-length": "4" },
    });
    const result = hook(req("https://x.com/"), resp) as Response;
    expect(result.headers.has("content-length")).toBe(false);
  });

  it("passes through non-text responses untouched", () => {
    const hook = rewriteBody((body) => body + "MODIFIED");
    const resp = new Response("binary", {
      headers: { "content-type": "application/octet-stream" },
    });
    const result = hook(req("https://x.com/"), resp);
    // Should return the same response object
    expect(result).toBe(resp);
  });

  it("passes through bodyless responses", () => {
    const hook = rewriteBody((body) => body + "X");
    const resp = new Response(null, {
      headers: { "content-type": "text/plain" },
    });
    const result = hook(req("https://x.com/"), resp);
    expect(result).toBe(resp);
  });

  it("handles various text content types", async () => {
    const hook = rewriteBody((body) => `[${body}]`);
    const types = [
      "application/json",
      "text/html; charset=utf-8",
      "application/xml",
      "application/javascript",
    ];
    for (const ct of types) {
      const resp = new Response("data", { headers: { "content-type": ct } });
      const result = hook(req("https://x.com/"), resp) as Response;
      expect(await result.text()).toBe("[data]");
    }
  });

  it("provides req and resp to transform fn", async () => {
    const hook = rewriteBody((body, r, res) => {
      return `${new URL(r.url).host}:${res.status}:${body}`;
    });
    const resp = new Response("ok", {
      status: 201,
      headers: { "content-type": "text/plain" },
    });
    const result = hook(req("https://mirs.uk/path"), resp) as Response;
    expect(await result.text()).toBe("mirs.uk:201:ok");
  });
});

describe("ProxyRequest", () => {
  it("applies pre hooks then fetches then post hooks", async () => {
    const p = new ProxyRequest();
    p.pre(stripPrefix("/mirror"));
    p.pre(setBaseUrl("https://upstream.com"));
    p.post((_, resp) => {
      const headers = new Headers(resp.headers);
      headers.set("x-proxied", "true");
      return new Response(resp.body, { status: resp.status, headers });
    });

    const resp = await p.apply(req("https://mirs.uk/mirror/data?q=1"));
    expect(resp.headers.get("x-proxied")).toBe("true");
    // cachedfetch mock returns `upstream:{pathname}`
    expect(await resp.text()).toBe("upstream:/data");
  });

  it("clone produces independent copy", () => {
    const a = new ProxyRequest();
    a.pre(stripPrefix("/a"));
    const b = a.clone();
    b.pre(stripPrefix("/b"));
    expect(a.prehooks).toHaveLength(1);
    expect(b.prehooks).toHaveLength(2);
  });
});

describe("upstreamProxy", () => {
  it("builds a ProxyRequest with standard hooks", async () => {
    const p = upstreamProxy({ url: "https://registry.npmjs.org", prefix: "/npm" });
    const resp = await p.apply(
      req("https://mirs.uk/npm/react?v=18", {
        headers: { Range: "bytes=0-99", Cookie: "secret", Accept: "text/html" },
      }),
    );
    // Mock returns pathname from the final request after hooks
    expect(await resp.text()).toBe("upstream:/react");
  });

  it("works without prefix", async () => {
    const p = upstreamProxy({ url: "https://upstream.com" });
    const resp = await p.apply(req("https://mirs.uk/some/path"));
    expect(await resp.text()).toBe("upstream:/some/path");
  });

  it("passes extra headers from passthroughHeaders", async () => {
    const p = upstreamProxy({
      url: "https://upstream.com",
      passthroughHeaders: ["Authorization"],
    });
    // Inject a hook to verify headers reached upstream
    const origPre = p.prehooks;
    let capturedHeaders: Headers | null = null;
    p.prehooks = [
      ...origPre,
      (r) => {
        capturedHeaders = r.headers;
        return r;
      },
    ];
    await p.apply(
      req("https://mirs.uk/path", {
        headers: { Authorization: "Bearer token", Cookie: "nope", Range: "bytes=0-1" },
      }),
    );
    expect(capturedHeaders!.get("authorization")).toBe("Bearer token");
    expect(capturedHeaders!.get("range")).toBe("bytes=0-1");
    expect(capturedHeaders!.has("cookie")).toBe(false);
  });
});
