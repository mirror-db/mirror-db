import { describe, expect, it } from "vitest";

import type { Credentials } from "./creds";
import {
  getToken,
  proxyRegistryRequest,
  type FetchImpl,
} from "./registry-proxy";
import { parseWwwAuthenticate } from "./www-authenticate";

const CHALLENGE =
  'Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"';

interface Call {
  url: string;
  init?: RequestInit;
}

/** Build a mock fetch from a URL→Response handler, recording every call. */
function mockFetch(
  handler: (url: string, init: RequestInit | undefined, n: number) => Response,
): { fetchImpl: FetchImpl; calls: Call[] } {
  const calls: Call[] = [];
  const fetchImpl: FetchImpl = async (input, init) => {
    const url = typeof input === "string" ? input : input.toString();
    calls.push({ url, init });
    return handler(url, init, calls.length - 1);
  };
  return { fetchImpl, calls };
}

function authHeader(init: RequestInit | undefined): string | null {
  return new Headers(init?.headers).get("authorization");
}

const req = (path: string, init?: RequestInit) =>
  new Request(`https://dcr.mirs.uk${path}`, init);

describe("proxyRegistryRequest", () => {
  it("passes through an anonymous public pull (first 200)", async () => {
    const { fetchImpl, calls } = mockFetch(
      () => new Response("manifest-body", { status: 200, headers: { "content-type": "application/json" } }),
    );

    const res = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(await res.text()).toBe("manifest-body");
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe(
      "https://registry-1.docker.io/v2/library/nginx/manifests/latest",
    );
  });

  it("handles 401 → token → retry with Bearer", async () => {
    const { fetchImpl, calls } = mockFetch((url, _init, n) => {
      if (n === 0) {
        return new Response(null, {
          status: 401,
          headers: { "www-authenticate": CHALLENGE },
        });
      }
      if (url.startsWith("https://auth.docker.io/token")) {
        return new Response(JSON.stringify({ token: "TKN" }), { status: 200 });
      }
      return new Response("ok", { status: 200 });
    });

    const res = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(calls).toHaveLength(3);
    // Token request carries the service + scope from the challenge.
    const tokenUrl = new URL(calls[1].url);
    expect(tokenUrl.searchParams.get("service")).toBe("registry.docker.io");
    expect(tokenUrl.searchParams.get("scope")).toBe("repository:library/nginx:pull");
    // Retried registry request carries the bearer token.
    expect(authHeader(calls[2].init)).toBe("Bearer TKN");
  });

  it("uses Basic auth when credentials are available", async () => {
    const creds: Credentials = { username: "alice", password: "pat-secret" };
    const { fetchImpl, calls } = mockFetch((url, _init, n) => {
      if (n === 0) {
        return new Response(null, { status: 401, headers: { "www-authenticate": CHALLENGE } });
      }
      if (url.startsWith("https://auth.docker.io/token")) {
        return new Response(JSON.stringify({ token: "TKN" }), { status: 200 });
      }
      return new Response("ok", { status: 200 });
    });

    await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      getCredentials: async () => creds,
    });

    expect(authHeader(calls[1].init)).toBe(`Basic ${btoa("alice:pat-secret")}`);
  });

  it("never returns a WWW-Authenticate challenge to the client", async () => {
    // Upstream stays 401 even after the (anonymous) token attempt fails.
    const { fetchImpl } = mockFetch((url) => {
      if (url.startsWith("https://auth.docker.io/token")) {
        return new Response("denied", { status: 403 });
      }
      return new Response(null, { status: 401, headers: { "www-authenticate": CHALLENGE } });
    });

    const res = await proxyRegistryRequest(req("/v2/private/repo/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.headers.get("www-authenticate")).toBeNull();
  });

  it("follows a relative redirect server-side, keeping auth on the same host", async () => {
    const { fetchImpl, calls } = mockFetch((url, _init, n) => {
      if (n === 0) {
        return new Response(null, { status: 401, headers: { "www-authenticate": CHALLENGE } });
      }
      if (url.startsWith("https://auth.docker.io/token")) {
        return new Response(JSON.stringify({ token: "TKN" }), { status: 200 });
      }
      if (url === "https://registry-1.docker.io/v2/library/nginx/blobs/sha256:abc") {
        // Relative redirect to an on-host download path.
        return new Response(null, { status: 307, headers: { location: "/downloads/blob-1" } });
      }
      return new Response("BLOBDATA", { status: 200 });
    });

    const res = await proxyRegistryRequest(req("/v2/library/nginx/blobs/sha256:abc"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.status).toBe(200);
    expect(await res.text()).toBe("BLOBDATA");
    // Final hop resolved against the upstream host and carried the bearer.
    const last = calls[calls.length - 1];
    expect(last.url).toBe("https://registry-1.docker.io/downloads/blob-1");
    expect(authHeader(last.init)).toBe("Bearer TKN");
  });

  it("drops auth when a redirect points to a different (CDN) host", async () => {
    const { fetchImpl, calls } = mockFetch((url) => {
      if (url === "https://registry-1.docker.io/v2/library/nginx/blobs/sha256:abc") {
        return new Response(null, {
          status: 307,
          headers: { location: "https://cdn.example.com/signed/blob" },
        });
      }
      return new Response("BLOBDATA", { status: 200 });
    });

    const res = await proxyRegistryRequest(req("/v2/library/nginx/blobs/sha256:abc"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.status).toBe(200);
    const last = calls[calls.length - 1];
    expect(last.url).toBe("https://cdn.example.com/signed/blob");
    expect(authHeader(last.init)).toBeNull();
  });

  it("does not forward the client's Authorization header upstream", async () => {
    const { fetchImpl, calls } = mockFetch(() => new Response("ok", { status: 200 }));

    await proxyRegistryRequest(
      req("/v2/library/nginx/manifests/latest", {
        headers: { authorization: "Bearer client-supplied" },
      }),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    expect(authHeader(calls[0].init)).toBeNull();
  });
});

describe("getToken", () => {
  it("returns the token and sends Basic auth for credentials", async () => {
    const { fetchImpl, calls } = mockFetch(
      () => new Response(JSON.stringify({ access_token: "AT" }), { status: 200 }),
    );
    const challenge = parseWwwAuthenticate(CHALLENGE)!;

    const token = await getToken(
      challenge,
      { username: "bob", password: "pw" },
      fetchImpl,
    );

    expect(token).toBe("AT");
    expect(authHeader(calls[0].init)).toBe(`Basic ${btoa("bob:pw")}`);
  });

  it("returns null when the token endpoint rejects", async () => {
    const { fetchImpl } = mockFetch(() => new Response("no", { status: 401 }));
    const challenge = parseWwwAuthenticate(CHALLENGE)!;
    expect(await getToken(challenge, null, fetchImpl)).toBeNull();
  });
});
