import { describe, expect, it } from "vitest";

import { WebListFs } from "@server/pkgs/web-list/web-list";

import {
  AptConflictError,
  AptGenerationError,
  AptLimitError,
  aptIndexResponse,
  digestHex,
} from "./metadata";
import { AptSnapshotIndex, SnapshotListSource } from "./snapshot";

const BASE = "https://up.example/repo/";

interface Route {
  body: Uint8Array | string;
  status?: number;
}

/** Minimal upstream: a URL → body map that records every request. */
class Upstream {
  private readonly routes = new Map<string, Route>();
  readonly requests: string[] = [];
  readonly fetch = async (input: string | URL | Request): Promise<Response> => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    this.requests.push(url);
    const route = this.routes.get(url);
    if (!route) return new Response(null, { status: 404 });
    return new Response(route.body, { status: route.status ?? 200 });
  };

  add(path: string, body: Uint8Array | string, status?: number): void {
    this.routes.set(`${BASE}${path}`, { body, status });
  }

  reads(path: string): boolean {
    return this.requests.some((url) => url === `${BASE}${path}`);
  }
}

async function gzip(text: string): Promise<Uint8Array> {
  const body = new Response(new TextEncoder().encode(text)).body!;
  const compressed = body.pipeThrough(new CompressionStream("gzip"));
  return new Uint8Array(await new Response(compressed).arrayBuffer());
}

interface FileSpec {
  name: string;
  bytes: Uint8Array;
}

interface ReleaseOptions {
  validUntil?: string;
  acquireByHash?: boolean;
}

/** Build a `Release` describing `files` with real size + hashes. */
async function releaseText(
  suite: string,
  files: FileSpec[],
  opts: ReleaseOptions = {},
): Promise<string> {
  const sha256: string[] = [];
  const sha1: string[] = [];
  for (const file of files) {
    sha256.push(
      ` ${await digestHex("SHA-256", file.bytes)} ${file.bytes.byteLength} ${file.name}`,
    );
    sha1.push(` ${await digestHex("SHA-1", file.bytes)} ${file.bytes.byteLength} ${file.name}`);
  }
  return [
    `Suite: ${suite}`,
    `Codename: ${suite}`,
    "Date: Sat, 21 Nov 2026 11:15:03 UTC",
    "Architectures: amd64 arm64",
    "Components: main",
    ...(opts.acquireByHash ? ["Acquire-By-Hash: yes"] : []),
    ...(opts.validUntil ? [`Valid-Until: ${opts.validUntil}`] : []),
    "SHA256:",
    ...sha256,
    "SHA1:",
    ...sha1,
    "",
  ].join("\n");
}

/** Register a suite's release plus every file it references. */
async function registerSuite(
  up: Upstream,
  suite: string,
  files: FileSpec[],
  opts: ReleaseOptions = {},
): Promise<void> {
  up.add(`dists/${suite}/InRelease`, await releaseText(suite, files, opts));
  for (const file of files) {
    up.add(`dists/${suite}/${file.name}`, file.bytes);
    if (opts.acquireByHash) {
      const slash = file.name.lastIndexOf("/");
      const dir = slash === -1 ? "" : file.name.slice(0, slash + 1);
      const hash = await digestHex("SHA-256", file.bytes);
      up.add(`dists/${suite}/${dir}by-hash/SHA256/${hash}`, file.bytes);
    }
  }
}

function makeIndex(
  up: Upstream,
  suites: string[],
  limits?: ConstructorParameters<typeof AptSnapshotIndex>[0]["limits"],
): AptSnapshotIndex {
  return new AptSnapshotIndex({
    mirror: "test",
    base: BASE,
    suites,
    fetchImpl: up.fetch,
    limits,
  });
}

const packagesText = (...entries: string[]): string => entries.map((e) => `${e}\n\n`).join("");

