import { describe, expect, it } from "vitest";

import { StanzaStreamParser, type Stanza } from "./stanza";

function parseChunks(chunks: string[]): Stanza[] {
  const stanzas: Stanza[] = [];
  const parser = new StanzaStreamParser((stanza) => stanzas.push(stanza));
  for (const chunk of chunks) parser.push(chunk);
  parser.end();
  return stanzas;
}

describe("StanzaStreamParser", () => {
  it("parses several stanzas separated by blank lines", () => {
    const stanzas = parseChunks([
      [
        "Package: a",
        "Version: 1",
        "",
        "Package: b",
        "Version: 2",
        "",
      ].join("\n"),
    ]);

    expect(stanzas).toHaveLength(2);
    expect(stanzas[0]!.get("package")).toBe("a");
    expect(stanzas[1]!.get("version")).toBe("2");
  });

  it("unfolds continuation lines into the previous field", () => {
    const stanzas = parseChunks([
      [
        "Package: nginx",
        "Description: small web server",
        " with more words",
        "\tand a tabbed line",
        "Size: 42",
        "",
      ].join("\n"),
    ]);

    expect(stanzas[0]!.get("description")).toBe(
      "small web server\nwith more words\nand a tabbed line",
    );
    // A continuation must not become its own field.
    expect(stanzas[0]!.has("size")).toBe(true);
    expect(stanzas[0]!.get("size")).toBe("42");
  });

  it("handles CRLF line endings", () => {
    const stanzas = parseChunks(["Package: a\r\nVersion: 1\r\n\r\n"]);
    expect(stanzas).toHaveLength(1);
    expect(stanzas[0]!.get("package")).toBe("a");
    expect(stanzas[0]!.get("version")).toBe("1");
  });

  it("reassembles a line split across chunks", () => {
    const stanzas = parseChunks(["Package: ne", "tpkg\nVers", "ion: 3\n", "\n"]);
    expect(stanzas[0]!.get("package")).toBe("netpkg");
    expect(stanzas[0]!.get("version")).toBe("3");
  });

  it("flushes a final stanza without a trailing blank line", () => {
    const stanzas = parseChunks(["Package: tail"]);
    expect(stanzas).toHaveLength(1);
    expect(stanzas[0]!.get("package")).toBe("tail");
  });

  it("ignores an empty body", () => {
    expect(parseChunks(["", "\n\n"])).toHaveLength(0);
  });

  it("keeps multi-line Files-style blocks intact", () => {
    const stanzas = parseChunks([
      [
        "Directory: pool/main/n/nginx",
        "Files:",
        " 11111111111111111111111111111111 10 a.dsc",
        " 22222222222222222222222222222222 20 a.tar.gz",
        "Checksums-Sha256:",
        ` ${"a".repeat(64)} 10 a.dsc`,
        "",
      ].join("\n"),
    ]);

    expect(stanzas[0]!.get("files")).toBe(
      "11111111111111111111111111111111 10 a.dsc\n22222222222222222222222222222222 20 a.tar.gz",
    );
  });
});
