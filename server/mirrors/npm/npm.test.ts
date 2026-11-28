import { describe, expect, it, vi, beforeEach } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: vi.fn(),
  sanitizeResponse: vi.fn((res: Response) => res),
}));

import { npm } from "./index";
import { cachedfetch } from "@server/pkgs/fetch";

const mockCachedfetch = cachedfetch as ReturnType<typeof vi.fn>;

function makeRequest(path: string, headers?: Record<string, string>) {
  const relativePath = path.replace(/^\/npm\/?/, "").split("?", 1)[0]!;
  return [
    new Request(`https://mirs.uk${path}`, { headers }),
    { relativePath },
  ] as const;
}

/** Extract the URL string from the first argument of a cachedfetch call. */
function fetchedUrl(callIndex = 0): string {
  const arg = mockCachedfetch.mock.calls[callIndex][0];
  if (arg instanceof Request) return arg.url;
  if (arg instanceof URL) return arg.toString();
  return String(arg);
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("npm mirror", () => {
  it("has correct name and path", () => {
    expect(npm.name).toBe("npm");
    expect(npm.path).toBe("npm");
  });

  it("rewrites tarball URLs to the relay host when the request came through one", async () => {
    // `request.url` is always the Worker origin. Rewriting to it would send the
    // tarball download straight past the relay, which is the whole problem the
    // relay exists to solve.
    const meta = JSON.stringify({
      name: "lodash",
      versions: {
        "4.17.21": {
          dist: { tarball: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz" },
        },
      },
    });
    mockCachedfetch.mockResolvedValue(
      new Response(meta, { headers: { "content-type": "application/json" } }),
    );

    const res = await npm.fetch(
      ...makeRequest("/npm/lodash", { "x-mdb-relay-host": "relay.example.com" }),
    );
    const body = (await res.json()) as any;
    expect(body.versions["4.17.21"].dist.tarball).toBe(
      "https://relay.example.com/npm/lodash/-/lodash-4.17.21.tgz",
    );
    // The post hook needs the original relay context; it is not upstream data.
    const upstream = mockCachedfetch.mock.calls[0][0] as Request;
    expect(upstream.headers.get("x-mdb-relay-host")).toBeNull();
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

    const res = await npm.fetch(...makeRequest("/npm/lodash"));
    const body = await res.text();
    const parsed = JSON.parse(body);

    expect(parsed.versions["4.17.21"].dist.tarball).toBe(
      "https://mirs.uk/npm/lodash/-/lodash-4.17.21.tgz",
    );
    expect(fetchedUrl()).toBe("https://registry.npmjs.org/lodash");
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

    const res = await npm.fetch(...makeRequest("/npm/@babel/core"));
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

    const res = await npm.fetch(...makeRequest("/npm/lodash/-/lodash-4.17.21.tgz"));
    expect(await res.text()).toBe("binary-data");
    expect(fetchedUrl()).toBe(
      "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
    );
  });

  it("passes through search API with query string", async () => {
    const results = JSON.stringify({ objects: [] });
    mockCachedfetch.mockResolvedValue(
      new Response(results, { headers: { "content-type": "application/json" } }),
    );

    await npm.fetch(...makeRequest("/npm/-/v1/search?text=lodash"));
    expect(fetchedUrl()).toBe(
      "https://registry.npmjs.org/-/v1/search?text=lodash",
    );
  });

  it.each([
    ["", "https://registry.npmjs.org/?download=a%2Fb"],
    ["nested/package", "https://registry.npmjs.org/nested/package?download=a%2Fb"],
    [
      "/empty///segment/%2fCase%2F",
      "https://registry.npmjs.org//empty///segment/%2fCase%2F?download=a%2Fb",
    ],
  ])("maps explicit rel %s without reading it from the request URL", async (relativePath, expected) => {
    mockCachedfetch.mockResolvedValue(
      new Response("binary", { headers: { "content-type": "application/octet-stream" } }),
    );

    await npm.fetch(
      makeRequest("/npm/not-the-relative-path?download=a%2Fb")[0],
      { relativePath },
    );

    expect(fetchedUrl()).toBe(expected);
  });
});
