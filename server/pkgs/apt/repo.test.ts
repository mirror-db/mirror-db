import { beforeEach, describe, expect, it, vi } from "vitest";

import { AptRepo } from "./repo";

const HASH_A = "a".repeat(64);
const VALID_UNTIL = "Sat, 14 Jun 2026 10:00:00 UTC";

const release = (suite: string) => `Suite: ${suite}
Codename: ${suite}
Valid-Until: ${VALID_UNTIL}
SHA256:
 ${HASH_A}  1234 main/binary-amd64/Packages
`;

// `AptRepo` fetches each suite's Release through `cachedfetch`; mock it.
const { cachedfetchMock } = vi.hoisted(() => ({ cachedfetchMock: vi.fn() }));
vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: cachedfetchMock,
  mdbfetch: vi.fn(),
}));

/** Serve InRelease for the given suites; everything else 404s. */
function mockUpstream(suites: string[]): { urls: string[] } {
  const urls: string[] = [];
  cachedfetchMock.mockImplementation(async (input: unknown) => {
    const url = String(input);
    urls.push(url);
    const m = url.match(/\/dists\/([^/]+)\/InRelease$/);
    if (m && suites.includes(m[1])) {
      return new Response(release(m[1]), { status: 200 });
    }
    return new Response(null, { status: 404 });
  });
  return { urls };
}

beforeEach(() => cachedfetchMock.mockReset());

const BASE = "https://up.example/debian/";

describe("AptRepo.resolve", () => {
  it("builds the index from the configured suites", async () => {
    mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie", "bogus"] });

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
    mockUpstream([]); // every suite 404s
    const repo = new AptRepo({ base: BASE, suites: ["trixie"] });

    await repo.resolve();

    expect(repo.ready).toBe(false);
    expect(repo.knownPaths.size).toBe(0);
  });

  it("ensureResolved runs in the background and flips to ready", async () => {
    mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie"] });

    const jobs: Promise<unknown>[] = [];
    repo.ensureResolved((p) => jobs.push(p));
    expect(repo.state).toBe("resolving");

    await Promise.all(jobs);
    expect(repo.ready).toBe(true);
  });

  it("normalizes a base without a trailing slash", () => {
    const repo = new AptRepo({ base: "https://up.example/debian" });
    expect(repo.url("dists/trixie/InRelease")).toBe(`${BASE}dists/trixie/InRelease`);
  });
});
