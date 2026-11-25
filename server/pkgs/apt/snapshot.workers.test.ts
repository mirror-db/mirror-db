import { describe, expect, it } from "vitest";

import { digestHex } from "./metadata";
import { AptSnapshotIndex } from "./snapshot";

/**
 * Real gzip through the Workers `DecompressionStream`. The Node project covers
 * the parser with the same built-in, but this is the runtime that actually
 * serves it — a polyfill difference would silently disable `Packages.gz`.
 */

const BASE = "https://up.example/repo/";

async function gzip(text: string): Promise<Uint8Array> {
  const body = new Response(new TextEncoder().encode(text)).body!;
  return new Uint8Array(await new Response(body.pipeThrough(new CompressionStream("gzip"))).arrayBuffer());
}

describe("APT snapshot — gzip in workerd", () => {
  it("parses a gzip Packages index and synthesises its directories", async () => {
    const plain = [
      "Package: nginx",
      "Version: 1.0",
      "Filename: pool/main/n/nginx/nginx_1.0_amd64.deb",
      "Size: 4096",
      "",
      "",
    ].join("\n");
    const compressed = await gzip(plain);

    const release = [
      "Suite: bookworm",
      "Codename: bookworm",
      "Architectures: amd64",
      "Components: main",
      "SHA256:",
      ` ${await digestHex("SHA-256", compressed)} ${compressed.byteLength} main/binary-amd64/Packages.gz`,
      "",
    ].join("\n");

    const routes = new Map<string, string | Uint8Array>([
      [`${BASE}dists/bookworm/InRelease`, release],
      [`${BASE}dists/bookworm/main/binary-amd64/Packages.gz`, compressed],
    ]);
    const fetchImpl = async (input: string | URL | Request): Promise<Response> => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      const body = routes.get(url);
      return body === undefined
        ? new Response(null, { status: 404 })
        : new Response(body);
    };

    const index = new AptSnapshotIndex({
      mirror: "test",
      base: BASE,
      suites: ["bookworm"],
      fetchImpl,
    });
    const snapshot = await index.awaitSnapshot();

    expect(snapshot).not.toBeNull();
    expect(snapshot!.files.find((f) => f.path === "pool/main/n/nginx/nginx_1.0_amd64.deb"))
      .toMatchObject({ size: 4096, kind: "package" });
    expect(snapshot!.readdir("pool/main/n/nginx/")).toEqual([
      expect.objectContaining({ name: "nginx_1.0_amd64.deb", type: "file", size: 4096 }),
    ]);
  });
});
