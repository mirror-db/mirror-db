import { describe, expect, it } from "vitest";

import {
  AptConflictError,
  AptFileTable,
  AptGenerationError,
  AptLimitError,
  aptIndexEtag,
  aptIndexResponse,
  AptSnapshot,
  buildAptIndexDocument,
  buildAptIndexStable,
  digestHex,
  normalizeRepoPath,
  packagesConsumer,
  parseIndexStream,
  resolveLimits,
  sourcesConsumer,
  type AptSnapshotFile,
  type AptSnapshotSuite,
} from "./metadata";
import type { Stanza } from "./stanza";

const SHA1_A = "a".repeat(40);
const SHA1_B = "b".repeat(40);
const SHA256_A = "c".repeat(64);
const SHA256_B = "d".repeat(64);

function makeTable(limits = resolveLimits()) {
  return new AptFileTable(limits);
}

/** Run the real streaming decoder over plain text and collect stanzas. */
async function stanzas(text: string): Promise<Stanza[]> {
  const out: Stanza[] = [];
  await parseIndexStream(
    new TextEncoder().encode(text),
    false,
    resolveLimits(),
    () => {},
    (stanza) => out.push(stanza),
  );
  return out;
}

describe("normalizeRepoPath", () => {
  it("keeps ordinary repo-relative paths", () => {
    expect(normalizeRepoPath("pool/main/n/nginx/nginx_1.0_amd64.deb")).toBe(
      "pool/main/n/nginx/nginx_1.0_amd64.deb",
    );
    expect(normalizeRepoPath("dists/bookworm/Release")).toBe("dists/bookworm/Release");
  });

  it.each([
    ["/etc/passwd"],
    ["../escape"],
    ["a/../b"],
    ["a/./b"],
    ["a//b"],
    ["a/"],
    ["a\\b"],
    ["https://evil.example/x"],
    ["a\u0000b"],
    [""],
    ["   "],
    ["pool/x?y"],
    ["pool/x#y"],
    ["a%2fb"],
    ["a%5cb"],
    ["%2e%2e%2fescape"],
    ["a/%2e/b"],
  ])("rejects %j", (path) => {
    expect(() => normalizeRepoPath(path)).toThrow(AptGenerationError);
  });

  it("keeps ordinary percent escapes that decode to a plain name", () => {
    expect(normalizeRepoPath("pool/a%20b.deb")).toBe("pool/a%20b.deb");
  });
});

describe("AptFileTable", () => {
  it("merges missing hashes for the same path", () => {
    const files = makeTable();
    files.add("pool/x.deb", { size: 10, sha1: SHA1_A }, "package");
    files.add("pool/x.deb", { size: 10, sha256: SHA256_A }, "package");

    const entry = files.get("pool/x.deb")!;
    expect(entry.size).toBe(10);
    expect(entry.sha1).toBe(SHA1_A);
    expect(entry.sha256).toBe(SHA256_A);
  });

  it("rejects a size conflict", () => {
    const files = makeTable();
    files.add("pool/x.deb", { size: 10 }, "package");
    expect(() => files.add("pool/x.deb", { size: 11 }, "package")).toThrow(
      AptConflictError,
    );
  });

  it("rejects a same-algorithm hash conflict", () => {
    const files = makeTable();
    files.add("pool/x.deb", { size: 10, sha256: SHA256_A }, "package");
    expect(() => files.add("pool/x.deb", { size: 10, sha256: SHA256_B }, "package")).toThrow(
      AptConflictError,
    );
    // Different algorithms are not a conflict.
    expect(() => files.add("pool/x.deb", { size: 10, sha1: SHA1_A }, "package")).not.toThrow();
  });

  it("rejects a kind conflict", () => {
    const files = makeTable();
    files.add("dists/bookworm/Release", { size: 1 }, "metadata");
    expect(() => files.add("dists/bookworm/Release", { size: 1 }, "package")).toThrow(
      AptConflictError,
    );
  });

  it("enforces the file-count limit", () => {
    const files = makeTable({ ...resolveLimits(), maxFiles: 2 });
    files.add("a", { size: 1 }, "package");
    files.add("b", { size: 1 }, "package");
    expect(() => files.add("c", { size: 1 }, "package")).toThrow(AptLimitError);
  });
});

