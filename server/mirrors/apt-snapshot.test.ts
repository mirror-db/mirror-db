import { beforeEach, describe, expect, it, vi } from "vitest";

import { withRelayDirectives } from "@server/relay-control";

import { aptConfigs } from "./apt-mirrors";
import { createAptMirror } from "./apt-mirror";
import { digestHex } from "@server/pkgs/apt/metadata";

/**
 * Snapshot-backed APT mounts, end to end: an upstream with files but no
 * directory listing still gets browsable HTML/JSON/WebDAV directories and the
 * fixed `/.mdb/apt-index.json` endpoint, while files keep streaming upstream.
 */

vi.mock("cloudflare:workers", () => ({
  env: {
    ASSETS: {
      fetch: vi.fn(() =>
        Promise.resolve(
          new Response(
            "<html><head><title><!--title--></title></head><body><!--app-html--></body></html>",
          ),
        ),
      ),
    },
    BASE_DOMAIN: "mirs.uk",
  },
}));

const { cachedfetchMock, ezfetchMock } = vi.hoisted(() => ({
  cachedfetchMock: vi.fn(),
  ezfetchMock: vi.fn(),
}));
vi.mock("@server/pkgs/fetch", async (importOriginal) => {
  const original: any = await importOriginal();
  return {
    ...original,
    cachedfetch: cachedfetchMock,
    ezfetch: ezfetchMock,
  };
});

const BASE = "https://up.example/repo/";
const MOUNT = "https://mirs.uk/hashicorp/";

const routes = new Map<string, string | Uint8Array>();

function respond(input: unknown): Response {
  const url = typeof input === "string" ? input : (input as Request).url ?? String(input);
  const body = routes.get(url);
  return body === undefined ? new Response(null, { status: 404 }) : new Response(body);
}

const packages = [
  "Package: terraform",
  "Filename: pool/main/t/terraform/terraform_1.0_amd64.deb",
  "Size: 2048",
  `SHA256: ${"c".repeat(64)}`,
  "",
  "",
].join("\n");

async function makeMirror() {
  const encoded = new TextEncoder().encode(packages);
  routes.set(`${BASE}dists/bookworm/InRelease`, [
    "Suite: bookworm",
    "Codename: bookworm",
    "Date: Sat, 21 Nov 2026 11:15:03 UTC",
    "Architectures: amd64",
    "Components: main",
    "SHA256:",
    ` ${await digestHex("SHA-256", encoded)} ${encoded.byteLength} main/binary-amd64/Packages`,
    "",
  ].join("\n"));
  routes.set(`${BASE}dists/bookworm/main/binary-amd64/Packages`, encoded);
  routes.set(`${BASE}pool/main/t/terraform/terraform_1.0_amd64.deb`, "deb-bytes");

  return createAptMirror({
    name: "hashicorp",
    base: BASE,
    suites: ["bookworm"],
    snapshot: { fetchImpl: async (input) => respond(input) },
  });
}

const req = (path: string, init?: RequestInit) => new Request(`${MOUNT}${path}`, init);

beforeEach(() => {
  routes.clear();
  cachedfetchMock.mockReset();
  ezfetchMock.mockReset();
  cachedfetchMock.mockImplementation(async (input: unknown) => respond(input));
  ezfetchMock.mockImplementation(async (input: unknown) => respond(input));
});

describe("snapshot APT mounts — configuration", () => {
  it("enables snapshots on exactly the five listing-less repositories", () => {
    const enabled = aptConfigs.filter((config) => config.snapshot).map((config) => config.name);
    expect(enabled).toEqual([
      "mise",
      "syncthing-pkgs",
      "hashicorp",
      "opentofu",
      "github-cli",
    ]);
  });

  it("every snapshot mount declares a non-empty suite list", () => {
    for (const config of aptConfigs.filter((entry) => entry.snapshot)) {
      expect(config.suites?.length, config.name).toBeGreaterThan(0);
    }
  });

  it("refuses a snapshot mount without suites at construction", () => {
    expect(() =>
      createAptMirror({ name: "nosuites", base: BASE, snapshot: true }),
    ).toThrow(/requires an explicit non-empty suites list/);
    expect(() =>
      createAptMirror({ name: "emptysuites", base: BASE, suites: [], snapshot: true }),
    ).toThrow(/requires an explicit non-empty suites list/);
  });
});

