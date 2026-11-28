import { beforeEach, describe, expect, it, vi } from "vitest";

import { AptRepo } from "./repo";
import { createAptProxy } from "./proxy";

const HASH_A = "a".repeat(64);
const VALID_UNTIL = "Sat, 14 Jun 2026 10:00:00 UTC";

const release = (suite: string) => `Suite: ${suite}
Codename: ${suite}
Valid-Until: ${VALID_UNTIL}
SHA256:
 ${HASH_A}  1234 main/binary-amd64/Packages
`;

// `AptRepo` fetches each suite's Release through `cachedfetch`; mock it.
const { cachedfetchMock, ezfetchMock } = vi.hoisted(() => ({
  cachedfetchMock: vi.fn(),
  ezfetchMock: vi.fn(),
}));
vi.mock("@server/pkgs/fetch", () => ({
  cachedfetch: cachedfetchMock,
  ezfetch: ezfetchMock,
  mdbfetch: vi.fn(),
  sanitizeResponse: (response: Response) => response,
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

beforeEach(() => {
  cachedfetchMock.mockReset();
  ezfetchMock.mockReset();
  ezfetchMock.mockResolvedValue(new Response("upstream"));
});

const BASE = "https://up.example/debian/";

describe("AptRepo facts", () => {
  it("builds a generation from the configured suites", async () => {
    mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie", "bogus"] });

    const facts = await repo.facts();

    expect(repo.ready).toBe(true);
    expect(facts).not.toBeNull();
    expect(facts!.paths.has("dists/trixie/InRelease")).toBe(true);
    expect(facts!.paths.has("dists/trixie/main/binary-amd64/Packages")).toBe(true);
    expect(facts!.hashes.has(HASH_A)).toBe(true);
    expect(facts!.validUntil).toBe(Date.parse(VALID_UNTIL));
    expect(facts!.coverage).toBe("metadata-only");
    // The unreachable suite contributes nothing.
    expect(facts!.paths.has("dists/bogus/InRelease")).toBe(false);
    expect(repo.status()).toBe(facts!.status);
    expect(repo.status().aptstatus?.suites[0]).toMatchObject({ suite: "trixie" });
  });

  it("returns no facts and initializing status when nothing resolves", async () => {
    mockUpstream([]); // every suite 404s
    const repo = new AptRepo({ base: BASE, suites: ["trixie"] });

    await expect(repo.facts()).resolves.toBeNull();

    expect(repo.ready).toBe(false);
    expect(repo.status()).toEqual({ health: "initializing" });
  });

  it("ensureResolved runs in the background and publishes facts", async () => {
    mockUpstream(["trixie"]);
    const repo = new AptRepo({ base: BASE, suites: ["trixie"] });

    const jobs: Promise<unknown>[] = [];
    repo.ensureResolved((p) => jobs.push(p));
    expect(repo.status()).toEqual({ health: "initializing" });

    await Promise.all(jobs);
    expect(repo.ready).toBe(true);
    expect(await repo.facts()).not.toBeNull();
  });

  it("normalizes a base without a trailing slash", () => {
    const repo = new AptRepo({ base: "https://up.example/debian" });
    expect(repo.url("dists/trixie/InRelease")).toBe(`${BASE}dists/trixie/InRelease`);
  });

  it("forwards the first empty segment, later empty segments and escapes upstream", async () => {
    const repo = new AptRepo({ base: "https://up.example/debian" });
    vi.spyOn(repo, "facts").mockResolvedValue(null);
    const proxy = createAptProxy(repo);
    const rel = "/empty//x%2fCase%2F";

    await proxy.fetch(new Request(`https://mirs.uk/debian${rel}`), rel);

    expect(ezfetchMock).toHaveBeenCalledWith(
      `${BASE}/empty//x%2fCase%2F`,
      undefined,
    );
  });
});