describe("packagesConsumer", () => {
  it("turns a Package stanza into a repo-root-relative file entry", async () => {
    const files = makeTable();
    const entries = await stanzas(
      [
        "Package: nginx",
        "Version: 1.0",
        "Filename: pool/main/n/nginx/nginx_1.0_amd64.deb",
        "Size: 1234",
        `SHA256: ${SHA256_A}`,
        `SHA1: ${SHA1_A}`,
        "Description: web server",
        " with folded continuation",
        "",
        "Package: skipped-no-filename",
        "Size: 1",
        "",
        "Package: bad-size",
        "Filename: pool/bad.deb",
        "Size: not-a-number",
        "",
      ].join("\n"),
    );
    for (const stanza of entries) packagesConsumer(files)(stanza);

    expect(files.size).toBe(1);
    expect(files.get("pool/main/n/nginx/nginx_1.0_amd64.deb")).toMatchObject({
      size: 1234,
      sha1: SHA1_A,
      sha256: SHA256_A,
      kind: "package",
    });
  });

  it("rejects a Filename that escapes the repository", async () => {
    const files = makeTable();
    const entries = await stanzas(
      ["Package: evil", "Filename: ../../etc/passwd", "Size: 1", ""].join("\n"),
    );

    expect(() => packagesConsumer(files)(entries[0]!)).toThrow(AptGenerationError);
  });
});

describe("sourcesConsumer", () => {
  it("merges Directory with Files and Checksums-* blocks", async () => {
    const files = makeTable();
    const entries = await stanzas(
      [
        "Package: nginx",
        "Binary: nginx",
        "Directory: pool/main/n/nginx",
        "Files:",
        ` 11111111111111111111111111111111 100 nginx_1.0.dsc`,
        ` 22222222222222222222222222222222 200 nginx_1.0.tar.gz`,
        "Checksums-Sha1:",
        ` ${SHA1_A} 100 nginx_1.0.dsc`,
        ` ${SHA1_B} 200 nginx_1.0.tar.gz`,
        "Checksums-Sha256:",
        ` ${SHA256_A} 100 nginx_1.0.dsc`,
        ` ${SHA256_B} 200 nginx_1.0.tar.gz`,
        "",
      ].join("\n"),
    );
    for (const stanza of entries) sourcesConsumer(files)(stanza);

    expect(files.size).toBe(2);
    expect(files.get("pool/main/n/nginx/nginx_1.0.dsc")).toMatchObject({
      size: 100,
      sha1: SHA1_A,
      sha256: SHA256_A,
      kind: "package",
    });
    expect(files.get("pool/main/n/nginx/nginx_1.0.tar.gz")).toMatchObject({
      size: 200,
      sha1: SHA1_B,
      sha256: SHA256_B,
    });
  });

  it("rejects a conflicting size between blocks", async () => {
    const files = makeTable();
    const entries = await stanzas(
      [
        "Directory: pool/x",
        "Files:",
        ` 11111111111111111111111111111111 100 x.dsc`,
        "Checksums-Sha256:",
        ` ${SHA256_A} 101 x.dsc`,
        "",
      ].join("\n"),
    );

    expect(() => sourcesConsumer(files)(entries[0]!)).toThrow(AptConflictError);
  });

  it("rejects a Directory that escapes the repository", async () => {
    const files = makeTable();
    const entries = await stanzas(
      ["Directory: ../../etc", "Files:", ` 11111111111111111111111111111111 1 passwd`, ""].join(
        "\n",
      ),
    );

    expect(() => sourcesConsumer(files)(entries[0]!)).toThrow(AptGenerationError);
  });

  it("collapses a trailing slash on Directory instead of emitting //", async () => {
    const files = makeTable();
    const entries = await stanzas(
      [
        "Directory: pool/main/n/nginx/",
        "Files:",
        " 11111111111111111111111111111111 100 nginx_1.0.dsc",
        "",
      ].join("\n"),
    );

    expect(() => sourcesConsumer(files)(entries[0]!)).not.toThrow();
    expect(files.get("pool/main/n/nginx/nginx_1.0.dsc")).toMatchObject({ size: 100 });
  });
});

