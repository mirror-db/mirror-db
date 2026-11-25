import { afterEach, describe, expect, it, vi } from "vitest";

import { createAptProxy } from "./proxy";
import { AptRepo } from "./repo";

/**
 * The by-hash branch goes through the **real** `cachedfetch`, so this has to run
 * where the Cache API exists. A HEAD lookup can match a stored GET, and the body
 * it returns must never be relayed for a HEAD.
 */

const HASH = "a".repeat(64);
const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

afterEach(() => {
  fetchMock.mockReset();
});

function makeProxy(): ReturnType<typeof createAptProxy> {
  const repo = new AptRepo({ base: "https://up.example/debian/" });
  repo.state = "ready";
  repo.knownPaths = new Set([`dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`]);
  repo.knownHashes = new Set([HASH]);
  repo.knownSuites = new Set(["trixie"]);
  repo.validUntil = Date.now() + 3_600_000;
  repo.awaitResolved = async () => {};
  return createAptProxy(repo);
}

describe("createAptProxy — real cachedfetch HEAD forwarding", () => {
  it("sends a by-hash HEAD upstream as HEAD and returns no body", async () => {
    fetchMock.mockImplementation(async () =>
      new Response("ignored", { status: 200, headers: { "content-length": "7" } }),
    );

    const res = await makeProxy().fetch(
      new Request(
        `https://mirs.uk/dists/trixie/main/binary-amd64/by-hash/SHA256/${HASH}`,
        { method: "HEAD" },
      ),
    );

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const sent = fetchMock.mock.calls[0]![0] as Request;
    expect(sent.method).toBe("HEAD");
    expect(await res.text()).toBe("");
    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=31536000, immutable",
    );
  });
});
