import { describe, expect, it } from "vitest";

import { renderAptSources } from "./render";

const URI = "https://mirs.uk/debian/";

describe("renderAptSources — deb822 shape", () => {
  it("defaults Types to deb and omits empty optional fields", () => {
    expect(
      renderAptSources([{ uris: URI, suites: ["bookworm"] }]),
    ).toBe(
      ["Types: deb", `URIs: ${URI}`, "Suites: bookworm", ""].join("\n"),
    );
  });

  it("emits fields in the fixed order", () => {
    expect(
      renderAptSources([
        {
          uris: URI,
          suites: ["bookworm"],
          components: ["main"],
          architectures: ["amd64"],
          signedBy: "/usr/share/keyrings/debian-archive-keyring.gpg",
        },
      ]),
    ).toBe(
      [
        "Types: deb",
        `URIs: ${URI}`,
        "Suites: bookworm",
        "Components: main",
        "Architectures: amd64",
        "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg",
        "",
      ].join("\n"),
    );
  });

  it("joins list fields with a single space and supports several URIs", () => {
    expect(
      renderAptSources([
        {
          types: ["deb", "deb-src"],
          uris: [URI, "https://mirs.uk/debian-security/"],
          suites: ["bookworm", "bookworm-updates"],
          components: [],
          architectures: [],
        },
      ]),
    ).toBe(
      [
        "Types: deb deb-src",
        `URIs: ${URI} https://mirs.uk/debian-security/`,
        "Suites: bookworm bookworm-updates",
        "",
      ].join("\n"),
    );
  });

  it("separates stanzas with exactly one blank line and ends with one newline", () => {
    const file = renderAptSources([
      { uris: URI, suites: ["bookworm"] },
      { uris: URI, suites: ["trixie"] },
    ]);
    expect(file).toBe(
      [
        "Types: deb",
        `URIs: ${URI}`,
        "Suites: bookworm",
        "",
        "Types: deb",
        `URIs: ${URI}`,
        "Suites: trixie",
        "",
      ].join("\n"),
    );
    expect(file.endsWith("\n\n")).toBe(false);
  });

  it("prints comments above the stanza", () => {
    expect(
      renderAptSources([
        {
          comment: ["apt 3.x rejects the older CUDA certificate", ""],
          uris: URI,
          suites: ["stable"],
        },
      ]),
    ).toBe(
      [
        "# apt 3.x rejects the older CUDA certificate",
        "#",
        "Types: deb",
        `URIs: ${URI}`,
        "Suites: stable",
        "",
      ].join("\n"),
    );
  });

  it("rejects an empty stanza list", () => {
    expect(() => renderAptSources([])).toThrow();
  });

  it("rejects a stanza without suites or URIs", () => {
    expect(() => renderAptSources([{ uris: URI, suites: [] }])).toThrow();
    expect(() => renderAptSources([{ uris: [], suites: ["stable"] }])).toThrow();
  });
});

describe("renderAptSources — inline Signed-By", () => {
  // The value arrives already formatted as `/keys/apt/<name>/in-sources`
  // formats it: first line unindented, continuation lines with one space,
  // blank lines as `.` (deb822 has no empty continuation line).
  const sources = [
    "-----BEGIN PGP PUBLIC KEY BLOCK-----",
    " .",
    " mQINBF5dI2sBEACy",
    " =abcd",
    " -----END PGP PUBLIC KEY BLOCK-----",
  ].join("\n");

  it("puts the first line after `Signed-By: ` and keeps the rest verbatim", () => {
    expect(
      renderAptSources([
        { uris: "https://mirs.uk/mise/", suites: ["stable"], signedBy: { sources } },
      ]),
    ).toBe(
      [
        "Types: deb",
        "URIs: https://mirs.uk/mise/",
        "Suites: stable",
        `Signed-By: ${sources}`,
        "",
      ].join("\n"),
    );
  });
});
