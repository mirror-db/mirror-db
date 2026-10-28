import { describe, expect, it } from "vitest";
import { buildRequest } from "./fetch";

describe("buildRequest", () => {
  it("preserves method when no init provided", () => {
    const original = new Request("https://example.com/path", { method: "HEAD" });
    const built = buildRequest(original);
    expect(built.method).toBe("HEAD");
  });

  it("preserves headers when no init provided", () => {
    const original = new Request("https://example.com/path", {
      headers: { "Range": "bytes=0-10", "X-Custom": "value" },
    });
    const built = buildRequest(original);
    expect(built.headers.get("range")).toBe("bytes=0-10");
    expect(built.headers.get("x-custom")).toBe("value");
  });

  it("preserves method and headers when joining parts", () => {
    const original = new Request("https://example.com", {
      method: "HEAD",
      headers: { "Range": "bytes=0-10" },
    });
    const built = buildRequest(original, ["api", "v1"]);
    expect(built.method).toBe("HEAD");
    expect(built.headers.get("range")).toBe("bytes=0-10");
    expect(new URL(built.url).pathname).toBe("/api/v1");
  });

  it("allows init to override method", () => {
    const original = new Request("https://example.com", { method: "HEAD" });
    const built = buildRequest(original, { method: "GET" });
    expect(built.method).toBe("GET");
  });

  it("allows init to override headers", () => {
    const original = new Request("https://example.com", {
      headers: { "Range": "bytes=0-10" },
    });
    const built = buildRequest(original, { headers: { "Accept": "text/html" } });
    expect(built.headers.has("range")).toBe(false);
    expect(built.headers.get("accept")).toBe("text/html");
  });

  it("defaults redirect to follow", () => {
    const original = new Request("https://example.com");
    const built = buildRequest(original);
    expect(built.redirect).toBe("follow");
  });

  it("allows parts + init override", () => {
    const original = new Request("https://example.com", {
      method: "HEAD",
      headers: { "Range": "bytes=0-10" },
    });
    const built = buildRequest(original, ["api"], { method: "POST" });
    expect(built.method).toBe("POST");
    expect(built.headers.get("range")).toBe("bytes=0-10");
    expect(new URL(built.url).pathname).toBe("/api");
  });
});
