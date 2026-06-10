import { describe, expect, it } from "vitest";

import { parseRelease } from "./release";

const HASH_A = "a".repeat(64);
const HASH_B = "b".repeat(64);

const PLAIN_RELEASE = `Origin: Debian
Label: Debian
Suite: stable
Codename: trixie
Date: Sat, 07 Jun 2026 10:00:00 UTC
Valid-Until: Sat, 14 Jun 2026 10:00:00 UTC
Architectures: amd64 arm64
Components: main contrib
MD5Sum:
 ${"f".repeat(32)}     1234 main/binary-amd64/Packages
SHA256:
 ${HASH_A}     1234 main/binary-amd64/Packages
 ${HASH_B}    56789 main/binary-amd64/Packages.gz
SHA512:
 ${"c".repeat(128)}  1234 main/binary-amd64/Packages
`;

const IN_RELEASE = `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA512

${PLAIN_RELEASE}-----BEGIN PGP SIGNATURE-----

iQIzBAEBCgAdFiEE...signature-bytes...
-----END PGP SIGNATURE-----
`;

describe("parseRelease", () => {
  it("parses suite metadata", () => {
    const idx = parseRelease(PLAIN_RELEASE);
    expect(idx.suite).toBe("stable");
    expect(idx.codename).toBe("trixie");
    expect(idx.validUntil).toBe(Date.parse("Sat, 14 Jun 2026 10:00:00 UTC"));
  });

  it("collects every checksum block (MD5Sum, SHA256, SHA512)", () => {
    const idx = parseRelease(PLAIN_RELEASE);
    expect(idx.files).toEqual([
      { algo: "MD5Sum", hash: "f".repeat(32), size: 1234, path: "main/binary-amd64/Packages" },
      { algo: "SHA256", hash: HASH_A, size: 1234, path: "main/binary-amd64/Packages" },
      { algo: "SHA256", hash: HASH_B, size: 56789, path: "main/binary-amd64/Packages.gz" },
      { algo: "SHA512", hash: "c".repeat(128), size: 1234, path: "main/binary-amd64/Packages" },
    ]);
  });

  it("strips the inline PGP signature from an InRelease", () => {
    const idx = parseRelease(IN_RELEASE);
    expect(idx.suite).toBe("stable");
    expect(idx.files).toHaveLength(4);
    expect(idx.files.some((f) => f.hash === HASH_A)).toBe(true);
  });

  it("returns null validUntil when the field is absent", () => {
    const idx = parseRelease(`Suite: testing\nSHA256:\n ${HASH_A} 10 x\n`);
    expect(idx.validUntil).toBeNull();
    expect(idx.files).toEqual([{ algo: "SHA256", hash: HASH_A, size: 10, path: "x" }]);
  });
});
