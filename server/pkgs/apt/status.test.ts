import { describe, expect, it } from "vitest";
import { Release } from "apt-parser";

import { CollectionCounter, analyzeApt, toSuiteMeta } from "./status";

const HASH_A = "a".repeat(64);
const HASH_B = "b".repeat(64);

const release = (suite: string, hash: string, size: number) =>
  new Release(
    `Suite: ${suite}
Codename: ${suite}
Architectures: amd64
Components: main
SHA256:
 ${hash} ${size} main/binary-amd64/Packages
`,
    { skipValidation: true },
  );

describe("toSuiteMeta", () => {
  it("keeps metadata but drops the checksum blocks and raw map", () => {
    const meta = toSuiteMeta(release("trixie", HASH_A, 1234));

    expect(meta.suite).toBe("trixie");
    expect(meta.codename).toBe("trixie");

    // The bulky file listing must not survive — neither as parsed hash arrays
    // nor via the inherited raw key-value map.
    const bag = meta as unknown as Record<string, unknown>;
    expect(bag.sha256).toBeUndefined();
    expect(bag.md5).toBeUndefined();
    expect(bag.raw).toBeUndefined();
    expect(bag.required).toBeUndefined();

    // And it must not re-serialize the hash anywhere in the payload.
    expect(JSON.stringify(meta)).not.toContain(HASH_A);
  });
});

describe("CollectionCounter.merge", () => {
  it("folds in another counter without double-counting files/size", () => {
    const a = new CollectionCounter([[HASH_A, 100]]);
    const b = new CollectionCounter([[HASH_B, 200]]);
    a.merge(b);

    const stats = a.export();
    expect(stats.files).toBe(2);
    expect(stats.size).toBe(300);
    expect(stats.deduplicatedFiles).toBe(2);
    expect(stats.deduplicatedSize).toBe(300);
  });

  it("counts shared content once in deduplicatedFiles across suites", () => {
    const stats = analyzeApt([
      release("a", HASH_A, 100),
      release("b", HASH_A, 100), // same hash recurs in a second suite
    ]).stats;

    // `files` counts every entry; `deduplicatedFiles` collapses the shared hash.
    expect(stats.files).toBe(2);
    expect(stats.deduplicatedFiles).toBe(1);
  });
});
