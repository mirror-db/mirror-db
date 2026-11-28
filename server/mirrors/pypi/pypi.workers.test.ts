import { describe, expect, it, vi, beforeEach } from "vitest";

// HTMLRewriter is available in the workers pool — test the rewriting logic.
vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: vi.fn(),
  mdbfetch: vi.fn(),
  sanitizeResponse: vi.fn((res: Response) => res),
}));

import { pypi } from "./index";
import { cachedfetch, mdbfetch, sanitizeResponse } from "@server/pkgs/fetch";

const mockCachedfetch = cachedfetch as ReturnType<typeof vi.fn>;
const mockMdbfetch = mdbfetch as ReturnType<typeof vi.fn>;
const mockSanitize = sanitizeResponse as ReturnType<typeof vi.fn>;

function makeRequest(path: string) {
  const relativePath = path.replace(/^\/pypi\/?/, "").split("?", 1)[0]!;
  return [new Request(`https://mirs.uk${path}`), { relativePath }] as const;
}

beforeEach(() => {
  vi.clearAllMocks();
  mockMdbfetch.mockReset();
  mockSanitize.mockImplementation((res: Response) => res);
});

describe("pypi simple/ rewriting (HTMLRewriter)", () => {
  it("rewrites /simple/pkg/ hrefs to relative in index", async () => {
    const html = `<html><body>
      <a href="/simple/requests/">requests</a>
      <a href="/simple/flask/">flask</a>
    </body></html>`;
    mockCachedfetch.mockResolvedValue(
      new Response(html, { headers: { "content-type": "text/html" } }),
    );

    const res = await pypi.fetch(...makeRequest("/pypi/simple/"));
    const body = await res.text();

    expect(body).toContain('href="requests/"');
    expect(body).toContain('href="flask/"');
    expect(body).not.toContain("/simple/");
  });

  it("rewrites files.pythonhosted.org URLs in package page", async () => {
    const html = `<html><body>
      <a href="https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz#sha256=abc123">requests-2.31.0.tar.gz</a>
      <a href="https://files.pythonhosted.org/packages/ef/gh/requests-2.30.0-py3-none-any.whl#sha256=def456">requests-2.30.0-py3-none-any.whl</a>
    </body></html>`;
    mockCachedfetch.mockResolvedValue(
      new Response(html, { headers: { "content-type": "text/html" } }),
    );

    const res = await pypi.fetch(...makeRequest("/pypi/simple/requests/"));
    const body = await res.text();

    expect(body).toContain('href="../../packages/ab/cd/requests-2.31.0.tar.gz#sha256=abc123"');
    expect(body).toContain('href="../../packages/ef/gh/requests-2.30.0-py3-none-any.whl#sha256=def456"');
    expect(body).not.toContain("files.pythonhosted.org");
  });

  it("passes through non-200 simple responses", async () => {
    mockCachedfetch.mockResolvedValue(new Response(null, { status: 404 }));

    const res = await pypi.fetch(...makeRequest("/pypi/simple/nonexistent/"));
    expect(res.status).toBe(404);
  });
});

describe("pypi prefetch boundary", () => {
  const simpleIndex = `<html><body><a href="/simple/requests/">requests</a></body></html>`;

  it("marks the mount-relative simple index", async () => {
    for (const path of ["/pypi/simple/", "/pypi/simple"]) {
      mockCachedfetch.mockResolvedValue(
        new Response(simpleIndex, { headers: { "content-type": "text/html" } }),
      );
      const res = await pypi.fetch(...makeRequest(path));
      expect(res.headers.get("x-mdb-relay"), path).toBe("prefetch");
    }
  });

  it("never marks per-package pages, the JSON API or file downloads", async () => {
    const html = `<html><body><a href="https://files.pythonhosted.org/packages/ab/cd/x.whl">x</a></body></html>`;
    const cases = [
      "/pypi/simple/requests/",
      "/pypi/simple/requests",
      "/pypi/pypi/requests/json",
      "/pypi/packages/ab/cd/requests-2.31.0.tar.gz",
    ];
    for (const path of cases) {
      mockCachedfetch.mockResolvedValue(
        new Response(html, { headers: { "content-type": "text/html" } }),
      );
      const res = await pypi.fetch(...makeRequest(path));
      expect(res.headers.get("x-mdb-relay"), path).toBeNull();
    }
  });

  it("does not advertise prefetch on a failed index fetch", async () => {
    mockCachedfetch.mockResolvedValue(new Response(null, { status: 502 }));
    const res = await pypi.fetch(...makeRequest("/pypi/simple/"));
    expect(res.headers.get("x-mdb-relay")).toBeNull();
  });

  it("keeps prefetch on a 304 revalidation of the simple index", async () => {
    // A conditional request goes to pypi.org, not the read-through cache.
    mockMdbfetch.mockImplementation(async (request: Request) => {
      expect(request.headers.get("if-none-match")).toBe('"index-1"');
      return new Response(null, { status: 304, headers: { etag: '"index-1"' } });
    });

    const res = await pypi.fetch(
      new Request("https://mirs.uk/pypi/simple/", {
        headers: { "if-none-match": '"index-1"' },
      }),
      { relativePath: "simple/" },
    );

    expect(res.status).toBe(304);
    expect(res.headers.get("x-mdb-relay")).toBe("prefetch");
    expect(mockCachedfetch).not.toHaveBeenCalled();
  });

  it("rewrites links on a conditional 200", async () => {
    mockMdbfetch.mockResolvedValue(
      new Response(simpleIndex, { headers: { "content-type": "text/html" } }),
    );

    const res = await pypi.fetch(
      new Request("https://mirs.uk/pypi/simple/", {
        headers: { "if-none-match": '"old"' },
      }),
      { relativePath: "simple/" },
    );

    expect(res.status).toBe(200);
    expect(await res.text()).toContain('href="requests/"');
    expect(res.headers.get("x-mdb-relay")).toBe("prefetch");
  });
});