describe("AptSnapshotIndex — index selection", () => {
  it("prefers Packages.gz and never downloads the plain copy", async () => {
    const up = new Upstream();
    const plain = packagesText(
      "Package: nginx\nFilename: pool/main/n/nginx/nginx_1.0_amd64.deb\nSize: 10\n" +
        "SHA256: " + "1".repeat(64),
    );
    const gz = await gzip(plain);
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
      { name: "main/binary-amd64/Packages.gz", bytes: gz },
    ]);

    const index = makeIndex(up, ["bookworm"]);
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages.gz")).toBe(true);
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages")).toBe(false);
    expect(snapshot!.files.find((f) => f.path === "pool/main/n/nginx/nginx_1.0_amd64.deb"))
      .toMatchObject({ size: 10, kind: "package" });
  });

  it("falls back to the plain index when .gz is unreachable", async () => {
    const up = new Upstream();
    const plain = packagesText(
      "Package: nginx\nFilename: pool/nginx.deb\nSize: 5\n",
    );
    const files: FileSpec[] = [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
      { name: "main/binary-amd64/Packages.gz", bytes: await gzip(plain) },
    ];
    up.add("dists/bookworm/InRelease", await releaseText("bookworm", files));
    up.add("dists/bookworm/main/binary-amd64/Packages", new TextEncoder().encode(plain));
    // The .gz route is deliberately absent.

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages.gz")).toBe(true);
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages")).toBe(true);
    expect(snapshot!.files.some((f) => f.path === "pool/nginx.deb")).toBe(true);
  });

  it("prefers by-hash and verifies the body against the release", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    await registerSuite(
      up,
      "bookworm",
      [{ name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) }],
      { acquireByHash: true },
    );

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(
      up.requests.some((url) =>
        url.includes("dists/bookworm/main/binary-amd64/by-hash/SHA256/"),
      ),
    ).toBe(true);
    // The mutable path was never needed.
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages")).toBe(false);
  });

  it("parses identical index content only once across suites", async () => {
    const up = new Upstream();
    const plain = packagesText(
      "Package: shared\nFilename: pool/shared.deb\nSize: 4\nSHA256: " + "7".repeat(64),
    );
    const bytes = await gzip(plain);
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages.gz", bytes },
    ]);
    await registerSuite(up, "trixie", [
      { name: "main/binary-amd64/Packages.gz", bytes },
    ]);

    const index = makeIndex(up, ["bookworm", "trixie"]);
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    // One download/parse for two identical files.
    expect(
      up.requests.filter((url) => url.endsWith("main/binary-amd64/Packages.gz")).length,
    ).toBe(1);
    expect(index.lastStats).toMatchObject({ planned: 2, parsed: 1, deduped: 1 });
    expect(index.lastStats!.parsedBytes).toBe(new TextEncoder().encode(plain).byteLength);
    // Every Release-named metadata path is still in the table.
    expect(
      snapshot!.files.filter((f) => f.path.endsWith("main/binary-amd64/Packages.gz")),
    ).toHaveLength(2);
    // The package itself appears once.
    expect(snapshot!.files.filter((f) => f.path === "pool/shared.deb")).toHaveLength(1);
  });

  it("dedupes identical content under two components of one suite", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: c\nFilename: pool/c.deb\nSize: 2\n");
    const bytes = await gzip(plain);
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages.gz", bytes },
      { name: "test/binary-amd64/Packages.gz", bytes },
    ]);

    const index = makeIndex(up, ["bookworm"]);
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(index.lastStats).toMatchObject({ planned: 2, parsed: 1, deduped: 1 });
  });

  it("rejects one digest declared with two different sizes", async () => {
    const up = new Upstream();
    const digest = "9".repeat(64);
    up.add(
      "dists/bookworm/InRelease",
      [
        "Suite: bookworm",
        "Codename: bookworm",
        "Architectures: amd64 arm64",
        "Components: main",
        "SHA256:",
        ` ${digest} 10 main/binary-amd64/Packages.gz`,
        ` ${digest} 11 main/binary-arm64/Packages.gz`,
        "",
      ].join("\n"),
    );

    await expect(makeIndex(up, ["bookworm"]).build()).rejects.toThrow(AptConflictError);
  });

  it("ignores indexes it is not required to parse", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
      { name: "main/binary-amd64/Translation-en", bytes: new TextEncoder().encode("x") },
      { name: "Contents-amd64", bytes: new TextEncoder().encode("y") },
      { name: "main/dep11/Components-amd64.yml", bytes: new TextEncoder().encode("z") },
    ]);

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(up.reads("dists/bookworm/main/binary-amd64/Translation-en")).toBe(false);
    expect(up.reads("dists/bookworm/Contents-amd64")).toBe(false);
    expect(up.reads("dists/bookworm/main/dep11/Components-amd64.yml")).toBe(false);
    // But they are registered as metadata files.
    expect(snapshot!.files.find((f) => f.path === "dists/bookworm/Contents-amd64")?.kind)
      .toBe("metadata");
  });
});