describe("snapshot APT mounts — synthetic directories", () => {
  it("renders the mount root as HTML", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req(""));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/html");
    expect(await res.text()).toContain("dists/");
  });

  it("answers the JSON mode at every level", async () => {
    const mirror = await makeMirror();
    const root: any = await (await mirror.fetch(req("?format=json"))).json();
    expect(root.path).toBe("");
    expect(root.entries.map((e: any) => e.name)).toEqual(["dists", "pool"]);

    const suite: any = await (
      await mirror.fetch(req("dists/bookworm/?format=json"))
    ).json();
    expect(suite.entries.map((e: any) => e.name)).toContain("InRelease");

    const pool: any = await (
      await mirror.fetch(req("pool/main/t/terraform/?format=json"))
    ).json();
    expect(pool.entries).toEqual([
      expect.objectContaining({ name: "terraform_1.0_amd64.deb", size: 2048 }),
    ]);
  });

  it("answers PROPFIND with the same entries", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req("", { method: "PROPFIND", headers: { depth: "1" } }));

    expect(res.status).toBe(207);
    const xml = await res.text();
    expect(xml).toContain("<D:multistatus");
    expect(xml).toContain("dists/");
    expect(xml).toContain("pool/");
  });

  it("404s a directory the metadata does not define", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req("pool/nope/?format=json"));
    expect(res.status).toBe(404);
  });

  it("lets a known suite's other signed names reach upstream", async () => {
    const mirror = await makeMirror();
    routes.set(`${BASE}dists/bookworm/Release`, "unsigned-release");
    routes.set(`${BASE}dists/bookworm/Release.gpg`, "detached-signature");

    for (const [name, body, directives] of [
      ["Release", "unsigned-release", "no-store, prefetch"],
      ["Release.gpg", "detached-signature", "no-store"],
    ] as const) {
      const res = await mirror.fetch(req(`dists/bookworm/${name}`));
      expect(res.status, name).toBe(200);
      expect(await res.text(), name).toBe(body);
      // Still a mutable dists object: the relay must not store it, and only the
      // release itself is worth refreshing.
      expect(res.headers.get("x-mdb-relay"), name).toBe(directives);
    }
  });

  it("still 404s an unknown path inside a known suite", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req("dists/bookworm/ghost"));

    expect(res.status).toBe(404);
  });

  it("redirects the mount root without a trailing slash", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(new Request("https://mirs.uk/hashicorp"));

    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("/hashicorp/");
  });

  it("redirects a pool directory without a trailing slash", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req("pool/main/t/terraform"));

    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("/hashicorp/pool/main/t/terraform/");
  });
});

describe("snapshot APT mounts — fixed index endpoint", () => {
  it("serves the generation as JSON with a strong ETag", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req(".mdb/apt-index.json"));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("application/json");
    expect(res.headers.get("etag")).toMatch(/^"[0-9a-f]{64}"$/);
    expect(res.headers.get("cache-control")).toMatch(/max-age=\d+/);

    const doc: any = await res.json();
    expect(doc.schema).toBe(1);
    expect(doc.mirror).toBe("hashicorp");
    expect(doc.suites[0]).toMatchObject({
      suite: "bookworm",
      release_path: "dists/bookworm/InRelease",
    });
    expect(doc.files).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          path: "pool/main/t/terraform/terraform_1.0_amd64.deb",
          size: 2048,
          kind: "package",
        }),
        expect.objectContaining({
          path: "dists/bookworm/main/binary-amd64/Packages",
          kind: "metadata",
        }),
      ]),
    );
  });

  it("answers HEAD with headers and no body", async () => {
    const mirror = await makeMirror();
    const get = await mirror.fetch(req(".mdb/apt-index.json"));
    const head = await mirror.fetch(req(".mdb/apt-index.json", { method: "HEAD" }));

    expect(head.status).toBe(200);
    expect(head.headers.get("etag")).toBe(get.headers.get("etag"));
    expect(await head.text()).toBe("");
  });

  it("honours If-None-Match with 304", async () => {
    const mirror = await makeMirror();
    const get = await mirror.fetch(req(".mdb/apt-index.json"));
    const etag = get.headers.get("etag")!;

    const cached = await mirror.fetch(
      req(".mdb/apt-index.json", { headers: { "if-none-match": etag } }),
    );
    expect(cached.status).toBe(304);
    expect(cached.headers.get("etag")).toBe(etag);
  });

  it("404s other paths under .mdb/", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req(".mdb/other.json"));
    expect(res.status).toBe(404);
  });

  it("answers 502 for the endpoint and directories when no generation exists", async () => {
    const mirror = createAptMirror({
      name: "hashicorp",
      base: BASE,
      suites: ["bookworm"],
      snapshot: { fetchImpl: async () => new Response(null, { status: 404 }) },
    });

    const endpoint = await mirror.fetch(req(".mdb/apt-index.json"));
    expect(endpoint.status).toBe(502);
    expect(await endpoint.text()).toContain("unavailable");

    const directory = await mirror.fetch(req("?format=json"));
    expect(directory.status).toBe(502);
  });
});

describe("snapshot APT mounts — file passthrough and relay directives", () => {
  it("marks Release/InRelease no-store + prefetch", async () => {
    const mirror = await makeMirror();
    const res = await mirror.fetch(req("dists/bookworm/InRelease"));

    expect(res.status).toBe(200);
    expect(res.headers.get("x-mdb-relay")).toBe("no-store, prefetch");
    // Freshness comes from the generation's own expiry, not a second resolve.
    const maxAge = Number(res.headers.get("cache-control")?.match(/max-age=(\d+)/)?.[1]);
    expect(maxAge).toBeGreaterThan(3500);
    // The front door adds no-vary without dropping either directive.
    expect(withRelayDirectives(res, ["no-vary"]).headers.get("x-mdb-relay")).toBe(
      "no-store, prefetch, no-vary",
    );
  });

  it("does not prefetch Packages or pool payloads", async () => {
    const mirror = await makeMirror();

    const index = await mirror.fetch(req("dists/bookworm/main/binary-amd64/Packages"));
    expect(index.headers.get("x-mdb-relay")).toBe("no-store");

    const pool = await mirror.fetch(req("pool/main/t/terraform/terraform_1.0_amd64.deb"));
    expect(pool.headers.get("x-mdb-relay")).toBeNull();
    expect(await pool.text()).toBe("deb-bytes");
  });
});
