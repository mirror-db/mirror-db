import { afterEach, describe, expect, it, vi } from "vitest";

import { createAptProxy } from "./proxy";
import { AptRepo } from "./repo";

/**
 * Method forwarding through the **real** `ezfetch` (no module mock): the init
 * this proxy passes must survive `buildRequest`/`mdbfetch` and reach the network
 * layer as a HEAD, not a GET. Only `fetch` itself is stubbed.
 */

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

afterEach(() => {
  fetchMock.mockReset();
});

function makeProxy(): ReturnType<typeof createAptProxy> {
  const repo = new AptRepo({ base: "https://up.example/debian/" });
  repo.state = "ready";
  repo.knownPaths = new Set();
  repo.knownHashes = new Set();
  repo.knownSuites = new Set();
  repo.validUntil = Date.now() + 3_600_000;
  // Keep the hand-set index: a real resolve would hit the stub again.
  repo.awaitResolved = async () => {};
  return createAptProxy(repo);
}

describe("createAptProxy — real ezfetch method forwarding", () => {
  it("sends a pool HEAD upstream as HEAD and returns no body", async () => {
    fetchMock.mockImplementation(async () =>
      new Response("ignored", { status: 200, headers: { "content-length": "7" } }),
    );

    const res = await makeProxy().fetch(
      new Request("https://mirs.uk/pool/main/n/nginx/nginx_1.0_amd64.deb", {
        method: "HEAD",
      }),
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
    );

    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.method).toBe("GET");
    expect(await res.text()).toBe("data");
  });
});