describe("parseIndexStream limits", () => {
  it("rejects an index above the per-file decompressed cap", async () => {
    const text = `Package: big\nDescription: ${"x".repeat(200)}\n\n`;
    await expect(
      parseIndexStream(
        new TextEncoder().encode(text),
        false,
        { ...resolveLimits(), maxFileBytes: 50 },
        () => {},
        () => {},
      ),
    ).rejects.toThrow(AptLimitError);
  });
});

describe("buildAptIndexDocument", () => {
  const generatedAt = new Date("2026-01-01T00:00:00Z");
  const suites: AptSnapshotSuite[] = [
    {
      suite: "bookworm",
      releasePath: "dists/bookworm/InRelease",
      date: new Date("2026-01-01T00:00:00Z"),
      validUntil: new Date("2026-01-08T00:00:00Z"),
      sha256: SHA256_A,
    },
  ];
  const files: AptSnapshotFile[] = [
    { path: "pool/x.deb", size: 5, sha256: SHA256_B, kind: "package" },
    { path: "dists/bookworm/Packages", size: 1, kind: "metadata" },
  ];

  it("omits absent optional fields and sorts files", () => {
    const body = buildAptIndexDocument("m", generatedAt, Date.parse("2026-01-01T01:00:00Z"), suites, files);
    const doc = JSON.parse(body);

    expect(doc).toEqual({
      schema: 1,
      mirror: "m",
      generated_at: "2026-01-01T00:00:00.000Z",
      expires_at: "2026-01-01T01:00:00.000Z",
      suites: [
        {
          suite: "bookworm",
          release_path: "dists/bookworm/InRelease",
          date: "2026-01-01T00:00:00.000Z",
          valid_until: "2026-01-08T00:00:00.000Z",
          sha256: SHA256_A,
        },
      ],
      files: [
        { path: "dists/bookworm/Packages", size: 1, kind: "metadata" },
        { path: "pool/x.deb", size: 5, sha256: SHA256_B, kind: "package" },
      ],
    });
    // Optional sha1 is absent, never `null`.
    expect(body).not.toContain("null");
    expect(doc.files[0]).not.toHaveProperty("sha256");
  });

  it("is deterministic for equal input", () => {
    const a = buildAptIndexDocument("m", generatedAt, 1, suites, files);
    const b = buildAptIndexDocument("m", generatedAt, 1, suites, files);
    expect(a).toBe(b);
  });

  it("excludes the build times from the stable validator input", () => {
    const stable = JSON.parse(buildAptIndexStable("m", suites, files));
    expect(stable).not.toHaveProperty("generated_at");
    expect(stable).not.toHaveProperty("expires_at");
    expect(stable.files).toEqual(JSON.parse(buildAptIndexDocument("m", generatedAt, 1, suites, files)).files);
  });

  it("derives an ETag from metadata only, so a rebuild keeps it", async () => {
    const a = await aptIndexEtag("m", suites, files);
    const b = await aptIndexEtag("m", suites, files);
    const changed = await aptIndexEtag("m", suites, [
      ...files,
      { path: "pool/y.deb", size: 7, kind: "package" },
    ]);

    expect(a).toBe(b);
    expect(a).toMatch(/^"[0-9a-f]{64}"$/);
    expect(changed).not.toBe(a);
  });
});

describe("AptSnapshot directories", () => {
  const snapshot = new AptSnapshot(
    "m",
    new Date(),
    Date.now() + 1000,
    [],
    [
      { path: "dists/bookworm/InRelease", size: 10, kind: "metadata" },
      { path: "dists/bookworm/main/binary-amd64/Packages.gz", size: 20, kind: "metadata" },
      { path: "pool/main/n/nginx/nginx_1.0_amd64.deb", size: 30, kind: "package" },
    ],
    [],
    "{}",
    '"etag"',
  );

  it("synthesises every level from the file paths", () => {
    expect(snapshot.readdir("").map((e) => [e.name, e.type])).toEqual([
      ["dists", "directory"],
      ["pool", "directory"],
    ]);
    expect(snapshot.readdir("dists/").map((e) => e.name)).toEqual(["bookworm"]);
    expect(snapshot.readdir("dists/bookworm/").map((e) => e.name)).toEqual([
      "main",
      "InRelease",
    ]);
    expect(snapshot.readdir("pool/main/n/nginx/")).toEqual([
      expect.objectContaining({ name: "nginx_1.0_amd64.deb", type: "file", size: 30 }),
    ]);
  });

  it("404s a directory that the paths do not define", () => {
    for (const path of ["dists/nonexistent/", "nope"]) {
      try {
        snapshot.readdir(path);
        expect.unreachable("unknown snapshot directories must not be listed");
      } catch (error) {
        expect(error).toMatchObject({ name: "WebListError", status: 404 });
      }
    }
  });
});

