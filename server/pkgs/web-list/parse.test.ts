import { describe, expect, it } from "vitest";

import {
  cleanLastModified,
  cleanName,
  cleanSize,
  cleanType,
  guessColumns,
  rowsToEntries,
  type Cell,
} from "./parse";

// Pure stage-2 logic — no HTMLRewriter, runs in the node project. The full
// HTML→entries path against real listings lives in web-list.workers.test.ts.

describe("cleanSize", () => {
  it("parses unit suffixes into bytes", () => {
    expect(cleanSize("1.2K")).toBe(Math.round(1.2 * 1024));
    expect(cleanSize("15M")).toBe(15 * 1024 ** 2);
    expect(cleanSize("208K")).toBe(208 * 1024);
    expect(cleanSize("36M")).toBe(36 * 1024 ** 2);
  });

  it("parses bare byte counts", () => {
    expect(cleanSize("291")).toBe(291);
    expect(cleanSize(" 86 ")).toBe(86);
  });

  it("returns null for directories / blanks", () => {
    expect(cleanSize("-")).toBeNull();
    expect(cleanSize("  - ")).toBeNull();
    expect(cleanSize("")).toBeNull();
  });
});

describe("cleanLastModified", () => {
  it("parses `YYYY-MM-DD HH:MM` as UTC", () => {
    const d = cleanLastModified("2026-05-16 10:14  ");
    expect(d?.toISOString()).toBe("2026-05-16T10:14:00.000Z");
  });

  it("returns null when absent", () => {
    expect(cleanLastModified("")).toBeNull();
    expect(cleanLastModified("-")).toBeNull();
  });
});

describe("cleanType", () => {
  it("treats trailing-slash name/href as a directory", () => {
    expect(cleanType("dists/", "dists/")).toBe("directory");
    expect(cleanType("README", "README")).toBe("file");
    expect(cleanType("ls-lR.gz", "ls-lR.gz")).toBe("file");
  });
});

describe("cleanName", () => {
  it("strips a trailing slash", () => {
    expect(cleanName("dists/")).toBe("dists");
    expect(cleanName("README")).toBe("README");
  });
});

describe("guessColumns", () => {
  const header = (labels: string[]): Cell[] => labels.map((text) => ({ text }));

  it("maps Apache header labels to column indexes", () => {
    const cols = guessColumns([
      header(["", "Name", "Last modified", "Size", "Description"]),
    ]);
    expect(cols.name).toBe(1);
    expect(cols.lastModified).toBe(2);
    expect(cols.size).toBe(3);
    expect(cols.type).toBe(-1); // no `type` column in standard listings
  });

  it("ignores the `<hr>` separator row, picking the richest header", () => {
    const cols = guessColumns([
      header([""]), // colspan separator
      header(["", "Name", "Last modified", "Size"]),
    ]);
    expect(cols.name).toBe(1);
    expect(cols.size).toBe(3);
  });
});

describe("rowsToEntries", () => {
  const headerRows: Cell[][] = [
    [{ text: "" }, { text: "Name" }, { text: "Last modified" }, { text: "Size" }],
  ];
  const dataRow = (href: string, name: string, mtime: string, size: string): Cell[] => [
    { text: "" },
    { text: name, href },
    { text: mtime },
    { text: size },
  ];

  it("builds typed entries and derives type from the href", () => {
    const entries = rowsToEntries({
      headerRows,
      dataRows: [
        dataRow("README", "README", "2026-05-16 10:14  ", "1.2K"),
        dataRow("dists/", "dists/", "2026-05-16 10:15  ", "  - "),
      ],
    });

    expect(entries).toEqual([
      {
        name: "README",
        href: "README",
        type: "file",
        lastModified: new Date("2026-05-16T10:14:00Z"),
        size: Math.round(1.2 * 1024),
      },
      {
        name: "dists",
        href: "dists/",
        type: "directory",
        lastModified: new Date("2026-05-16T10:15:00Z"),
        size: null,
      },
    ]);
  });

  it("drops the parent-dir, sort, and absolute nav rows", () => {
    const entries = rowsToEntries({
      headerRows,
      dataRows: [
        dataRow("/", "Parent Directory", "", "  - "),
        dataRow("?C=N;O=D", "sort", "", ""),
        dataRow("README", "README", "2026-05-16 10:14  ", "1.2K"),
      ],
    });
    expect(entries.map((e) => e.name)).toEqual(["README"]);
  });

  it("drops short ancillary-table rows that lack the size column", () => {
    const entries = rowsToEntries({
      headerRows,
      // e.g. Debian's "Other directories" table: <td><a>doc</a></td><td>blurb</td>
      dataRows: [[{ text: "doc", href: "doc/" }, { text: "Debian documentation." }]],
    });
    expect(entries).toEqual([]);
  });

  it("returns [] when no name column is found", () => {
    expect(rowsToEntries({ headerRows: [], dataRows: [] })).toEqual([]);
  });
});
