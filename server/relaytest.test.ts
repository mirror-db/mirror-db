import { describe, expect, it } from "vitest";

import { buildMirrorManifest } from "./api/mirror-manifest";
import { handleRelaytest } from "./relaytest";

const CASES = [
  "fresh",
  "revalidate",
  "ttl",
  "vary",
  "encoding",
  "range",
  "no-store",
  "digest",
] as const;

const get = (name: string, headers: Record<string, string> = {}) =>
  new Request(`https://mirs.uk/relaytest/${name}`, { headers });

const head = (name: string, headers: Record<string, string> = {}) =>
  new Request(`https://mirs.uk/relaytest/${name}`, { method: "HEAD", headers });

const hex = (buffer: ArrayBuffer): string =>
  Array.from(new Uint8Array(buffer), (b) => b.toString(16).padStart(2, "0")).join("");

describe("relaytest — GET/HEAD parity", () => {
  it.each(CASES)("serves %s with the same status and headers for HEAD", async (name) => {
    const getRes = await handleRelaytest(get(name), name);
    const headRes = await handleRelaytest(head(name), name);

    expect(headRes.status).toBe(getRes.status);
    expect(await headRes.text()).toBe("");

    const comparable = [
      "etag",
      "cache-control",
      "vary",
      "content-type",
      "content-length",
      "accept-ranges",
      "content-range",
      "content-encoding",
      "x-mdb-relay",
      "digest",
      "content-digest",
      "docker-content-digest",
    ];
    for (const header of comparable) {
      expect(headRes.headers.get(header), `${name} ${header}`).toBe(
        getRes.headers.get(header),
      );
    }
  });
});

describe("relaytest — freshness cases", () => {
  it("fresh returns a strong validator and a long lifetime", async () => {
    const res = await handleRelaytest(get("fresh"), "fresh");

    expect(res.status).toBe(200);
    expect(res.headers.get("etag")).toBe('"fresh-v1"');
    expect(res.headers.get("cache-control")).toBe("public, max-age=3600");
    expect(await res.text()).toBe("relaytest: fresh v1\n");
  });

  it("revalidate answers a matching If-None-Match with 304", async () => {
    const first = await handleRelaytest(get("revalidate"), "revalidate");
    expect(first.status).toBe(200);
    expect(first.headers.get("cache-control")).toBe("no-cache");

    const res = await handleRelaytest(
      get("revalidate", { "if-none-match": '"revalidate-v1"' }),
      "revalidate",
    );

    expect(res.status).toBe(304);
    expect(res.headers.get("etag")).toBe('"revalidate-v1"');
    expect(await res.text()).toBe("");
  });

  it("ttl supplies no cache freshness of its own", async () => {
    const res = await handleRelaytest(get("ttl"), "ttl");

    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBeNull();
    expect(res.headers.get("expires")).toBeNull();
    expect(await res.text()).toBe("relaytest: ttl v1\n");
  });
});

describe("relaytest — vary", () => {
  it("produces different bodies for different request-header values", async () => {
    const a = await handleRelaytest(
      get("vary", { "x-relaytest-variant": "a" }),
      "vary",
    );
    const b = await handleRelaytest(
      get("vary", { "x-relaytest-variant": "b" }),
      "vary",
    );

    expect(a.headers.get("vary")).toBe("x-relaytest-variant");
    expect(b.headers.get("vary")).toBe("x-relaytest-variant");
    expect(await a.text()).toBe("relaytest: vary a\n");
    expect(await b.text()).toBe("relaytest: vary b\n");
    expect(a.headers.get("etag")).not.toBe(b.headers.get("etag"));
  });

  it("answers a conditional request for the same variant with 304", async () => {
    const first = await handleRelaytest(
      get("vary", { "x-relaytest-variant": "a" }),
      "vary",
    );
    const etag = first.headers.get("etag") as string;

    const res = await handleRelaytest(
      get("vary", { "x-relaytest-variant": "a", "if-none-match": etag }),
      "vary",
    );

    expect(res.status).toBe(304);
  });
});

describe("relaytest — encoding", () => {
  it("serves identity when that is all the client accepts", async () => {
    const res = await handleRelaytest(
      get("encoding", { "accept-encoding": "identity" }),
      "encoding",
    );

    expect(res.headers.get("vary")).toBe("accept-encoding");
    expect(res.headers.get("content-encoding")).toBeNull();
    expect(await res.text()).toBe("relaytest: encoding v1\n");
  });

  it("serves gzip when the client accepts it, and it round-trips", async () => {
    const res = await handleRelaytest(
      get("encoding", { "accept-encoding": "gzip" }),
      "encoding",
    );

    expect(res.headers.get("vary")).toBe("accept-encoding");
    expect(res.headers.get("content-encoding")).toBe("gzip");
    expect(res.headers.get("etag")).toBe('"encoding-gzip-v1"');

    const plain = await new Response(
      res.body!.pipeThrough(new DecompressionStream("gzip")),
    ).text();
    expect(plain).toBe("relaytest: encoding v1\n");
  });

  it("rejects a request that accepts no encoding", async () => {
    const res = await handleRelaytest(
      get("encoding", { "accept-encoding": "identity;q=0, gzip;q=0" }),
      "encoding",
    );

    expect(res.status).toBe(406);
  });

  it("answers HEAD with the same 406 and no body", async () => {
    const headers = { "accept-encoding": "identity;q=0, gzip;q=0" };
    const getRes = await handleRelaytest(get("encoding", headers), "encoding");
    const headRes = await handleRelaytest(head("encoding", headers), "encoding");

    expect(getRes.status).toBe(406);
    expect(headRes.status).toBe(406);
    expect(headRes.headers.get("vary")).toBe(getRes.headers.get("vary"));
    expect(await headRes.text()).toBe("");
  });
});

