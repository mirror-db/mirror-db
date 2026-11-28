import { describe, expect, it, vi } from "vitest";

import {
  ProxyRequest,
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
const mount = (relativePath = "") => ({ relativePath });

describe("setBaseUrl", () => {
  it("maps the explicit relative path and preserves the original query", () => {
    const hook = setBaseUrl("https://registry.npmjs.org");
    const r = hook(
      req("https://mirs.uk/npm/ignored?version=latest"),
      mount("react"),
    ) as Request;
    const u = new URL(r.url);
    expect(u.origin).toBe("https://registry.npmjs.org");
    expect(u.pathname).toBe("/react");
    expect(u.search).toBe("?version=latest");
  });

  it("joins a base path with the relative path", () => {
    const hook = setBaseUrl("https://upstream.com/prefix");
    const r = hook(req("https://mirs.uk/mount/ignored"), mount("sub/path")) as Request;
    expect(new URL(r.url).pathname).toBe("/prefix/sub/path");
  });

  it("accepts URL object", () => {
    const hook = setBaseUrl(new URL("https://upstream.com"));
    const r = hook(req("https://mirs.uk/path"), mount("path")) as Request;
    expect(new URL(r.url).origin).toBe("https://upstream.com");
  });

  it.each([
    ["", "https://upstream.com/base/"],
    ["nested/path", "https://upstream.com/base/nested/path"],
    ["/empty//segment", "https://upstream.com/base//empty//segment"],
    ["escaped%2fCase%2F", "https://upstream.com/base/escaped%2fCase%2F"],
  ])("preserves relative path %s verbatim", async (relativePath, expected) => {
    const hook = setBaseUrl("https://upstream.com/base/");
    const r = await hook(req("https://mirs.uk/mount/a-different-path"), mount(relativePath));
    expect(r.url).toBe(expected);
  });
});

describe("stripSearch", () => {
  it("removes query string", () => {
    const r = stripSearch(req("https://x.com/path?foo=bar&baz=1"), mount()) as Request;
    const u = new URL(r.url);
    expect(u.search).toBe("");
    expect(u.pathname).toBe("/path");
  });

  it("no-op when no query", () => {
    const r = stripSearch(req("https://x.com/path"), mount()) as Request;
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
      mount(),
    ) as Request;
    expect(r.headers.get("range")).toBe("bytes=0-99");
    expect(r.headers.get("accept")).toBe("text/html");
    expect(r.headers.has("cookie")).toBe(false);
  });

  it("case-insensitive matching", () => {
    const hook = filterHeaders(["RANGE"]);
    const r = hook(
      req("https://x.com/", { headers: { range: "bytes=0-10" } }),
      mount(),
    ) as Request;
    expect(r.headers.get("range")).toBe("bytes=0-10");
  });

  it("preserves request method", () => {
    const hook = filterHeaders(["Range"]);
    const r = hook(
      req("https://x.com/", {
        method: "HEAD",
        headers: { range: "bytes=0-10" },
      }),
      mount(),
    ) as Request;
    expect(r.method).toBe("HEAD");
  });
});

describe("standardizeUserAgent", () => {
  it("adds user-agent if absent", () => {
    const r = standardizeUserAgent(req("https://x.com/"), mount()) as Request;
    expect(r.headers.get("user-agent")).toContain("mirror-db");
  });

  it("preserves existing user-agent", () => {
    const r = standardizeUserAgent(
      req("https://x.com/", { headers: { "user-agent": "custom/1.0" } }),
      mount(),
    ) as Request;
    expect(r.headers.get("user-agent")).toBe("custom/1.0");
  });
});

describe("followRedirects", () => {
  it("sets redirect to follow", () => {
    const r = followRedirects(req("https://x.com/"), mount()) as Request;
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
    p.pre((request) => request);
    p.pre(setBaseUrl("https://upstream.com"));
    p.post((_, resp) => {
      const headers = new Headers(resp.headers);
      headers.set("x-proxied", "true");
      return new Response(resp.body, { status: resp.status, headers });
    });

    const resp = await p.apply(req("https://mirs.uk/mirror/ignored?q=1"), mount("data"));
    expect(resp.headers.get("x-proxied")).toBe("true");
    // cachedfetch mock returns `upstream:{pathname}`
    expect(await resp.text()).toBe("upstream:/data");
  });

  it("clone produces independent copy", () => {
    const a = new ProxyRequest();
    a.pre((request) => request);
    const b = a.clone();
    b.pre((request) => request);
    expect(a.prehooks).toHaveLength(1);
    expect(b.prehooks).toHaveLength(2);
  });
});

describe("upstreamProxy", () => {
  it("builds a ProxyRequest with standard hooks", async () => {
    const p = upstreamProxy({ url: "https://registry.npmjs.org" });
    const resp = await p.apply(
      req("https://mirs.uk/npm/ignored?v=18", {
        headers: { Range: "bytes=0-99", Cookie: "secret", Accept: "text/html" },
      }),
      mount("react"),
    );
    // Mock returns pathname from the final request after hooks
    expect(await resp.text()).toBe("upstream:/react");
  });

  it("maps any explicit relative path without inspecting the mount URL", async () => {
    const p = upstreamProxy({ url: "https://upstream.com" });
    const resp = await p.apply(
      req("https://mirs.uk/not-the-mount/path"),
      mount("some/path"),
    );
    expect(await resp.text()).toBe("upstream:/some/path");
  });

  it("HEAD request preserves method through full chain", async () => {
    const p = upstreamProxy({ url: "https://upstream.com" });

    // Capture the request that reaches cachedfetch
    let capturedMethod: string | null = null;
    const origPre = p.prehooks;
    p.prehooks = [
      ...origPre,
      (r) => {
        capturedMethod = r.method;
        return r;
      },
    ];

    await p.apply(req("https://mirs.uk/path", { method: "HEAD" }), mount("path"));
    expect(capturedMethod).toBe("HEAD");
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
      mount("path"),
    );
    expect(capturedHeaders!.get("authorization")).toBe("Bearer token");
    expect(capturedHeaders!.get("range")).toBe("bytes=0-1");
    expect(capturedHeaders!.has("cookie")).toBe(false);
  });
});
