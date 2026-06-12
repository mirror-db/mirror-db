import { describe, expect, it, vi, beforeEach } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: vi.fn(),
  sanitizeResponse: vi.fn((res: Response) => res),
}));

import { npm } from "./index";
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

describe("npm mirror", () => {
  it("has correct name and path", () => {
    expect(npm.name).toBe("npm");
    expect(npm.path).toBe("npm");
  });

  it("rewrites tarball URLs in JSON metadata", async () => {
    const meta = JSON.stringify({
      name: "lodash",
      versions: {
        "4.17.21": {
          dist: {
            tarball: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
            shasum: "abc123",
          },
        },
      },
    });
    mockCachedfetch.mockResolvedValue(
      new Response(meta, { headers: { "content-type": "application/json" } }),
    );

    const res = await npm.fetch(makeRequest("/npm/lodash"));
    const body = await res.text();
    const parsed = JSON.parse(body);

    expect(parsed.versions["4.17.21"].dist.tarball).toBe(
      "https://mirs.uk/npm/lodash/-/lodash-4.17.21.tgz",
    );
    expect(mockCachedfetch).toHaveBeenCalledWith("https://registry.npmjs.org/lodash");
  });

  it("rewrites scoped package tarball URLs", async () => {
    const meta = JSON.stringify({
      name: "@babel/core",
      versions: {
        "7.24.0": {
          dist: {
            tarball: "https://registry.npmjs.org/@babel/core/-/core-7.24.0.tgz",
          },
        },
      },
    });
    mockCachedfetch.mockResolvedValue(
      new Response(meta, { headers: { "content-type": "application/json" } }),
    );

    const res = await npm.fetch(makeRequest("/npm/@babel/core"));
    const body = await res.text();
    const parsed = JSON.parse(body);

    expect(parsed.versions["7.24.0"].dist.tarball).toBe(
      "https://mirs.uk/npm/@babel/core/-/core-7.24.0.tgz",
    );
  });

  it("passes through tarballs without rewriting", async () => {
    mockCachedfetch.mockResolvedValue(
      new Response("binary-data", {
        headers: { "content-type": "application/octet-stream" },
      }),
    );

    const res = await npm.fetch(makeRequest("/npm/lodash/-/lodash-4.17.21.tgz"));
    expect(await res.text()).toBe("binary-data");
    expect(mockCachedfetch).toHaveBeenCalledWith(
      "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
    );
  });

  it("passes through search API", async () => {
    const results = JSON.stringify({ objects: [] });
    mockCachedfetch.mockResolvedValue(
      new Response(results, { headers: { "content-type": "application/json" } }),
    );

    await npm.fetch(makeRequest("/npm/-/v1/search?text=lodash"));
    expect(mockCachedfetch).toHaveBeenCalledWith(
      "https://registry.npmjs.org/-/v1/search?text=lodash",
    );
  });
});
