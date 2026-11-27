import { describe, expect, it } from "vitest";

import type { Credentials } from "./creds";
import {
  getToken,
  proxyRegistryRequest,
  type CacheLike,
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

  it("strips Set-Cookie but delivers Vary", async () => {
    const { fetchImpl } = mockFetch(
      () =>
        new Response("ok", {
          status: 200,
          headers: {
            "set-cookie": "session=secret; HttpOnly",
            vary: "Accept",
          },
        }),
    );

    const res = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(res.headers.get("set-cookie")).toBeNull();
    // The relay needs Vary to build its variants; the Worker only drops it from
    // the Cache API copy (which refuses to store a response carrying Vary).
    expect(res.headers.get("vary")).toBe("Accept");
  });

  it("does not store Vary in the Cache API copy", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("LAYER", {
          status: 200,
          headers: { "content-length": "5", vary: "Accept" },
        }),
    );

    await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });

    const entry = [...store.values()][0];
    expect(entry).toBeDefined();
    expect(entry.headers.get("vary")).toBeNull();
  });

  it("never caches a Set-Cookie header", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("LAYER", {
          status: 200,
          headers: {
            "content-length": "5",
            "set-cookie": "session=secret; HttpOnly",
          },
        }),
    );

    await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });

    const entry = [...store.values()][0];
    expect(entry).toBeDefined();
    expect(entry.headers.get("set-cookie")).toBeNull();
  });

  it("does not forward relay control headers upstream", async () => {
    const { fetchImpl, calls } = mockFetch(() => new Response("ok", { status: 200 }));

    await proxyRegistryRequest(
      req("/v2/library/nginx/manifests/latest", {
        headers: {
          "x-mdb-relay": "no-store",
          "x-mdb-relay-host": "relay.example.com",
          "x-keep": "yes",
        },
      }),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    const headers = new Headers(calls[0].init?.headers);
    expect(headers.has("x-mdb-relay")).toBe(false);
    expect(headers.has("x-mdb-relay-host")).toBe(false);
    expect(headers.get("x-keep")).toBe("yes");
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

/** In-memory Cache API mock keyed by URL string. */
function mockCache(): { cache: CacheLike; store: Map<string, Response> } {
  const store = new Map<string, Response>();
  const key = (r: string | URL | Request) =>
    typeof r === "string" ? r : r.toString();
  const cache: CacheLike = {
    async match(r) {
      return store.get(key(r));
    },
    async put(r, res) {
      store.set(key(r), res);
    },
  };
  return { cache, store };
}

const DIGEST = "sha256:" + "a".repeat(64);

describe("proxyRegistryRequest caching", () => {
  it("caches a content-addressed blob and serves the second hit from cache", async () => {
    const { cache } = mockCache();
    const { fetchImpl, calls } = mockFetch(
      () =>
        new Response("BLOBDATA", {
          status: 200,
          headers: { "content-length": "8" },
        }),
    );

    const opts = { upstream: "registry-1.docker.io", fetchImpl, cache };

    const first = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`),
      opts,
    );
    expect(await first.text()).toBe("BLOBDATA");
    expect(first.headers.get("cache-control")).toContain("immutable");
    expect(calls).toHaveLength(1);

    const second = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`),
      opts,
    );
    expect(await second.text()).toBe("BLOBDATA");
    // No new upstream fetch — served from cache.
    expect(calls).toHaveLength(1);
  });

  it("shares a cached layer across repositories by digest", async () => {
    const { cache } = mockCache();
    const { fetchImpl, calls } = mockFetch(
      () =>
        new Response("LAYER", {
          status: 200,
          headers: { "content-length": "5" },
        }),
    );
    const opts = { upstream: "registry-1.docker.io", fetchImpl, cache };

    // First repo populates the cache for this digest.
    await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), opts);
    expect(calls).toHaveLength(1);

    // A different repo referencing the same digest hits the same entry.
    const res = await proxyRegistryRequest(
      req(`/v2/library/node/blobs/${DIGEST}`),
      opts,
    );
    expect(await res.text()).toBe("LAYER");
    expect(calls).toHaveLength(1); // no second upstream fetch
  });

  it("skips caching a blob larger than the size limit but still advertises immutable", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("BIG", {
          status: 200,
          headers: {
            "content-length": String(2 * 1024 * 1024 * 1024),
            "cache-control": "no-store, no-transform",
          },
        }),
    );

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });
    expect(store.size).toBe(0);
    // Delivery still states the digest's immutable freshness even when the
    // Worker cannot cache the body; the orthogonal directive survives.
    const cacheControl = res.headers.get("cache-control")!;
    expect(cacheControl).toContain("public");
    expect(cacheControl).toContain("max-age=31536000");
    expect(cacheControl).toContain("immutable");
    expect(cacheControl).toContain("no-transform");
    expect(cacheControl).not.toContain("no-store");
  });

  it("skips caching when Content-Length is unknown but still advertises immutable", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(() => new Response("data", { status: 200 }));

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });
    expect(store.size).toBe(0);
    expect(res.headers.get("cache-control")).toContain("immutable");
  });

  it("advertises immutable on a HEAD of a digest-addressed blob", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response(null, {
          status: 200,
          headers: { "content-length": "5" },
        }),
    );

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`, { method: "HEAD" }), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toContain("immutable");
    expect(store.size).toBe(0);
  });

  it("does not cache a mutable tag manifest", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl, calls } = mockFetch(
      () => new Response("manifest", { status: 200 }),
    );
    const opts = { upstream: "registry-1.docker.io", fetchImpl, cache };

    const first = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), opts);
    const second = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), opts);

    expect(store.size).toBe(0);
    expect(calls).toHaveLength(2);
    // A tag is mutable: freshness is not forced on it.
    expect(first.headers.get("cache-control")).toBeNull();
    expect(second.headers.get("cache-control")).toBeNull();
  });

  it("does not force immutable onto a non-200 digest response", async () => {
    const { fetchImpl } = mockFetch(
      () => new Response("missing", { status: 404 }),
    );

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });
    expect(res.status).toBe(404);
    expect(res.headers.get("cache-control")).toBeNull();
  });

  it("caches a manifest fetched by digest", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("manifest", {
          status: 200,
          headers: { "content-length": "8" },
        }),
    );

    await proxyRegistryRequest(req(`/v2/library/nginx/manifests/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
    });
    expect(store.size).toBe(1);
  });

  it("a ranged digest GET bypasses a hot cache entry and reaches upstream for a 206", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl, calls } = mockFetch((_url, init) => {
      const range = new Headers(init?.headers).get("range");
      if (range) {
        return new Response("CDATA", {
          status: 206,
          headers: { "content-range": "bytes 2-6/8", "content-length": "5" },
        });
      }
      return new Response("BLOBDATA", {
        status: 200,
        headers: { "content-length": "8" },
      });
    });
    const opts = { upstream: "registry-1.docker.io", fetchImpl, cache };

    await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), opts);
    expect(store.size).toBe(1);

    const ranged = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`, { headers: { range: "bytes=2-6" } }),
      opts,
    );

    // The stored copy is a complete 200 and must not answer a Range request.
    expect(ranged.status).toBe(206);
    expect(await ranged.text()).toBe("CDATA");
    expect(ranged.headers.get("content-range")).toBe("bytes 2-6/8");
    expect(calls).toHaveLength(2);
    expect(new Headers(calls[1].init?.headers).get("range")).toBe("bytes=2-6");
    // No second entry: the digest cache still holds only the complete object.
    expect(store.size).toBe(1);
  });

  it("never stores a 206 answered to a ranged digest request", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("CDATA", {
          status: 206,
          headers: { "content-range": "bytes 2-6/8", "content-length": "5" },
        }),
    );

    const res = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`, { headers: { range: "bytes=2-6" } }),
      { upstream: "registry-1.docker.io", fetchImpl, cache },
    );

    expect(res.status).toBe(206);
    expect(store.size).toBe(0);
  });

  it("forces identity on a digest request but leaves tag negotiation alone", async () => {
    const { fetchImpl, calls } = mockFetch(() => new Response("ok", { status: 200 }));
    const opts = { upstream: "registry-1.docker.io", fetchImpl };

    await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`, {
        headers: { "accept-encoding": "gzip" },
      }),
      opts,
    );
    await proxyRegistryRequest(
      req("/v2/library/nginx/manifests/latest", {
        headers: { "accept-encoding": "gzip" },
      }),
      opts,
    );

    // One digest names one byte sequence, so its representation cannot depend on
    // what the client can decode.
    expect(new Headers(calls[0].init?.headers).get("accept-encoding")).toBe("identity");
    // A tag manifest is negotiated normally.
    expect(new Headers(calls[1].init?.headers).get("accept-encoding")).toBe("gzip");
  });

  it("schedules the cache write via waitUntil when provided", async () => {
    const { cache } = mockCache();
    const { fetchImpl } = mockFetch(
      () =>
        new Response("X", {
          status: 200,
          headers: { "content-length": "1" },
        }),
    );
    const scheduled: Promise<unknown>[] = [];

    await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
      cache,
      waitUntil: (p) => scheduled.push(p),
    });

    expect(scheduled).toHaveLength(1);
    await Promise.all(scheduled);
  });
});

describe("proxyRegistryRequest — relay directives", () => {
  const SHA256 = "a".repeat(64);
  const SHA1 = "b".repeat(40);
  const SHA512 = "c".repeat(128);
  const relay = (res: Response) => res.headers.get("x-mdb-relay");

  it("declares immutable and the path digest on a digest blob 200", async () => {
    const { fetchImpl } = mockFetch(
      () =>
        new Response("BLOB", {
          status: 200,
          headers: {
            "content-length": "4",
            etag: '"blob"',
            "docker-content-digest": DIGEST,
          },
        }),
    );

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    // The declaration comes from the request path, not from the response's
    // Docker-Content-Digest, and it does not displace any existing header.
    expect(relay(res)).toBe(`immutable, sha256=${SHA256}`);
    expect(res.headers.get("etag")).toBe('"blob"');
    expect(res.headers.get("docker-content-digest")).toBe(DIGEST);
  });

  it("declares immutable and the path digest on a 206", async () => {
    const { fetchImpl } = mockFetch(
      () =>
        new Response("CDATA", {
          status: 206,
          headers: { "content-range": "bytes 2-6/8", "content-length": "5" },
        }),
    );

    const res = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`, { headers: { range: "bytes=2-6" } }),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    expect(res.status).toBe(206);
    expect(relay(res)).toBe(`immutable, sha256=${SHA256}`);
  });

  it("declares immutable and the path digest on a 304", async () => {
    const { fetchImpl } = mockFetch(
      () => new Response(null, { status: 304, headers: { etag: '"blob"' } }),
    );

    const res = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/${DIGEST}`, {
        headers: { "if-none-match": '"blob"' },
      }),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    expect(res.status).toBe(304);
    // A revalidation still describes the same immutable bytes, so the relay's
    // own revalidation state keeps the declaration.
    expect(relay(res)).toBe(`immutable, sha256=${SHA256}`);
  });

  it("declares sha1 only when the path names a full sha1 digest", async () => {
    const { fetchImpl } = mockFetch(() => new Response("ok", { status: 200 }));

    const res = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/sha1:${SHA1}`),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    expect(relay(res)).toBe(`immutable, sha1=${SHA1}`);
  });

  it("declares only immutable for an algorithm the relay cannot address", async () => {
    const { fetchImpl } = mockFetch(() => new Response("ok", { status: 200 }));

    const res = await proxyRegistryRequest(
      req(`/v2/library/nginx/blobs/sha512:${SHA512}`),
      { upstream: "registry-1.docker.io", fetchImpl },
    );

    expect(relay(res)).toBe("immutable");
  });

  it("never declares anything on a tag manifest, even with a response digest", async () => {
    const { fetchImpl } = mockFetch(
      () =>
        new Response("manifest", {
          status: 200,
          headers: { "docker-content-digest": DIGEST, "content-length": "8" },
        }),
    );

    const res = await proxyRegistryRequest(req("/v2/library/nginx/manifests/latest"), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    // A tag is mutable: a digest header on the response is not a path fact.
    expect(relay(res)).toBeNull();
  });

  it("declares nothing on a digest-addressed error", async () => {
    const { fetchImpl } = mockFetch(() => new Response("missing", { status: 404 }));

    const res = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), {
      upstream: "registry-1.docker.io",
      fetchImpl,
    });

    expect(relay(res)).toBeNull();
  });

  it("declares on a cache hit without storing the header", async () => {
    const { cache, store } = mockCache();
    const { fetchImpl, calls } = mockFetch(
      () =>
        new Response("BLOB", {
          status: 200,
          headers: { "content-length": "4" },
        }),
    );
    const opts = { upstream: "registry-1.docker.io", fetchImpl, cache };

    const first = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), opts);
    expect(relay(first)).toBe(`immutable, sha256=${SHA256}`);

    const second = await proxyRegistryRequest(req(`/v2/library/nginx/blobs/${DIGEST}`), opts);
    expect(calls).toHaveLength(1);
    expect(relay(second)).toBe(`immutable, sha256=${SHA256}`);

    // Relay control is per delivery; the Worker's cached copy stays clean.
    const entry = [...store.values()][0]!;
    expect(entry.headers.get("x-mdb-relay")).toBeNull();
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
