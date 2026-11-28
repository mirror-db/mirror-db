import { describe, expect, it } from "vitest";

// Raw HTML fixtures captured from the live mirrors.
import debianHtml from "./__fixtures__/cloudflaremirrors-debian.html?raw";
import ubuntuHtml from "./__fixtures__/nl-archive-ubuntu.html?raw";
import nvidiaHtml from "./__fixtures__/nvidia-cuda-repos.html?raw";
import releasesUbuntuHtml from "./__fixtures__/releases-ubuntu.html?raw";
import cdimageUbuntuHtml from "./__fixtures__/cdimage-ubuntu.html?raw";
import microsoftHtml from "./__fixtures__/packages-microsoft.html?raw";
import { parseListing } from "./parse";
import {
  createBareUlParser,
  createPreParser,
  createTableParser,
  createUlParser,
} from "./parsers";
import { WebListError, WebListFs, type WebListEntry } from "./web-list";

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

const parserRecognitionCases = [
  {
    name: "table",
    factory: createTableParser,
    empty: "<table><tr><th>Name</th><th>Last modified</th><th>Size</th></tr></table>",
    unrecognized: "<table><tr><th>Product Name</th></tr><tr><td>Keyboard</td></tr></table>",
  },
  {
    name: "NVIDIA ul",
    factory: createUlParser,
    empty: '<ul class="directorycontents"></ul>',
    unrecognized: '<ul class="navigation"><li><a href="x">x</a></li></ul>',
  },
  {
    name: "pre",
    factory: createPreParser,
    empty: '<pre><a href="?C=N;O=D">Name</a> <a href="?C=M;O=A">Last modified</a> <a href="?C=S;O=A">Size</a></pre>',
    unrecognized: "<pre>This is introductory text, not a directory listing.</pre>",
  },
];

describe.each(parserRecognitionCases)("parseListing — $name recognition", ({
  factory,
  empty,
  unrecognized,
}) => {
  it("returns an empty list when its format is recognized but has no entries", async () => {
    expect(await parseListing(empty, [factory])).toEqual([]);
  });

  it("returns null when the page is not its listing format", async () => {
    expect(await parseListing(unrecognized, [factory])).toBeNull();
  });
});

describe("parseListing — bare ul recognition", () => {
  it.each([
    "<ul></ul>",
    '<ul><li><a href="/login">Sign in</a></li></ul>',
    '<ul class="navigation"><li><a href="x">x</a></li></ul>',
  ])("returns null without a valid non-parent entry: %s", async (html) => {
    expect(await parseListing(html, [createBareUlParser])).toBeNull();
  });

  it("recognizes a listing when it yields a valid non-parent entry", async () => {
    const entries = await parseListing(
      '<ul><li><a href="release/">release/</a></li></ul>',
      [createBareUlParser],
    );
    expect(entries?.map((entry) => entry.name)).toEqual(["release"]);
  });
});

describe("parseListing — result selection", () => {
  it("keeps the first non-empty parser result ahead of later parsers", async () => {
    const html = `
      <pre><a href="from-pre">from-pre</a></pre>
      <table>
        <tr><th>Name</th></tr>
        <tr><td><a href="from-table">from-table</a></td></tr>
      </table>`;

    const entries = await parseListing(html, [createPreParser, createTableParser]);

    expect(entries?.map((entry) => entry.name)).toEqual(["from-pre"]);
  });

  it("continues past a recognized empty parser to a later non-empty result", async () => {
    const html = `
      <table><tr><th>Name</th><th>Size</th></tr></table>
      <ul class="directorycontents"><li><span class="file"><a href="from-ul">from-ul</a></span></li></ul>`;

    const entries = await parseListing(html, [createTableParser, createUlParser]);

    expect(entries?.map((entry) => entry.name)).toEqual(["from-ul"]);
  });

  it("returns null when no parser recognizes a successful HTML page", async () => {
    const html = "<html><body><h1>Welcome</h1><pre>Sign in to continue.</pre></body></html>";
    expect(await parseListing(html)).toBeNull();
  });

  it.each([
    '<ul><li><a href="/login">Sign in</a></li></ul>',
    "<table><tr><th>Product Name</th></tr><tr><td>Keyboard</td></tr></table>",
  ])("rejects an ambiguous non-listing page with the default parsers: %s", async (html) => {
    expect(await parseListing(html)).toBeNull();
  });

  it("keeps valid entries from a name-only table", async () => {
    const html = `
      <table>
        <tr><th>Name</th></tr>
        <tr><td><a href="from-table">from-table</a></td></tr>
      </table>`;
    const entries = await parseListing(html, [createTableParser]);
    expect(entries?.map((entry) => entry.name)).toEqual(["from-table"]);
  });
});