describe("AptSnapshotIndex — verification and atomicity", () => {
  it("rejects the generation on a hash mismatch and keeps the previous snapshot", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    const path = "dists/bookworm/main/binary-amd64/Packages";
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    const index = makeIndex(up, ["bookworm"]);
    const first = await index.awaitSnapshot();
    expect(first).not.toBeNull();

    // Same declared hash, different bytes: verification must fail.
    up.add(path, new TextEncoder().encode(plain.replace("pool/a.deb", "pool/b.deb")));
    (index.snapshot as unknown as { expiresAt: number }).expiresAt = 0;
    (index as unknown as { lastAttempt: number }).lastAttempt = 0;

    const second = await index.awaitSnapshot();

    expect(second).toBe(first);
    expect(index.state).toBe("ready");
    expect(index.lastError).toMatch(/size mismatch|sha256 mismatch/);
  });

  it("rejects a generation when no release resolves", async () => {
    const index = makeIndex(new Upstream(), ["bookworm"]);
    await expect(index.build()).rejects.toThrow(/no suite release resolved/);
  });

  it("rejects xz-only indexes", async () => {
    const up = new Upstream();
    const bytes = new Uint8Array([1, 2, 3]);
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages.xz", bytes },
    ]);

    await expect(makeIndex(up, ["bookworm"]).build()).rejects.toThrow(
      /unsupported index compression/,
    );
  });

  it("rejects a package path that escapes the repository", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: evil\nFilename: ../../etc/passwd\nSize: 1\n");
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    await expect(makeIndex(up, ["bookworm"]).build()).rejects.toThrow(AptGenerationError);
  });

  it("rejects a size conflict for one path across suites", async () => {
    const up = new Upstream();
    for (const [suite, size] of [["bookworm", 10], ["trixie", 11]] as const) {
      const plain = packagesText(`Package: a\nFilename: pool/shared.deb\nSize: ${size}\n`);
      await registerSuite(up, suite, [
        { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
      ]);
    }

    await expect(makeIndex(up, ["bookworm", "trixie"]).build()).rejects.toThrow(
      AptConflictError,
    );
  });
});

