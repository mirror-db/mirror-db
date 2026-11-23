import { describe, expect, it } from "vitest";

import { mergeCacheControl, sanitizeResponse } from "./http";

describe("sanitizeResponse", () => {
  it("strips auth challenges, cookies and hop-by-hop headers", () => {
    const upstream = new Response("x", {
      headers: {
        "www-authenticate": 'Bearer realm="x"',
        "set-cookie": "a=b",
        "set-cookie2": "c=d",
        age: "12",
        connection: "keep-alive",
        "keep-alive": "timeout=5",
        "transfer-encoding": "chunked",
        "content-type": "text/plain",
      },
    });

    const res = sanitizeResponse(upstream);

    for (const name of [
      "www-authenticate",
      "set-cookie",
      "set-cookie2",
      "age",
      "connection",
      "keep-alive",
      "transfer-encoding",
    ]) {
      expect(res.headers.has(name), name).toBe(false);
    }
    expect(res.headers.get("content-type")).toBe("text/plain");
  });

  it("delivers Vary unchanged", () => {
    // A relay builds cache variants from it; a client needs it for its own
    // representation negotiation. A Vary response bypasses the Worker cache.
    const upstream = new Response("x", {
      headers: { vary: "accept-encoding, x-relaytest-variant" },
    });

    expect(sanitizeResponse(upstream).headers.get("vary")).toBe(
      "accept-encoding, x-relaytest-variant",
    );
  });

  it("strips an upstream-injected X-MDB-Relay control header", () => {
    // Only our own middleware may add directives; a third-party upstream must
    // not be able to turn off caching or freeze a variant set.
    const upstream = new Response("x", {
      headers: { "x-mdb-relay": "no-store" },
    });

    expect(sanitizeResponse(upstream).headers.has("x-mdb-relay")).toBe(false);
  });

  it("leaves Cache-Control alone without an override", () => {
    const upstream = new Response("x", {
      headers: { "cache-control": "public, max-age=60, no-transform" },
    });

    expect(sanitizeResponse(upstream).headers.get("cache-control")).toBe(
      "public, max-age=60, no-transform",
    );
  });

  it("keeps upstream no-transform when a mirror overrides freshness", () => {
    const upstream = new Response("x", {
      status: 200,
      headers: { "cache-control": "private, max-age=60, no-transform" },
    });

    const res = sanitizeResponse(upstream, {
      cacheControl: "public, max-age=31536000, immutable",
    });

    expect(res.headers.get("cache-control")).toBe(
      "no-transform, public, max-age=31536000, immutable",
    );
  });

  it("lets an override replace cacheability directives", () => {
    const upstream = new Response("x", {
      status: 200,
      headers: { "cache-control": "public, max-age=60, immutable" },
    });

    const res = sanitizeResponse(upstream, {
      cacheControl: "public, max-age=0, must-revalidate",
    });

    expect(res.headers.get("cache-control")).toBe(
      "public, max-age=0, must-revalidate",
    );
  });

  it("does not apply the override to non-200 responses", () => {
    const upstream = new Response(null, {
      status: 304,
      headers: { "cache-control": "no-cache, no-transform" },
    });

    const res = sanitizeResponse(upstream, { cacheControl: "public, max-age=60" });

    expect(res.headers.get("cache-control")).toBe("no-cache, no-transform");
  });
});

describe("mergeCacheControl", () => {
  it("preserves directives the override does not own", () => {
    expect(
      mergeCacheControl("public, no-transform, max-age=5", "max-age=600"),
    ).toBe("no-transform, max-age=600");
  });

  it("does not duplicate a directive the override spells out itself", () => {
    expect(mergeCacheControl("no-transform", "public, max-age=60, no-transform")).toBe(
      "public, max-age=60, no-transform",
    );
  });

  it("handles a missing upstream value", () => {
    expect(mergeCacheControl(null, "public, max-age=60")).toBe(
      "public, max-age=60",
    );
  });

  it("keeps quoted commas intact", () => {
    expect(
      mergeCacheControl('no-cache="set-cookie, foo", no-transform', "max-age=600"),
    ).toBe('no-transform, max-age=600');
  });
});
