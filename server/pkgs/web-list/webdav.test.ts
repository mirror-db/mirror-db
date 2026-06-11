import { describe, expect, it } from "vitest";

import type { WebListEntry } from "./parse";
import { methodNotAllowed, optionsResponse, propfindResponse } from "./webdav";

const entries: WebListEntry[] = [
  {
    name: "dists",
    href: "dists/",
    type: "directory",
    lastModified: new Date("2026-05-16T10:15:00Z"),
    size: null,
  },
  {
    name: "README",
    href: "README",
    type: "file",
    lastModified: new Date("2026-05-16T10:14:00Z"),
    size: 1229,
  },
  {
    name: "ls-lR.gz",
    href: "ls-lR.gz",
    type: "file",
    lastModified: new Date("2026-06-10T14:13:00Z"),
    size: 15 * 1024 ** 2,
  },
];

describe("optionsResponse", () => {
  it("returns 204 with DAV:1 and correct Allow header", () => {
    const res = optionsResponse();
    expect(res.status).toBe(204);
    expect(res.headers.get("dav")).toBe("1");
    expect(res.headers.get("allow")).toBe("OPTIONS, GET, HEAD, PROPFIND");
  });
});

describe("methodNotAllowed", () => {
  it("returns 405 with Allow header", () => {
    const res = methodNotAllowed();
    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toBe("OPTIONS, GET, HEAD, PROPFIND");
  });
});

describe("propfindResponse", () => {
  it("returns 207 with application/xml content-type and DAV header", async () => {
    const res = propfindResponse(
      { path: "", baseHref: "/debian/", entries },
      1,
    );
    expect(res.status).toBe(207);
    expect(res.headers.get("content-type")).toBe("application/xml; charset=utf-8");
    expect(res.headers.get("dav")).toBe("1");
  });

  it("Depth 0 returns only the collection itself", async () => {
    const res = propfindResponse(
      { path: "dists/", baseHref: "/debian/", entries },
      0,
    );
    const xml = await res.text();
    const count = (xml.match(/<D:response>/g) || []).length;
    expect(count).toBe(1);
    // Self href
    expect(xml).toContain("<D:href>/debian/dists/</D:href>");
    expect(xml).toContain("<D:collection/>");
    // No children
    expect(xml).not.toContain("README");
  });

  it("Depth 1 returns the collection plus all children", async () => {
    const res = propfindResponse(
      { path: "", baseHref: "/debian/", entries },
      1,
    );
    const xml = await res.text();
    const count = (xml.match(/<D:response>/g) || []).length;
    // 1 (self) + 3 (entries)
    expect(count).toBe(4);
  });

  it("directories get <D:collection/> resourcetype and trailing slash in href", async () => {
    const res = propfindResponse(
      { path: "", baseHref: "/", entries },
      1,
    );
    const xml = await res.text();
    expect(xml).toContain("<D:href>/dists/</D:href>");
    // The dists entry has collection resourcetype
    const distsBlock = xml.split("<D:href>/dists/</D:href>")[1]!.split("</D:response>")[0]!;
    expect(distsBlock).toContain("<D:collection/>");
  });

  it("files get contentlength, contenttype, and no trailing slash", async () => {
    const res = propfindResponse(
      { path: "", baseHref: "/", entries },
      1,
    );
    const xml = await res.text();
    // README entry
    expect(xml).toContain("<D:href>/README</D:href>");
    const readmeBlock = xml.split("<D:href>/README</D:href>")[1]!.split("</D:response>")[0]!;
    expect(readmeBlock).toContain("<D:getcontentlength>1229</D:getcontentlength>");
    expect(readmeBlock).not.toContain("<D:collection/>");
    // ls-lR.gz gets gzip content type
    const gzBlock = xml.split("ls-lR.gz</D:href>")[1]!.split("</D:response>")[0]!;
    expect(gzBlock).toContain("<D:getcontenttype>application/gzip</D:getcontenttype>");
    expect(gzBlock).toContain(
      `<D:getcontentlength>${15 * 1024 ** 2}</D:getcontentlength>`,
    );
  });

  it("getlastmodified uses RFC 1123 format", async () => {
    const res = propfindResponse(
      { path: "", baseHref: "/", entries },
      1,
    );
    const xml = await res.text();
    // The README date in RFC 1123
    expect(xml).toContain(
      `<D:getlastmodified>${new Date("2026-05-16T10:14:00Z").toUTCString()}</D:getlastmodified>`,
    );
  });

  it("escapes XML entities in entry names and hrefs", async () => {
    const tricky: WebListEntry[] = [
      {
        name: "a&b<c",
        href: "a&b<c",
        type: "file",
        lastModified: null,
        size: 42,
      },
    ];
    const res = propfindResponse(
      { path: "", baseHref: "/", entries: tricky },
      1,
    );
    const xml = await res.text();
    expect(xml).toContain("<D:displayname>a&amp;b&lt;c</D:displayname>");
    expect(xml).toContain("<D:href>/a%26b%3Cc</D:href>");
  });

  it("null lastModified falls back to epoch", async () => {
    const noDate: WebListEntry[] = [
      { name: "x", href: "x", type: "file", lastModified: null, size: 0 },
    ];
    const res = propfindResponse(
      { path: "", baseHref: "/", entries: noDate },
      1,
    );
    const xml = await res.text();
    expect(xml).toContain(
      `<D:getlastmodified>${new Date(0).toUTCString()}</D:getlastmodified>`,
    );
  });

  it("null size omits getcontentlength", async () => {
    const noSize: WebListEntry[] = [
      { name: "y", href: "y", type: "file", lastModified: null, size: null },
    ];
    const res = propfindResponse(
      { path: "", baseHref: "/", entries: noSize },
      1,
    );
    const xml = await res.text();
    const yBlock = xml.split("/y</D:href>")[1]!.split("</D:response>")[0]!;
    expect(yBlock).not.toContain("<D:getcontentlength>");
  });
});