describe("AptSnapshotIndex — limits", () => {
  it("rejects an index above the per-file decompressed cap", async () => {
    const up = new Upstream();
    const plain = packagesText(
      `Package: big\nDescription: ${"x".repeat(400)}\nFilename: pool/big.deb\nSize: 1\n`,
    );
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    await expect(
      makeIndex(up, ["bookworm"], { maxFileBytes: 100 }).build(),
    ).rejects.toThrow(AptLimitError);
  });

  it("rejects a generation above the cumulative decompressed cap", async () => {
    const up = new Upstream();
    const make = async (name: string) =>
      packagesText(`Package: ${name}\nFilename: pool/${name}.deb\nSize: 1\n`);
    const first = new TextEncoder().encode(await make("a"));
    const second = new TextEncoder().encode(await make("b"));
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: first },
      { name: "main/binary-arm64/Packages", bytes: second },
    ]);

    const perFile = Math.max(first.byteLength, second.byteLength) + 1;
    await expect(
      makeIndex(up, ["bookworm"], {
        maxFileBytes: perFile,
        maxTotalBytes: first.byteLength + 1,
      }).build(),
    ).rejects.toThrow(AptLimitError);
  });

  it("rejects a generation above the file-count cap", async () => {
    const up = new Upstream();
    const plain = packagesText(
      "Package: a\nFilename: pool/a.deb\nSize: 1\n",
      "Package: b\nFilename: pool/b.deb\nSize: 1\n",
      "Package: c\nFilename: pool/c.deb\nSize: 1\n",
    );
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    await expect(makeIndex(up, ["bookworm"], { maxFiles: 2 }).build()).rejects.toThrow(
      AptLimitError,
    );
  });

  it("charges identical content once against the generation cap", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    const bytes = new TextEncoder().encode(plain);
    await registerSuite(up, "a", [{ name: "main/binary-amd64/Packages", bytes }]);
    await registerSuite(up, "b", [{ name: "main/binary-amd64/Packages", bytes }]);

    const index = makeIndex(up, ["a", "b"], {
      maxFileBytes: bytes.byteLength + 10,
      // Below 2x the index size: only a deduplicated generation fits.
      maxTotalBytes: bytes.byteLength + 1,
    });
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(index.lastStats).toMatchObject({ planned: 2, parsed: 1, deduped: 1 });
  });

  it("still rejects distinct indexes above the generation cap", async () => {
    const up = new Upstream();
    const one = new TextEncoder().encode(
      packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n"),
    );
    const two = new TextEncoder().encode(
      packagesText("Package: b\nFilename: pool/b.deb\nSize: 2\n"),
    );
    await registerSuite(up, "a", [{ name: "main/binary-amd64/Packages", bytes: one }]);
    await registerSuite(up, "b", [{ name: "main/binary-amd64/Packages", bytes: two }]);

    await expect(
      makeIndex(up, ["a", "b"], {
        maxFileBytes: Math.max(one.byteLength, two.byteLength) + 10,
        maxTotalBytes: one.byteLength + 1,
      }).build(),
    ).rejects.toThrow(AptLimitError);
  });
});

