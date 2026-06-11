/**
 * Minimal read-only WebDAV response helpers (RFC 4918 subset).
 *
 * Only the operations needed for anonymous directory browsing are implemented:
 * - OPTIONS (advertise DAV class 1)
 * - PROPFIND Depth 0/1 (multistatus listing)
 *
 * The XML is built via template strings — the shapes are fixed and trivial, so
 * pulling in an XML library would be overkill.
 */

import type { WebListEntry } from "./parse";

/** RFC 1123 date (required by `getlastmodified`). */
function httpDate(d: Date | null): string {
  if (!d) return new Date(0).toUTCString();
  return d.toUTCString();
}

/** Minimal XML entity escaping for text content / attribute values. */
function escXml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** Guess a content-type from a filename extension (bare minimum set). */
function mimeFromName(name: string): string {
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  const map: Record<string, string> = {
    gz: "application/gzip",
    xz: "application/x-xz",
    bz2: "application/x-bzip2",
    deb: "application/vnd.debian.binary-package",
    tar: "application/x-tar",
    html: "text/html",
    txt: "text/plain",
    json: "application/json",
    xml: "application/xml",
  };
  return map[ext] ?? "application/octet-stream";
}

/** Build a `<D:response>` element for one resource. */
function responseElement(href: string, entry: WebListEntry | "collection"): string {
  const isDir = entry === "collection" || entry.type === "directory";
  const displayname = entry === "collection" ? "" : escXml(entry.name);
  const lastmod = entry === "collection" ? httpDate(null) : httpDate(entry.lastModified);
  const size = entry === "collection" ? "" : entry.size != null ? String(entry.size) : "";
  const contentType = isDir ? "" : mimeFromName(typeof entry === "string" ? "" : entry.name);

  return `<D:response>
<D:href>${escXml(href)}</D:href>
<D:propstat>
<D:prop>
<D:displayname>${displayname}</D:displayname>
<D:getlastmodified>${lastmod}</D:getlastmodified>${
    size ? `\n<D:getcontentlength>${size}</D:getcontentlength>` : ""
  }${
    contentType ? `\n<D:getcontenttype>${contentType}</D:getcontenttype>` : ""
  }
<D:resourcetype>${isDir ? "<D:collection/>" : ""}</D:resourcetype>
</D:prop>
<D:status>HTTP/1.1 200 OK</D:status>
</D:propstat>
</D:response>`;
}

export interface PropfindResult {
  /** The directory path being listed (relative, with trailing slash). */
  path: string;
  /** Base href prefix to prepend (e.g. `/debian/`). */
  baseHref: string;
  /** Entries from readdir. */
  entries: WebListEntry[];
}

/**
 * Build a 207 Multi-Status response for a PROPFIND on a directory.
 *
 * @param depth `0` = just the directory itself, `1` = directory + children.
 */
export function propfindResponse(result: PropfindResult, depth: number): Response {
  const { path, baseHref, entries } = result;
  const selfHref = baseHref + path;

  const responses: string[] = [responseElement(selfHref, "collection")];

  if (depth >= 1) {
    for (const entry of entries) {
      const childHref =
        selfHref + encodeURIComponent(entry.name) + (entry.type === "directory" ? "/" : "");
      responses.push(responseElement(childHref, entry));
    }
  }

  const xml = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
${responses.join("\n")}
</D:multistatus>`;

  return new Response(xml, {
    status: 207,
    headers: {
      "content-type": "application/xml; charset=utf-8",
      dav: "1",
    },
  });
}

/** Standard OPTIONS response advertising WebDAV class 1 (read-only). */
export function optionsResponse(): Response {
  return new Response(null, {
    status: 204,
    headers: {
      allow: "OPTIONS, GET, HEAD, PROPFIND",
      dav: "1",
      "ms-author-via": "DAV",
    },
  });
}

/** 405 Method Not Allowed with the correct Allow header. */
export function methodNotAllowed(): Response {
  return new Response(null, {
    status: 405,
    headers: {
      allow: "OPTIONS, GET, HEAD, PROPFIND",
    },
  });
}
