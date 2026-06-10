import { spawn, type ChildProcess } from "node:child_process";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

/**
 * End-to-end test against the real worker running on Cloudflare's edge.
 *
 * Docker Hub is unreachable from this network, so the worker is launched with
 * `wrangler dev --remote` (executes on Cloudflare, real outbound network).
 * `--host dcr.mirs.uk` makes the worker see that Host, so the subdomain router
 * in server/index.ts dispatches to the Docker proxy. The whole lifecycle lives
 * here: `beforeAll` builds the worker and boots remote dev; `afterAll` tears it
 * down. Tests then hit real Docker Hub through the proxy with a plain `fetch`.
 */

const PORT = 8799;
const HOST = "dcr.mirs.uk";
const BASE = `http://localhost:${PORT}`;

const MANIFEST_ACCEPT = [
  "application/vnd.oci.image.index.v1+json",
  "application/vnd.docker.distribution.manifest.list.v2+json",
  "application/vnd.oci.image.manifest.v1+json",
  "application/vnd.docker.distribution.manifest.v2+json",
].join(", ");

let server: ChildProcess | undefined;

/** Run a command to completion, rejecting on a non-zero exit. */
function run(cmd: string, args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: "inherit", env: process.env });
    child.on("error", reject);
    child.on("exit", (code: number | null) =>
      code === 0 ? resolve() : reject(new Error(`${cmd} ${args.join(" ")} exited ${code}`)),
    );
  });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Poll the dev server until it answers or time out. */
async function waitForReady(timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      await fetch(`${BASE}/`, { signal: AbortSignal.timeout(2000) });
      return;
    } catch {
      await sleep(1000);
    }
  }
  throw new Error(`dev server not ready on :${PORT}`);
}

describe("docker proxy e2e (real Docker Hub via wrangler --remote)", () => {
  beforeAll(async () => {
    await run("pnpm", ["run", "build"]);
    // `--host` makes the worker see this Host so the subdomain router reaches
    // the Docker proxy (otherwise wrangler uses the route zone `mirs.uk`).
    server = spawn(
      "pnpm",
      ["exec", "wrangler", "dev", "--remote", "--host", HOST, "--port", String(PORT)],
      { stdio: "inherit", detached: true, env: process.env },
    );
    await waitForReady(120_000);
  }, 300_000);

  afterAll(() => {
    if (server?.pid) {
      try {
        process.kill(-server.pid, "SIGTERM");
      } catch {
        // already gone
      }
    }
  });

  it("GET /v2/ advertises the registry without a login challenge", async () => {
    const res = await fetch(`${BASE}/v2/`);
    expect(res.status).toBe(200);
    expect(res.headers.get("www-authenticate")).toBeNull();
  });

  it("pulls a real manifest for an official image (normalized to library/)", async () => {
    const res = await fetch(`${BASE}/v2/hello-world/manifests/latest`, {
      headers: { accept: MANIFEST_ACCEPT },
    });

    expect(res.status).toBe(200);
    expect(res.headers.get("www-authenticate")).toBeNull();
    expect(res.headers.get("content-type") ?? "").toContain("json");

    const manifest = (await res.json()) as {
      schemaVersion?: number;
      manifests?: unknown[];
      layers?: unknown[];
    };
    expect(manifest.schemaVersion).toBe(2);
    expect(Array.isArray(manifest.manifests) || Array.isArray(manifest.layers)).toBe(true);
  });

  it("HEAD on a manifest returns a content digest", async () => {
    const res = await fetch(`${BASE}/v2/hello-world/manifests/latest`, {
      method: "HEAD",
      headers: { accept: MANIFEST_ACCEPT },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get("docker-content-digest")).toMatch(/^sha256:/);
  });
});
