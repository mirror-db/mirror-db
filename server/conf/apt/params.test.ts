import { describe, expect, it } from "vitest";

import { AptQueryError, parseAptQuery } from "./params";

const query = (search: string) => new URLSearchParams(search);

describe("parseAptQuery — aliases", () => {
  it("accepts every documented alias", () => {
    expect(parseAptQuery(query("suite=bookworm"))).toEqual({ suites: ["bookworm"] });
    expect(parseAptQuery(query("suites=bookworm"))).toEqual({ suites: ["bookworm"] });
    expect(parseAptQuery(query("component=main"))).toEqual({ components: ["main"] });
    expect(parseAptQuery(query("components=main"))).toEqual({ components: ["main"] });
    expect(parseAptQuery(query("comp=main"))).toEqual({ components: ["main"] });
    expect(parseAptQuery(query("comps=main"))).toEqual({ components: ["main"] });
    expect(parseAptQuery(query("architecture=amd64"))).toEqual({
      architectures: ["amd64"],
    });
    expect(parseAptQuery(query("architectures=amd64"))).toEqual({
      architectures: ["amd64"],
    });
    expect(parseAptQuery(query("arch=amd64"))).toEqual({ architectures: ["amd64"] });
    expect(parseAptQuery(query("archs=amd64"))).toEqual({ architectures: ["amd64"] });
    expect(parseAptQuery(query("protocol=http"))).toEqual({ protocol: "http" });
    expect(parseAptQuery(query("proto=https"))).toEqual({ protocol: "https" });
  });

  it("returns no overrides for an empty query", () => {
    expect(parseAptQuery(query(""))).toEqual({});
  });
});

describe("parseAptQuery — CSV handling", () => {
  it("splits, trims, deduplicates, and preserves order", () => {
    expect(
      parseAptQuery(query("suites=bookworm,%20bookworm%20,%20trixie&arch=arm64,%20amd64")),
    ).toEqual({
      suites: ["bookworm", "trixie"],
      architectures: ["arm64", "amd64"],
    });
  });

  it("merges repeated occurrences of the same parameter", () => {
    expect(parseAptQuery(query("suite=bookworm&suite=trixie"))).toEqual({
      suites: ["bookworm", "trixie"],
    });
  });

  it("allows an explicitly empty components value", () => {
    expect(parseAptQuery(query("components="))).toEqual({ components: [] });
  });

  it("rejects an empty suite or architecture", () => {
    expect(() => parseAptQuery(query("suites="))).toThrow(AptQueryError);
    expect(() => parseAptQuery(query("arch="))).toThrow(AptQueryError);
    expect(() => parseAptQuery(query("suite=,%20"))).toThrow(AptQueryError);
  });

  it("rejects unknown parameters", () => {
    expect(() => parseAptQuery(query("suit=bookworm"))).toThrow(AptQueryError);
    expect(() => parseAptQuery(query("components=main&signed-by=/x.gpg"))).toThrow(
      AptQueryError,
    );
  });

  it("rejects values that would inject another deb822 field", () => {
    // `%0A` decodes to a newline; Suites is space-joined into one field, so a
    // whitespace-bearing token is always malformed input.
    expect(() =>
      parseAptQuery(query("suite=bookworm%0ASigned-By:%20/tmp/evil.gpg")),
    ).toThrow(AptQueryError);
    expect(() => parseAptQuery(query("arch=amd64%09arm64"))).toThrow(AptQueryError);
  });
});

describe("parseAptQuery — conflicting aliases", () => {
  it("rejects two aliases of one group selecting different values", () => {
    expect(() => parseAptQuery(query("suite=bookworm&suites=trixie"))).toThrow(
      /conflicting suites/,
    );
    expect(() => parseAptQuery(query("comp=main&components=contrib"))).toThrow(
      /conflicting components/,
    );
    expect(() => parseAptQuery(query("arch=amd64&archs=arm64"))).toThrow(
      /conflicting architectures/,
    );
    expect(() => parseAptQuery(query("protocol=http&proto=https"))).toThrow(
      /conflicting protocol/,
    );
  });

  it("accepts identical values spelled through different aliases", () => {
    expect(parseAptQuery(query("suite=bookworm&suites=bookworm"))).toEqual({
      suites: ["bookworm"],
    });
    // Order is part of the value: `a,b` and `b,a` are different selections.
    expect(() => parseAptQuery(query("arch=amd64,arm64&archs=arm64,amd64"))).toThrow(
      /conflicting architectures/,
    );
    expect(parseAptQuery(query("protocol=http&proto=http"))).toEqual({
      protocol: "http",
    });
  });
});

describe("parseAptQuery — protocol", () => {
  it("is absent unless requested", () => {
    expect(parseAptQuery(query(""))).not.toHaveProperty("protocol");
    expect(parseAptQuery(query("suites=bookworm"))).not.toHaveProperty("protocol");
  });

  it("accepts repeated occurrences of the same value", () => {
    expect(parseAptQuery(query("protocol=http&protocol=http"))).toEqual({
      protocol: "http",
    });
    expect(parseAptQuery(query("proto=https&proto=https"))).toEqual({
      protocol: "https",
    });
  });

  it("rejects repeated occurrences with different values", () => {
    expect(() => parseAptQuery(query("protocol=http&protocol=https"))).toThrow(
      /conflicting protocol/,
    );
  });

  it("rejects an empty value", () => {
    expect(() => parseAptQuery(query("protocol="))).toThrow(AptQueryError);
    expect(() => parseAptQuery(query("proto="))).toThrow(AptQueryError);
  });

  it("rejects any scheme other than http or https", () => {
    for (const value of ["ftp", "HTTP", "ws", "httpss"]) {
      expect(() => parseAptQuery(query(`protocol=${value}`))).toThrow(
        AptQueryError,
      );
    }
  });

  it("rejects a comma-separated value", () => {
    expect(() => parseAptQuery(query("protocol=http,https"))).toThrow(
      AptQueryError,
    );
    expect(() => parseAptQuery(query("proto=https,http"))).toThrow(
      AptQueryError,
    );
  });

  it("combines with list overrides", () => {
    expect(parseAptQuery(query("proto=http&suites=bookworm&arch=arm64"))).toEqual({
      protocol: "http",
      suites: ["bookworm"],
      architectures: ["arm64"],
    });
  });
});
