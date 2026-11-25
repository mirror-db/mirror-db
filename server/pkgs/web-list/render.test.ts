import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";

import { defaultRender, encodeInlineJson, escapeHtml } from "./render";

/**
 * Filenames come from an upstream listing, so they are untrusted input to the
 * HTML shell. A name that closes the injected `<script>` would execute in a
 * visitor's browser on the mirror's own origin.
 */

const SHELL = [
  "<!DOCTYPE html><html><head><title><!--title--></title>",
  "<script>const PAGE_META = __PAGE_META__;</script></head>",
  "<body><div id=\"app\"><!--app-html--></div></body></html>",
].join("\n");

// `env` is the `cloudflare:workers` test stub, shared by the module under test.
(env as { ASSETS?: unknown }).ASSETS = {
  fetch: async () => new Response(SHELL),
};

describe("encodeInlineJson", () => {
  it("escapes the characters that can end an inline script", () => {
    expect(encodeInlineJson({ name: "</script><script>alert(1)</script>" })).toBe(
      '{"name":"\\u003c/script\\u003e\\u003cscript\\u003ealert(1)\\u003c/script\\u003e"}',
    );
    expect(encodeInlineJson({ a: "&" })).toBe('{"a":"\\u0026"}');
  });
});

describe("escapeHtml", () => {
  it("escapes text and attribute metacharacters", () => {
    expect(escapeHtml(`a<b>&"'`)).toBe("a&lt;b&gt;&amp;&quot;&#39;");
  });
});

describe("defaultRender", () => {
  it("keeps a hostile filename out of the injected script", async () => {
    const name = "</script><script>alert(1)</script>";
    const res = await defaultRender(
      [{ name, href: name, type: "file", lastModified: null, size: 1 }],
      "",
    );
    const html = await res.text();

    expect(html).not.toContain("</script><script>alert(1)");
    expect(html).toContain("\\u003c/script\\u003e");
    // The no-JS fallback escapes the same name as text and attribute.
    expect(html).toContain("&lt;/script&gt;&lt;script&gt;");
  });

  it("escapes the path in the document title", async () => {
    const res = await defaultRender([], `x"><img src=x onerror=alert(1)>`);
    const html = await res.text();

    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
  });
});
