import { describe, expect, it, vi, beforeEach } from "vitest";

// Mock cachedfetch + sanitizeResponse before importing the module under test.
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

describe("pypi mirror", () => {
  it("has correct name and path", () => {
    expect(pypi.name).toBe("pypi");
    expect(pypi.path).toBe("/pypi/");
  });

  it("proxies JSON API to pypi.org", async () => {
    const json = { info: { name: "requests" } };
    mockCachedfetch.mockResolvedValue(new Response(JSON.stringify(json)));

    const res = await pypi.fetch(makeRequest("/pypi/pypi/requests/json"));

    expect(mockCachedfetch).toHaveBeenCalledWith("https://pypi.org/pypi/requests/json");
    expect(res.status).toBe(200);
  });

  it("proxies package file downloads to files.pythonhosted.org", async () => {
    mockCachedfetch.mockResolvedValue(new Response("file-bytes"));

    const res = await pypi.fetch(
      makeRequest("/pypi/packages/ab/cd/requests-2.31.0.tar.gz"),
    );

    expect(mockCachedfetch).toHaveBeenCalledWith(
      "https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz",
    );
    expect(res.status).toBe(200);
  });

  it("returns 404 for unknown paths", async () => {
    const res = await pypi.fetch(makeRequest("/pypi/unknown/path"));
    expect(res.status).toBe(404);
  });
});
