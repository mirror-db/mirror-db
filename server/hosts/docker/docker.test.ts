import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { env } from "@server/test-stubs/cloudflare-workers";
import { handle, KV_KEY, normalizeDockerPath } from "./docker";

/** Invoke the host handler with a Request for the given path. */
const call = (path: string, init?: RequestInit) =>
  handle(new Request(`http://dcr.localhost${path}`, init));

/** Minimal in-memory KV mock. */
function makeKv() {
  const store = new Map<string, string>();
  return {
    store,
    get: async (k: string) => store.get(k) ?? null,
    put: async (k: string, v: string) => void store.set(k, v),
  };
}

const USER = "tester";
const PASS = "dckr_pat_SUPER_SECRET";

beforeEach(() => {
  env.kv = makeKv();
  env.DOCKER_HUB_USER = USER;
  env.DOCKER_HUB_PASSWORD = PASS;
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("normalizeDockerPath", () => {
  it("prefixes single-segment (official) repos with library/", () => {
    expect(normalizeDockerPath("/v2/nginx/manifests/latest")).toBe(
      "/v2/library/nginx/manifests/latest",
    );
    expect(normalizeDockerPath("/v2/redis/blobs/sha256:abc")).toBe(
      "/v2/library/redis/blobs/sha256:abc",
    );
  });

  it("leaves namespaced repos and non-repo paths unchanged", () => {
    expect(normalizeDockerPath("/v2/bitnami/nginx/manifests/latest")).toBe(
      "/v2/bitnami/nginx/manifests/latest",
    );
    expect(normalizeDockerPath("/v2/")).toBe("/v2/");
    expect(normalizeDockerPath("/v2/_catalog")).toBe("/v2/_catalog");
  });
});

describe("POST /auth/login", () => {
  it("rejects without the admin token", async () => {
    const res = await call("/auth/login", { method: "POST" });
    expect(res.status).toBe(401);
  });

  it("stores credentials in KV and never leaks the password", async () => {
    // validateCredentials() hits the token endpoint — accept it.
    vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 200 })));

    const res = await call("/auth/login", {
      method: "POST",
      headers: { "x-admin-token": PASS },
    });

    expect(res.status).toBe(200);
    const text = await res.text();
    expect(text).toContain(USER);
    // The password must never appear in the response.
    expect(text).not.toContain(PASS);

    // Credentials persisted under the expected key.
    const stored = JSON.parse((env.kv.store as Map<string, string>).get(KV_KEY)!);
    expect(stored).toEqual({ username: USER, password: PASS });
  });

  it("returns 401 when Docker Hub rejects the credentials", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("nope", { status: 401 })));

    const res = await call("/auth/login", {
      method: "POST",
      headers: { "x-admin-token": PASS },
    });

    expect(res.status).toBe(401);
    expect(env.kv.store.has(KV_KEY)).toBe(false);
  });
});

describe("/v2 proxy routing", () => {
  it("normalizes official-image paths and forwards to Docker Hub", async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: string | URL | Request) => {
        calls.push(typeof input === "string" ? input : input.toString());
        return new Response("ok", { status: 200 });
      }),
    );

    const res = await call("/v2/nginx/manifests/latest");

    expect(res.status).toBe(200);
    expect(calls[0]).toBe(
      "https://registry-1.docker.io/v2/library/nginx/manifests/latest",
    );
  });
});
