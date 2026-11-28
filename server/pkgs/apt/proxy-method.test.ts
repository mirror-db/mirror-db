import { afterEach, describe, expect, it, vi } from "vitest";

import { createAptProxy } from "./proxy";
import type { AptRepositoryFacts } from "./repository";

/**
 * Method forwarding through the **real** `ezfetch` (no module mock): the init
 * this proxy passes must survive `buildRequest`/`mdbfetch` and reach the network
 * layer as a HEAD, not a GET. Only `fetch` itself is stubbed.
 */

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

const cacheMatch = vi.fn();
const cachePut = vi.fn();
vi.stubGlobal("caches", {
  default: { match: cacheMatch, put: cachePut },
  open: async () => ({ match: cacheMatch, put: cachePut }),
});

afterEach(() => {
  fetchMock.mockReset();
  cacheMatch.mockReset();
  cachePut.mockReset();
});

const HASH = "a".repeat(64);

function makeProxy(hashes: string[] = []): ReturnType<typeof createAptProxy> {
  const base = "https://up.example/debian/";
  const facts: AptRepositoryFacts = {
    paths: new Set(),
    hashes: new Set(hashes),
    suites: new Set(),
    validUntil: Date.now() + 3_600_000,
    coverage: "metadata-only",
    status: { health: "healthy" },
  };
  return createAptProxy({
    base,
    url: (rel) => new URL(rel, base).href,
    facts: async () => facts,
  });
}

describe("createAptProxy — real ezfetch method forwarding", () => {
  it("sends a pool HEAD upstream as HEAD and returns no body", async () => {
    // Match the Fetch contract: a HEAD upstream response has no body.
    fetchMock.mockImplementation(async () =>
      new Response(null, { status: 200, headers: { "content-length": "7" } }),
    );

    const res = await makeProxy().fetch(
      new Request("https://mirs.uk/pool/main/n/nginx/nginx_1.0_amd64.deb", {
        method: "HEAD",
      }),
      "pool/main/n/nginx/nginx_1.0_amd64.deb",
    );

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.url).toBe(
      "https://up.example/debian/pool/main/n/nginx/nginx_1.0_amd64.deb",
    );
    expect(sent.method).toBe("HEAD");
    expect(await res.text()).toBe("");
  });

  it("sends a pool GET upstream as GET", async () => {
    fetchMock.mockImplementation(async () => new Response("data"));

    const res = await makeProxy().fetch(
      new Request("https://mirs.uk/pool/main/n/nginx/nginx_1.0_amd64.deb"),
      "pool/main/n/nginx/nginx_1.0_amd64.deb",
    );

    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.method).toBe("GET");
    expect(await res.text()).toBe("data");
  });
});

describe("createAptProxy — real cachedfetch Range handling", () => {
  it("forwards Range for a by-hash object and never stores the 206", async () => {
    fetchMock.mockImplementation(async () =>
      new Response("CDATA", {
        status: 206,
        headers: { "content-range": "bytes 2-6/8", "content-length": "5" },
      }),
    );

    const res = await makeProxy([HASH]).fetch(
      new Request(
        `https://mirs.uk/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
        { headers: { range: "bytes=2-6" } },
      ),
      `dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
    );

    expect(res.status).toBe(206);
    expect(res.headers.get("content-range")).toBe("bytes 2-6/8");
    // cachedfetch bypasses its read-through cache for a Range request, so the
    // partial body can never land under the full-URL key.
    expect(res.headers.get("x-mdb-cache")).toBe("bypass");
    expect(cacheMatch).not.toHaveBeenCalled();
    expect(cachePut).not.toHaveBeenCalled();

    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.url).toBe(
      `https://up.example/debian/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
    );
    expect(sent.headers.get("range")).toBe("bytes=2-6");
  });

  it("still answers a pool Range from byte 0 without forwarding it", async () => {
    fetchMock.mockImplementation(async () =>
      new Response("FULL", { status: 200, headers: { "content-length": "4" } }),
    );

    const res = await makeProxy().fetch(
      new Request("https://mirs.uk/pool/main/n/nginx/nginx_1.0_amd64.deb", {
        headers: { range: "bytes=2-3" },
      }),
      "pool/main/n/nginx/nginx_1.0_amd64.deb",
    );

    expect(res.status).toBe(200);
    // Known limitation, unchanged: pool/ files do not resume.
    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.headers.has("range")).toBe(false);
  });
});