describe("relaytest — range", () => {
  const body = () =>
    Uint8Array.from({ length: 256 }, (_, i) => i);

  it("serves the full fixed body without a Range", async () => {
    const res = await handleRelaytest(get("range"), "range");
    const bytes = new Uint8Array(await res.arrayBuffer());

    expect(res.status).toBe(200);
    expect(res.headers.get("accept-ranges")).toBe("bytes");
    expect(res.headers.get("content-length")).toBe("256");
    expect(bytes).toEqual(body());
  });

  it("serves a single bounded range as 206", async () => {
    const res = await handleRelaytest(
      get("range", { range: "bytes=10-19" }),
      "range",
    );
    const bytes = new Uint8Array(await res.arrayBuffer());

    expect(res.status).toBe(206);
    expect(res.headers.get("content-range")).toBe("bytes 10-19/256");
    expect(res.headers.get("content-length")).toBe("10");
    expect(bytes).toEqual(body().slice(10, 20));
  });

  it("supports open-ended and suffix ranges", async () => {
    const open = await handleRelaytest(get("range", { range: "bytes=250-" }), "range");
    expect(open.status).toBe(206);
    expect(new Uint8Array(await open.arrayBuffer())).toEqual(body().slice(250));

    const suffix = await handleRelaytest(get("range", { range: "bytes=-4" }), "range");
    expect(suffix.status).toBe(206);
    expect(suffix.headers.get("content-range")).toBe("bytes 252-255/256");
    expect(new Uint8Array(await suffix.arrayBuffer())).toEqual(body().slice(252));
  });

  it("answers an unsatisfiable range with 416", async () => {
    const res = await handleRelaytest(
      get("range", { range: "bytes=999-" }),
      "range",
    );

    expect(res.status).toBe(416);
    expect(res.headers.get("content-range")).toBe("bytes */256");
    expect(await res.text()).toBe("");
  });

  it.each([
    "bytes=abc-def",
    "bytes=",
    "bytes=1-2-3",
    "bytes=1.5-2",
    "items=0-3",
  ])("ignores a syntactically invalid Range (%s) and serves 200", async (range) => {
    const res = await handleRelaytest(get("range", { range }), "range");
    const bytes = new Uint8Array(await res.arrayBuffer());

    expect(res.status).toBe(200);
    expect(res.headers.get("content-range")).toBeNull();
    expect(bytes).toEqual(body());
  });

  it("ignores Range when If-Range names another validator", async () => {
    const res = await handleRelaytest(
      get("range", { range: "bytes=0-9", "if-range": '"other"' }),
      "range",
    );

    expect(res.status).toBe(200);
    expect(res.headers.get("content-length")).toBe("256");
  });

  it("describes the full body for HEAD even with a Range", async () => {
    const res = await handleRelaytest(
      head("range", { range: "bytes=0-9" }),
      "range",
    );

    expect(res.status).toBe(200);
    expect(res.headers.get("content-length")).toBe("256");
    expect(await res.text()).toBe("");
  });
});

describe("relaytest — no-store and digest", () => {
  it("marks no-store with the relay control header", async () => {
    const res = await handleRelaytest(get("no-store"), "no-store");

    expect(res.headers.get("x-mdb-relay")).toBe("no-store");
    expect(res.headers.get("cache-control")).toBeNull();
  });

  it("returns digest headers matching the body bytes", async () => {
    const res = await handleRelaytest(get("digest"), "digest");
    const body = await res.arrayBuffer();
    const digestBytes = new Uint8Array(await crypto.subtle.digest("SHA-256", body));
    const sha256 = hex(digestBytes.buffer);
    const base64 = btoa(String.fromCharCode(...digestBytes));

    expect(res.headers.get("docker-content-digest")).toBe(`sha256:${sha256}`);
    expect(res.headers.get("content-digest")).toBe(`sha-256=:${base64}:`);
    expect(res.headers.get("digest")).toBe(`sha-256=${base64}`);
  });
});

describe("relaytest — routing edges", () => {
  it("404s an unknown case", async () => {
    const res = await handleRelaytest(get("nope"), "nope");

    expect(res.status).toBe(404);
    expect(res.headers.get("cache-control")).toBe("no-store");
  });

  it("rejects methods other than GET and HEAD", async () => {
    const res = await handleRelaytest(
      new Request("https://mirs.uk/relaytest/fresh", { method: "POST" }),
      "fresh",
    );

    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toBe("GET, HEAD");
  });

  it("is never advertised as a mirror", () => {
    const manifest = buildMirrorManifest();
    for (const name of CASES) {
      expect(manifest.mirrors.some((m) => m.path === name)).toBe(false);
      expect(manifest.mirrors.some((m) => m.name === name)).toBe(false);
    }
    expect(manifest.mirrors.some((m) => m.path === "relaytest")).toBe(false);
  });
});