describe("AptSnapshotIndex — content", () => {
  it("accepts a zero-byte Packages index as an empty generation", async () => {
    const up = new Upstream();
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages.gz", bytes: await gzip("") },
    ]);

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(snapshot!.suites.map((s) => s.suite)).toEqual(["bookworm"]);
    expect(snapshot!.files.filter((f) => f.kind === "package")).toEqual([]);
    expect(snapshot!.files.some((f) => f.path.endsWith("Packages.gz"))).toBe(true);
  });

  it("accepts a literally empty .gz file as an empty index", async () => {
    const up = new Upstream();
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages.gz", bytes: new Uint8Array(0) },
    ]);

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(snapshot!.files.filter((f) => f.kind === "package")).toEqual([]);
  });

  it("parses Sources: Directory + Files/Checksums become package files", async () => {
    const up = new Upstream();
    const sources = [
      "Package: nginx",
      "Directory: pool/main/n/nginx",
      "Files:",
      ` ${"1".repeat(32)} 11 nginx_1.0.dsc`,
      "Checksums-Sha1:",
      ` ${"a".repeat(40)} 11 nginx_1.0.dsc`,
      "Checksums-Sha256:",
      ` ${"b".repeat(64)} 11 nginx_1.0.dsc`,
      "",
      "",
    ].join("\n");
    await registerSuite(up, "bookworm", [
      { name: "main/source/Sources.gz", bytes: await gzip(sources) },
    ]);

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot!.files.find((f) => f.path === "pool/main/n/nginx/nginx_1.0.dsc"))
      .toEqual({
        path: "pool/main/n/nginx/nginx_1.0.dsc",
        size: 11,
        sha1: "a".repeat(40),
        sha256: "b".repeat(64),
        kind: "package",
      });
  });

  it("dedupes a shared package path and fills missing hashes", async () => {
    const up = new Upstream();
    await registerSuite(up, "bookworm", [
      {
        name: "main/binary-amd64/Packages",
        bytes: new TextEncoder().encode(
          packagesText(
            `Package: shared\nFilename: pool/shared.deb\nSize: 7\nSHA256: ${"1".repeat(64)}\n`,
          ),
        ),
      },
    ]);
    await registerSuite(up, "trixie", [
      {
        name: "main/binary-amd64/Packages",
        bytes: new TextEncoder().encode(
          packagesText(
            `Package: shared\nFilename: pool/shared.deb\nSize: 7\nSHA1: ${"2".repeat(40)}\n`,
          ),
        ),
      },
    ]);

    const snapshot = await makeIndex(up, ["bookworm", "trixie"]).awaitSnapshot();
    const entries = snapshot!.files.filter((f) => f.path === "pool/shared.deb");

    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({
      size: 7,
      sha256: "1".repeat(64),
      sha1: "2".repeat(40),
    });
  });

  it("takes the earliest Valid-Until across suites", async () => {
    const up = new Upstream();
    const plain = new TextEncoder().encode("");
    const later = "Tue, 14 Jun 2030 10:00:00 UTC";
    const earlier = "Fri, 14 Jun 2030 10:00:00 UTC";
    await registerSuite(up, "a", [{ name: "main/binary-amd64/Packages", bytes: plain }], {
      validUntil: later,
    });
    await registerSuite(up, "b", [{ name: "main/binary-amd64/Packages", bytes: plain }], {
      validUntil: earlier,
    });

    const snapshot = await makeIndex(up, ["a", "b"]).awaitSnapshot();
    expect(snapshot!.expiresAt).toBe(Date.parse(earlier));
  });

  it("defaults to one hour after generation when no suite has Valid-Until", async () => {
    const up = new Upstream();
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode("") },
    ]);

    const before = Date.now();
    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();
    const after = Date.now();

    expect(snapshot!.expiresAt - snapshot!.generatedAt.getTime()).toBe(3_600_000);
    expect(snapshot!.expiresAt).toBeGreaterThanOrEqual(before + 3_600_000 - 1_000);
    expect(snapshot!.expiresAt).toBeLessThanOrEqual(after + 3_600_000 + 1_000);
  });

  it("synthesises directories from the generation and 404s unknown ones", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/main/a/a.deb\nSize: 3\n");
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    const snapshot = (await makeIndex(up, ["bookworm"]).awaitSnapshot())!;

    expect(snapshot.readdir("").map((e) => e.name)).toEqual(["dists", "pool"]);
    expect(snapshot.readdir("dists/bookworm/").map((e) => e.name)).toContain("InRelease");
    expect(snapshot.readdir("pool/main/a/")).toEqual([
      expect.objectContaining({ name: "a.deb", size: 3, type: "file" }),
    ]);
    expect(() => snapshot.readdir("pool/nope/")).toThrow();
  });
});

