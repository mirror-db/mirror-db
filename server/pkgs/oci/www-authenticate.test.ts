import { describe, expect, it } from "vitest";

import { parseWwwAuthenticate } from "./www-authenticate";

describe("parseWwwAuthenticate", () => {
  it("parses a full Docker Hub Bearer challenge", () => {
    const header =
      'Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"';
    expect(parseWwwAuthenticate(header)).toEqual({
      scheme: "Bearer",
      params: {
        realm: "https://auth.docker.io/token",
        service: "registry.docker.io",
        scope: "repository:library/nginx:pull",
      },
    });
  });

  it("parses a challenge without a scope (e.g. /v2/ version check)", () => {
    const header = 'Bearer realm="https://auth.docker.io/token",service="registry.docker.io"';
    const result = parseWwwAuthenticate(header);
    expect(result?.params.scope).toBeUndefined();
    expect(result?.params.realm).toBe("https://auth.docker.io/token");
  });

  it("returns null for missing or garbage input", () => {
    expect(parseWwwAuthenticate(null)).toBeNull();
    expect(parseWwwAuthenticate("")).toBeNull();
    expect(parseWwwAuthenticate("Bearer")).toBeNull();
    expect(parseWwwAuthenticate("NotAChallenge")).toBeNull();
  });
});