describe("WebListFs.readdir — listing recognition", () => {
  it("turns a successful but unrecognized page into a diagnostic 502", async () => {
    const { fs } = fsFor(
      "https://portal.invalid/private/",
      "<html><body>secret upstream response body</body></html>",
    );

    const pending = fs.readdir("nested");
    await expect(pending).rejects.toBeInstanceOf(WebListError);
    await expect(pending).rejects.toMatchObject({
      status: 502,
      message: expect.stringContaining("unrecognized directory listing"),
    });
    await expect(pending).rejects.toMatchObject({
      message: expect.not.stringContaining("secret upstream response body"),
    });
  });
});

describe("WebListFs.readdir — ftp.hk.debian.org/debian", () => {
  it("parses every listing row into a typed entry", async () => {
    const { fs } = fsFor("http://ftp.us.debian.org/debian/", debianHtml);
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
    const { fs, lastUrl } = fsFor("http://ftp.us.debian.org/debian", debianHtml);
    expect(fs.url("pool/")).toBe("http://ftp.us.debian.org/debian/pool/");

    await fs.readdir("dists");
    expect(lastUrl()).toBe("http://ftp.us.debian.org/debian/dists/");

    await fs.fetch("README");
    expect(lastUrl()).toBe("http://ftp.us.debian.org/debian/README");
  });

  it("passes the first empty segment, later empty segments and escapes to fetch", async () => {
    const { fs, lastUrl } = fsFor("http://ftp.us.debian.org/debian", debianHtml);

    await fs.fetch("/empty//x%2fCase%2F");

    expect(lastUrl()).toBe("http://ftp.us.debian.org/debian//empty//x%2fCase%2F");
  });
});

describe("WebListFs.readdir — developer.download.nvidia.com (UL-based)", () => {
  it("parses <ul>/<li>/<span> NVIDIA-style listing", async () => {
    const { fs } = fsFor(
      "https://developer.download.nvidia.com/compute/cuda/repos/",
      nvidiaHtml,
    );
    const entries = await fs.readdir();
    const map = byName(entries);

    // Parent `..\` is excluded; both files and directories are captured.
    expect(Object.keys(map).sort()).toEqual(
      [
        "GPGKEY",
        "amzn2023",
        "cuda-keyring_1.1-1_all.deb",
        "debian10",
        "debian11",
        "debian12",
        "fedora39",
        "ubuntu2004",
        "ubuntu2204",
        "ubuntu2404",
      ].sort(),
    );

    // Files have correct type, size, and date
    expect(map["GPGKEY"]).toMatchObject({
      type: "file",
      href: "GPGKEY",
      size: Math.round(4.0 * 1024),
    });
    expect(map["GPGKEY"].lastModified?.toISOString()).toBe("2014-05-09T01:12:00.000Z");

    expect(map["cuda-keyring_1.1-1_all.deb"]).toMatchObject({
      type: "file",
      href: "cuda-keyring_1.1-1_all.deb",
      size: Math.round(5.2 * 1024),
    });
    expect(map["cuda-keyring_1.1-1_all.deb"].lastModified?.toISOString()).toBe(
      "2024-03-14T18:30:00.000Z",
    );

    // Directories have no size/date
    expect(map["amzn2023"]).toMatchObject({
      type: "directory",
      href: "amzn2023/",
      size: null,
    });
    expect(map["debian12"]).toMatchObject({ type: "directory", href: "debian12/" });
    expect(map["ubuntu2404"]).toMatchObject({ type: "directory", href: "ubuntu2404/" });
  });
});