describe("AptSnapshotIndex — release validation", () => {
  const goodPackages = () =>
    new TextEncoder().encode(packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n"));

  it("rejects an HTML body served with HTTP 200", async () => {
    const up = new Upstream();
    const bytes = goodPackages();
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes },
    ]);
    up.add("dists/bookworm/InRelease", "<html><body>captive portal</body></html>");

    const index = makeIndex(up, ["bookworm"]);
    await expect(index.build()).rejects.toThrow(/dists\/bookworm\/InRelease/);
    expect(index.snapshot).toBeNull();
  });

  it("rejects an empty release body and keeps the previous generation", async () => {
    const up = new Upstream();
    const bytes = goodPackages();
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes },
    ]);

    const index = makeIndex(up, ["bookworm"]);
    const first = await index.awaitSnapshot();
    expect(first).not.toBeNull();

    up.add("dists/bookworm/InRelease", "");
    (index.snapshot as unknown as { expiresAt: number }).expiresAt = 0;
    (index as unknown as { lastAttempt: number }).lastAttempt = 0;

    const second = await index.awaitSnapshot();
    expect(second).toBe(first);
    expect(index.lastError).toMatch(/is empty/);
  });

  it("rejects a release without a checksum block", async () => {
    const up = new Upstream();
    up.add(
      "dists/bookworm/InRelease",
      ["Suite: bookworm", "Codename: bookworm", "Architectures: amd64", ""].join("\n"),
    );

    await expect(makeIndex(up, ["bookworm"]).build()).rejects.toThrow(
      /declares no SHA256\/SHA1 checksums/,
    );
  });

  it("rejects a release whose declared digest is malformed", async () => {
    const up = new Upstream();
    up.add(
      "dists/bookworm/InRelease",
      [
        "Suite: bookworm",
        "Codename: bookworm",
        "SHA256:",
        " not-a-digest 100 main/binary-amd64/Packages",
        "",
      ].join("\n"),
    );

    await expect(makeIndex(up, ["bookworm"]).build()).rejects.toThrow(
      /unusable sha256 digest/,
    );
  });
});

describe("AptSnapshotIndex — transient versus absent", () => {
  it("skips a suite only when both release names are definitely absent", async () => {
    const up = new Upstream();
    const bytes = new TextEncoder().encode(
      packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n"),
    );
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes },
    ]);

    const index = makeIndex(up, ["gone", "bookworm"]);
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(snapshot!.suites.map((s) => s.suite)).toEqual(["bookworm"]);
  });

  it("rejects the whole generation on a 5xx from one suite", async () => {
    const up = new Upstream();
    const bytes = new TextEncoder().encode(
      packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n"),
    );
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes },
    ]);
    up.add("dists/broken/InRelease", "oops", 503);
    up.add("dists/broken/Release", "oops", 502);

    const index = makeIndex(up, ["bookworm", "broken"]);
    await expect(index.build()).rejects.toThrow(/answered HTTP 50/);
    // The healthy suite's work is discarded with the generation.
    await expect(index.awaitSnapshot()).resolves.toBeNull();
    expect(index.state).toBe("idle");
  });

  it("rejects the whole generation on a network error", async () => {
    const up = new Upstream();
    const bytes = new TextEncoder().encode(
      packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n"),
    );
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes },
    ]);

    const index = new AptSnapshotIndex({
      mirror: "test",
      base: BASE,
      suites: ["bookworm", "unreachable"],
      fetchImpl: async (input) => {
        const url = typeof input === "string" ? input : (input as Request).url;
        if (url.includes("dists/unreachable/")) throw new Error("ECONNRESET");
        return up.fetch(input);
      },
    });

    await expect(index.build()).rejects.toThrow(/fetch failed: ECONNRESET/);
    await expect(index.awaitSnapshot()).resolves.toBeNull();
  });
});

describe("AptSnapshotIndex — concurrency bound", () => {
  it("keeps at most four index groups in flight", async () => {
    const up = new Upstream();
    const files: FileSpec[] = [];
    for (let i = 0; i < 12; i += 1) {
      files.push({
        name: `component${i}/Packages`,
        bytes: new TextEncoder().encode(
          packagesText(`Package: p${i}\nFilename: pool/p${i}.deb\nSize: 1\n`),
        ),
      });
    }
    await registerSuite(up, "bookworm", files);

    let active = 0;
    let peak = 0;
    const index = new AptSnapshotIndex({
      mirror: "test",
      base: BASE,
      suites: ["bookworm"],
      fetchImpl: async (input) => {
        const url = typeof input === "string" ? input : (input as Request).url;
        const group = url.includes("Packages");
        if (group) {
          active += 1;
          peak = Math.max(peak, active);
        }
        try {
          await new Promise((resolve) => setTimeout(resolve, 5));
          return await up.fetch(input);
        } finally {
          if (group) active -= 1;
        }
      },
    });

    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(peak).toBeLessThanOrEqual(4);
    expect(index.lastStats).toMatchObject({ planned: 12, parsed: 12, deduped: 0 });
  });
});

