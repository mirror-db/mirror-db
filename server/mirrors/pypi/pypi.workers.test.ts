import { describe, expect, it, vi, beforeEach } from "vitest";

// HTMLRewriter is available in the workers pool — test the rewriting logic.
vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: vi.fn(),
  sanitizeResponse: vi.fn((res: Response) => res),
}));

import { pypi } from "./index";
import { cachedfetch, sanitizeResponse } from "@server/pkgs/fetch";

const mockCachedfetch = cachedfetch as ReturnType<typeof vi.fn>;
const mockSanitize = sanitizeResponse as ReturnType<typeof vi.fn>;

function makeRequest(path: string): Request {
  return new Request(`https://mirs.uk${path}`);
}

beforeEach(() => {
  vi.clearAllMocks();
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

    const res = await pypi.fetch(makeRequest("/pypi/simple/"));
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

    const res = await pypi.fetch(makeRequest("/pypi/simple/requests/"));
    const body = await res.text();

    expect(body).toContain('href="../../packages/ab/cd/requests-2.31.0.tar.gz#sha256=abc123"');
    expect(body).toContain('href="../../packages/ef/gh/requests-2.30.0-py3-none-any.whl#sha256=def456"');
    expect(body).not.toContain("files.pythonhosted.org");
  });

  it("passes through non-200 simple responses", async () => {
    mockCachedfetch.mockResolvedValue(new Response(null, { status: 404 }));

    const res = await pypi.fetch(makeRequest("/pypi/simple/nonexistent/"));
    expect(res.status).toBe(404);
  });
});
