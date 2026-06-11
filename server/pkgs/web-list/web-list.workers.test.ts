import { describe, expect, it } from "vitest";

// Raw HTML fixtures captured from the live mirrors.
import debianHtml from "./__fixtures__/cloudflaremirrors-debian.html?raw";
import ubuntuHtml from "./__fixtures__/nl-archive-ubuntu.html?raw";
import { WebListFs, type WebListEntry } from "./web-list";

// Runs in the workers pool → real HTMLRewriter. The fetch impl is stubbed so
// the test is offline + deterministic; only the parsing is under test.
const fsFor = (base: string, html: string) => {
  let lastUrl = "";
  const fs = new WebListFs(base, {
    fetchImpl: async (input) => {
      lastUrl = typeof input === "string" ? input : (input as Request).url;
      return new Response(html, { headers: { "content-type": "text/html" } });
    },
  });
  return { fs, lastUrl: () => lastUrl };
};

const byName = (entries: WebListEntry[]) =>
  Object.fromEntries(entries.map((e) => [e.name, e]));

describe("WebListFs.readdir — cloudflaremirrors.com/debian", () => {
  it("parses every listing row into a typed entry", async () => {
    const { fs } = fsFor("https://cloudflaremirrors.com/debian/", debianHtml);
    const entries = await fs.readdir();
    const map = byName(entries);

    // Parent Directory + the "Other directories" 2-cell table are excluded.
    expect(Object.keys(map).sort()).toEqual(
      [
        "README",
        "README.CD-manufacture",
        "README.html",
        "README.mirrors.html",
        "README.mirrors.txt",
        "dists",
        "doc",
        "extrafiles",
        "indices",
        "ls-lR.gz",
        "pool",
        "project",
        "tools",
        "zzz-dists",
      ].sort(),
    );

    expect(map["dists"]).toMatchObject({ type: "directory", href: "dists/", size: null });
    expect(map["README"]).toMatchObject({
      type: "file",
      href: "README",
      size: Math.round(1.2 * 1024),
    });
    expect(map["README"].lastModified?.toISOString()).toBe("2026-05-16T10:14:00.000Z");
    expect(map["ls-lR.gz"]).toMatchObject({ type: "file", size: 15 * 1024 ** 2 });
    expect(map["extrafiles"]).toMatchObject({ type: "file", size: 208 * 1024 });
  });
});

describe("WebListFs.readdir — nl.archive.ubuntu.com/ubuntu", () => {
  it("parses the listing despite the extra Description column", async () => {
    const { fs } = fsFor("https://nl.archive.ubuntu.com/ubuntu/", ubuntuHtml);
    const entries = await fs.readdir();
    const map = byName(entries);

    expect(Object.keys(map).sort()).toEqual(
      ["dists", "indices", "ls-lR.gz", "pool", "project", "ubuntu"].sort(),
    );
    expect(map["dists"]).toMatchObject({ type: "directory", size: null });
    expect(map["ls-lR.gz"]).toMatchObject({ type: "file", size: 36 * 1024 ** 2 });
    expect(map["dists"].lastModified?.toISOString()).toBe("2026-04-24T11:23:00.000Z");
  });
});

describe("WebListFs url/fetch", () => {
  it("joins relative paths onto the base and adds a trailing slash for readdir", async () => {
    const { fs, lastUrl } = fsFor("https://cloudflaremirrors.com/debian", debianHtml);
    expect(fs.url("pool/")).toBe("https://cloudflaremirrors.com/debian/pool/");

    await fs.readdir("dists");
    expect(lastUrl()).toBe("https://cloudflaremirrors.com/debian/dists/");

    await fs.fetch("README");
    expect(lastUrl()).toBe("https://cloudflaremirrors.com/debian/README");
  });
});