describe("AptSnapshotIndex — stable validator", () => {
  it("keeps the ETag across rebuilds of unchanged content and 304s", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
    ]);

    let now = Date.parse("2026-01-01T00:00:00Z");
    const index = new AptSnapshotIndex({
      mirror: "test",
      base: BASE,
      suites: ["bookworm"],
      fetchImpl: up.fetch,
      now: () => now,
    });

    const first = await index.build();
    now += 3_600_000;
    const second = await index.build();

    // The body carries the new times, the validator does not move.
    expect(second.generatedAt.getTime()).toBe(now);
    expect(second.body).not.toBe(first.body);
    expect(second.etag).toBe(first.etag);

    const url = "https://mirs.uk/hashicorp/.mdb/apt-index.json";
    const cached = aptIndexResponse(
      second,
      new Request(url, { headers: { "if-none-match": first.etag } }),
    );
    expect(cached.status).toBe(304);
    expect(cached.headers.get("etag")).toBe(first.etag);
    expect(cached.headers.get("cache-control")).toMatch(/max-age=\d+/);

    // Different upstream content → a different validator and a 200.
    const changed = packagesText("Package: b\nFilename: pool/b.deb\nSize: 2\n");
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(changed) },
    ]);
    const third = await index.build();
    expect(third.etag).not.toBe(first.etag);

    const stale = aptIndexResponse(
      third,
      new Request(url, { headers: { "if-none-match": first.etag } }),
    );
    expect(stale.status).toBe(200);
  });
});

describe("AptSnapshotIndex — index resilience", () => {
  it("falls back to the plain copy when the .gz matches its hash but does not decode", async () => {
    const up = new Upstream();
    const plain = packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n");
    const notGzip = new Uint8Array([0x1f, 0x8b, 0xde, 0xad, 0xbe, 0xef]);
    await registerSuite(up, "bookworm", [
      { name: "main/binary-amd64/Packages", bytes: new TextEncoder().encode(plain) },
      { name: "main/binary-amd64/Packages.gz", bytes: notGzip },
    ]);

    const snapshot = await makeIndex(up, ["bookworm"]).awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(snapshot!.files.some((f) => f.path === "pool/a.deb")).toBe(true);
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages.gz")).toBe(true);
    expect(up.reads("dists/bookworm/main/binary-amd64/Packages")).toBe(true);
  });

  it("lists only the release it actually fetched", async () => {
    const up = new Upstream();
    await registerSuite(up, "bookworm", [
      {
        name: "main/binary-amd64/Packages",
        bytes: new TextEncoder().encode(packagesText("Package: a\nFilename: pool/a.deb\nSize: 1\n")),
      },
    ]);

    const snapshot = (await makeIndex(up, ["bookworm"]).awaitSnapshot())!;
    const names = snapshot.readdir("dists/bookworm/").map((e) => e.name);

    expect(names).toEqual(["main", "InRelease"]);
    expect(names).not.toContain("Release.gpg");
    expect(names).not.toContain("Release");
  });
});

describe("SnapshotListSource", () => {
  it("answers 502, not an empty listing, when no generation was ever built", async () => {
    const up = new Upstream();
    const source = new SnapshotListSource(
      makeIndex(up, ["bookworm"]),
      new WebListFs(BASE, { fetchImpl: up.fetch }),
    );

    await expect(source.readdir("")).rejects.toMatchObject({ status: 502 });
  });
});