describe("AptSnapshot known sets", () => {
  const snapshot = new AptSnapshot(
    "m",
    new Date(),
    Date.now() + 1000,
    [],
    [{ path: "pool/main/a/a.deb", size: 1, kind: "package" }],
    ["dists/bookworm/Release"],
    "{}",
    '"etag"',
    [SHA256_A],
  );

  it("keeps declared paths and by-hash digests for enforcement", () => {
    expect(snapshot.knownPaths.has("pool/main/a/a.deb")).toBe(true);
    expect(snapshot.knownPaths.has("dists/bookworm/Release")).toBe(true);
    expect(snapshot.hasChildren("pool/")).toBe(true);
    expect(snapshot.hasChildren("dists/bookworm/Release/")).toBe(false);
    expect(snapshot.knownHashes.has(SHA256_A)).toBe(true);
  });
});

describe("aptIndexResponse", () => {
  const generatedAt = new Date("2026-01-01T00:00:00Z");
  const expiresAt = Date.now() + 3_600_000;
  const body = buildAptIndexDocument("m", generatedAt, expiresAt, [], []);
  let etag = "";
  const snapshot = () =>
    new AptSnapshot("m", generatedAt, expiresAt, [], [], [], body, etag);

  it("serves GET with strong ETag and freshness", async () => {
    etag = `"${await digestHex("SHA-256", new TextEncoder().encode(body))}"`;
    const res = aptIndexResponse(snapshot(), new Request("https://mirs.uk/m/.mdb/apt-index.json"));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("application/json");
    expect(res.headers.get("etag")).toBe(etag);
    const maxAge = Number(res.headers.get("cache-control")?.match(/max-age=(\d+)/)?.[1]);
    expect(maxAge).toBeGreaterThan(3500);
    expect(await res.text()).toBe(body);
  });

  it("answers HEAD with the same headers and no body", async () => {
    etag = `"${await digestHex("SHA-256", new TextEncoder().encode(body))}"`;
    const res = aptIndexResponse(
      snapshot(),
      new Request("https://mirs.uk/m/.mdb/apt-index.json", { method: "HEAD" }),
    );

    expect(res.status).toBe(200);
    expect(res.headers.get("etag")).toBe(etag);
    expect(res.headers.get("content-length")).toBe(String(new TextEncoder().encode(body).byteLength));
    expect(await res.text()).toBe("");
  });

  it("answers 304 on a matching If-None-Match, including weak form", async () => {
    etag = `"${await digestHex("SHA-256", new TextEncoder().encode(body))}"`;
    for (const value of [etag, `W/${etag}`, "*"]) {
      const res = aptIndexResponse(
        snapshot(),
        new Request("https://mirs.uk/m/.mdb/apt-index.json", {
          headers: { "if-none-match": value },
        }),
      );
      expect(res.status).toBe(304);
      expect(res.headers.get("etag")).toBe(etag);
      // A 304 still needs the freshness window the relay should hold.
      expect(res.headers.get("cache-control")).toMatch(/max-age=\d+/);
    }
  });

  it("serves 200 on a non-matching If-None-Match", async () => {
    etag = `"${await digestHex("SHA-256", new TextEncoder().encode(body))}"`;
    const res = aptIndexResponse(
      snapshot(),
      new Request("https://mirs.uk/m/.mdb/apt-index.json", {
        headers: { "if-none-match": '"other"' },
      }),
    );
    expect(res.status).toBe(200);
  });

  it("rejects other methods", () => {
    const res = aptIndexResponse(
      snapshot(),
      new Request("https://mirs.uk/m/.mdb/apt-index.json", { method: "POST" }),
    );
    expect(res.status).toBe(405);
  });
});
