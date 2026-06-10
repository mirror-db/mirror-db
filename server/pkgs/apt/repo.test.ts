import { describe, expect, it } from "vitest";

import { AptRepo, type FetchImpl } from "./repo";

const HASH_A = "a".repeat(64);
const VALID_UNTIL = "Sat, 14 Jun 2026 10:00:00 UTC";

const release = (suite: string) => `Suite: ${suite}
Codename: ${suite}
Valid-Until: ${VALID_UNTIL}
SHA256:
 ${HASH_A}  1234 main/binary-amd64/Packages
`;

/**
 * Mock fetch serving InRelease for the given suites; everything else 404s.
 * (HTMLRewriter is absent in Node, so the repo falls back to its configured
 * suite list — this exercises exactly that path.)
 */
function mockUpstream(suites: string[]): { fetchImpl: FetchImpl; urls: string[] } {
  const urls: string[] = [];
  const fetchImpl: FetchImpl = async (input) => {
    const url = input.toString();
    urls.push(url);
    const m = url.match(/\/dists\/([^/]+)\/InRelease$/);
    if (m && suites.includes(m[1])) {
      return new Response(release(m[1]), { status: 200 });
    }
    return new Response(null, { status: 404 });
  };
  return { fetchImpl, urls };
}

const BASE = "https://up.example/debian/";

describe("AptRepo.resolve", () => {
  it("builds the index from the configured suites (listing fallback)", async () => {
    const { fetchImpl } = mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie", "bogus"], fetchImpl });

    await repo.resolve();

    expect(repo.ready).toBe(true);
    expect(repo.knownPaths.has("dists/trixie/InRelease")).toBe(true);
    expect(repo.knownPaths.has("dists/trixie/main/binary-amd64/Packages")).toBe(true);
    expect(repo.knownHashes.has(HASH_A)).toBe(true);
    expect(repo.validUntil).toBe(Date.parse(VALID_UNTIL));
    // The unreachable suite contributes nothing.
    expect(repo.knownPaths.has("dists/bogus/InRelease")).toBe(false);
  });

  it("stays idle (no enforcement) when nothing resolves", async () => {
    const { fetchImpl } = mockUpstream([]); // every suite 404s
    const repo = new AptRepo({ base: BASE, suites: ["trixie"], fetchImpl });

    await repo.resolve();

    expect(repo.ready).toBe(false);
    expect(repo.knownPaths.size).toBe(0);
  });

  it("ensureResolved runs in the background and flips to ready", async () => {
    const { fetchImpl } = mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie"], fetchImpl });

    const jobs: Promise<unknown>[] = [];
    repo.ensureResolved((p) => jobs.push(p));
    expect(repo.state).toBe("resolving");

    await Promise.all(jobs);
    expect(repo.ready).toBe(true);
  });

  it("normalizes a base without a trailing slash", () => {
    const repo = new AptRepo({ base: "https://up.example/debian", fetchImpl: mockUpstream([]).fetchImpl });
    expect(repo.url("dists/trixie/InRelease")).toBe(`${BASE}dists/trixie/InRelease`);
  });
});
