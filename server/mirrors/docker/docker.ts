import { env, waitUntil } from "cloudflare:workers";

import {
  basicAuthHeader,
  getCreds,
  putCreds,
  type Credentials,
  type KvLike,
} from "@server/pkgs/oci/creds";
import {
  defaultCache,
  proxyRegistryRequest,
} from "@server/pkgs/oci/registry-proxy";
import { mdbfetch } from "@server/pkgs/fetch";

/** KV key holding the Docker Hub credentials. */
export const KV_KEY = "creds:docker-hub";
/** Docker Hub OCI registry host. */
export const UPSTREAM = "registry-1.docker.io";
/** Docker Hub token service. */
const TOKEN_SERVICE = "registry.docker.io";
const TOKEN_REALM = "https://auth.docker.io/token";
const TOKEN_CACHE_PREFIX = "token:docker-hub:";

/**
 * Normalize an official (single-segment) repository to the `library/` namespace
 * so `docker pull dcr.mirs.uk/nginx` resolves like real Docker Hub.
 * Multi-segment repos (`user/repo`) and non-repository paths pass through.
 */
export function normalizeDockerPath(path: string): string {
  const m = path.match(/^\/v2\/(.+?)\/(manifests|blobs|tags|referrers)(\/.*|$)/);
  if (!m) return path;
  const [, repo, resource, rest] = m;
  if (repo.includes("/")) return path; // already namespaced
  return `/v2/library/${repo}/${resource}${rest}`;
}

/** Read stored Docker Hub credentials, falling back to env secrets. */
async function resolveCredentials(): Promise<Credentials | null> {
  const stored = await getCreds(env.kv as unknown as KvLike, KV_KEY);
  if (stored) return stored;
  if (env.DOCKER_HUB_USER && env.DOCKER_HUB_PASSWORD) {
    return { username: env.DOCKER_HUB_USER, password: env.DOCKER_HUB_PASSWORD };
  }
  return null;
}

/**
 * Validate credentials against Docker Hub by requesting a pull token for a
 * public image. Returns true when the token endpoint accepts the credentials.
 */
async function validateCredentials(creds: Credentials): Promise<boolean> {
  const url = new URL(TOKEN_REALM);
  url.searchParams.set("service", TOKEN_SERVICE);
  url.searchParams.set("scope", "repository:library/hello-world:pull");
  const res = await mdbfetch(url.toString(), {
    headers: { Authorization: basicAuthHeader(creds) },
  });
  return res.ok;
}

const json = (body: unknown, status = 200): Response =>
  Response.json(body, { status });

/**
 * Admin: persist the env-configured Docker Hub credentials into KV.
 * Guarded by the `x-admin-token` header, which must equal DOCKER_HUB_PASSWORD
 * (the operator already knows this secret). Credentials are never echoed back.
 */
async function handleLogin(request: Request): Promise<Response> {
  const adminToken = request.headers.get("x-admin-token");
  if (!env.DOCKER_HUB_PASSWORD || adminToken !== env.DOCKER_HUB_PASSWORD) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  if (!env.DOCKER_HUB_USER) {
    return json({ ok: false, error: "DOCKER_HUB_USER not configured" }, 400);
  }

  const creds: Credentials = {
    username: env.DOCKER_HUB_USER,
    password: env.DOCKER_HUB_PASSWORD,
  };

  if (!(await validateCredentials(creds))) {
    return json({ ok: false, error: "invalid credentials" }, 401);
  }

  await putCreds(env.kv as unknown as KvLike, KV_KEY, creds);
  return json({ ok: true, username: creds.username });
}

/** Proxy a distribution-API request to Docker Hub with transparent auth. */
function proxy(request: Request): Promise<Response> {
  return proxyRegistryRequest(request, {
    upstream: UPSTREAM,
    getCredentials: resolveCredentials,
    rewritePath: normalizeDockerPath,
    kv: env.kv as unknown as KvLike,
    tokenCachePrefix: TOKEN_CACHE_PREFIX,
    cache: defaultCache(),
    waitUntil,
  });
}

/** Entry point for the `dcr` host: route by path, no framework. */
export function handle(request: Request): Promise<Response> | Response {
  const { pathname } = new URL(request.url);

  if (request.method === "POST" && pathname === "/auth/login") {
    return handleLogin(request);
  }
  if (pathname === "/v2" || pathname.startsWith("/v2/")) {
    return proxy(request);
  }
  return new Response("Not Found", { status: 404 });
}
