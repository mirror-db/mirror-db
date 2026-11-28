import { describe, expect, it } from "vitest";

import {
  contentAddressedPathFact,
  declarePathFact,
  immutablePathFact,
} from "./relay-path";

const SHA256 = "a".repeat(64);
const SHA1 = "b".repeat(40);
const relay = (response: Response) => response.headers.get("x-mdb-relay");

describe("relay path facts — response gate", () => {
  it.each([200, 206, 304])("declares a content-addressed fact on %i", (status) => {
    const response = new Response(null, { status });
    const fact = contentAddressedPathFact("sha256", SHA256);

    expect(relay(declarePathFact(response, fact))).toBe(`immutable, sha256=${SHA256}`);
  });

  it.each([201, 204, 302, 400, 404, 500])("declares nothing on %i", (status) => {
    const response = new Response(null, { status });
    const fact = contentAddressedPathFact("sha256", SHA256);

    expect(declarePathFact(response, fact)).toBe(response);
    expect(relay(response)).toBeNull();
  });

  it("returns the original response when there is no path fact", () => {
    const response = new Response(null, { status: 200 });

    expect(declarePathFact(response, null)).toBe(response);
    expect(declarePathFact(response)).toBe(response);
    expect(relay(response)).toBeNull();
  });

  it("declares immutable alone for a publication-pinned path", () => {
    const response = new Response(null, { status: 200 });

    expect(relay(declarePathFact(response, immutablePathFact()))).toBe("immutable");
  });
});

describe("relay path facts — content digests", () => {
  it.each([
    { algorithm: "sha256", hex: SHA256, expected: `sha256=${SHA256}` },
    {
      algorithm: "SHA256",
      hex: SHA256.toUpperCase(),
      expected: `sha256=${SHA256}`,
    },
    { algorithm: "sha1", hex: SHA1, expected: `sha1=${SHA1}` },
    { algorithm: "SHA1", hex: SHA1.toUpperCase(), expected: `sha1=${SHA1}` },
  ])("normalizes $algorithm and declares its exact-length digest", ({ algorithm, hex, expected }) => {
    const response = new Response(null, { status: 200 });
    const fact = contentAddressedPathFact(algorithm, hex);

    expect(relay(declarePathFact(response, fact))).toBe(`immutable, ${expected}`);
  });

  it.each([
    { algorithm: "sha256", hex: "a".repeat(63) },
    { algorithm: "sha256", hex: "a".repeat(65) },
    { algorithm: "sha1", hex: "b".repeat(39) },
    { algorithm: "sha1", hex: "b".repeat(41) },
    { algorithm: "sha256", hex: `${"a".repeat(63)}g` },
    { algorithm: "sha1", hex: `${"b".repeat(39)}z` },
  ])("keeps immutable but omits a malformed $algorithm digest", ({ algorithm, hex }) => {
    const response = new Response(null, { status: 200 });
    const fact = contentAddressedPathFact(algorithm, hex);

    expect(relay(declarePathFact(response, fact))).toBe("immutable");
  });

  it.each([
    { algorithm: "sha512", hex: "c".repeat(128) },
    { algorithm: "MD5Sum", hex: "d".repeat(32) },
  ])("declares immutable alone for $algorithm", ({ algorithm, hex }) => {
    const response = new Response(null, { status: 200 });
    const fact = contentAddressedPathFact(algorithm, hex);

    expect(relay(declarePathFact(response, fact))).toBe("immutable");
  });
});

describe("relay path facts — directive merge", () => {
  it("keeps existing storage, freshness and prefetch directives", () => {
    const response = new Response(null, {
      status: 200,
      headers: { "x-mdb-relay": "no-store, ttl=60, prefetch, no-vary" },
    });
    const fact = contentAddressedPathFact("SHA256", SHA256.toUpperCase());

    expect(relay(declarePathFact(response, fact))).toBe(
      `no-store, ttl=60, prefetch, no-vary, immutable, sha256=${SHA256}`,
    );
  });

  it("never overrides an existing path declaration", () => {
    const other = "e".repeat(64);
    const response = new Response(null, {
      status: 200,
      headers: { "x-mdb-relay": `immutable, sha256=${other}` },
    });
    const fact = contentAddressedPathFact("sha256", SHA256);

    expect(relay(declarePathFact(response, fact))).toBe(`immutable, sha256=${other}`);
  });
});