describe("WebListFs.readdir — releases.ubuntu.com (pre-based)", () => {
  it("parses <pre>-based Apache autoindex listing", async () => {
    const { fs } = fsFor("https://releases.ubuntu.com/", releasesUbuntuHtml);
    const entries = await fs.readdir();
    const map = byName(entries);

    // Sort links (?C=...) and absolute URLs are excluded.
    expect(Object.keys(map).length).toBeGreaterThanOrEqual(20);

    // Directories are correctly typed
    expect(map["noble"]).toMatchObject({
      type: "directory",
      href: "noble/",
      size: null,
    });
    expect(map["noble"].lastModified?.toISOString()).toBe("2026-03-10T17:24:00.000Z");

    expect(map["jammy"]).toMatchObject({ type: "directory", href: "jammy/" });
    expect(map["resolute"]).toMatchObject({ type: "directory", href: "resolute/" });
    expect(map["26.04"]).toMatchObject({ type: "directory", href: "26.04/" });

    // Verify versioned entries
    expect(map["14.04.6"]).toMatchObject({
      type: "directory",
      href: "14.04.6/",
    });
    expect(map["14.04.6"].lastModified?.toISOString()).toBe("2025-07-31T15:39:00.000Z");

    // Size is `-` for all directories → null
    for (const e of entries) {
      expect(e.size).toBeNull();
    }
  });
});

describe("WebListFs.readdir — cdimage.ubuntu.com (bare <ul>/<li>)", () => {
  it("parses classless Apache <ul> listing with no metadata", async () => {
    const { fs } = fsFor("https://cdimage.ubuntu.com/", cdimageUbuntuHtml);
    const entries = await fs.readdir();
    const map = byName(entries);

    // All entries from the bare <ul> listing (Parent Directory excluded via /).
    expect(Object.keys(map).length).toBeGreaterThanOrEqual(20);

    // Known directories
    expect(map["ubuntu"]).toMatchObject({
      type: "directory",
      href: "ubuntu/",
    });
    expect(map["kubuntu"]).toMatchObject({ type: "directory", href: "kubuntu/" });
    expect(map["xubuntu"]).toMatchObject({ type: "directory", href: "xubuntu/" });
    expect(map["noble"]).toMatchObject({ type: "directory", href: "noble/" });

    // No metadata in this format
    for (const e of entries) {
      expect(e.lastModified).toBeNull();
      expect(e.size).toBeNull();
    }
  });
});

describe("WebListFs.readdir — packages.microsoft.com (multi-pre)", () => {
  it("parses <pre>-based listing with introductory text block", async () => {
    const { fs } = fsFor("https://packages.microsoft.com/", microsoftHtml);
    const entries = await fs.readdir();
    const map = byName(entries);

    // Real directory entries from the listing <pre> block
    expect(map["ubuntu"]).toMatchObject({ type: "directory", href: "ubuntu/" });
    expect(map["debian"]).toMatchObject({ type: "directory", href: "debian/" });
    expect(map["fedora"]).toMatchObject({ type: "directory", href: "fedora/" });
    expect(map["keys"]).toMatchObject({ type: "directory", href: "keys/" });
    expect(map["repos"]).toMatchObject({ type: "directory", href: "repos/" });

    // Date format is DD-Mon-YYYY — not ISO, so cleanLastModified returns null
    expect(map["ubuntu"].lastModified).toBeNull();

    // No sizes (all `-`)
    expect(map["ubuntu"].size).toBeNull();
  });
});
