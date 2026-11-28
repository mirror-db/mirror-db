import { beforeEach, describe, expect, it, vi } from "vitest";

import { aptMirrors } from "@server/mirrors/apt-mirrors";
import { sanitizeResponse } from "@server/pkgs/fetch";
import { env } from "@server/test-stubs/cloudflare-workers";

/**
 * Front-door wiring: the Worker must normalise a relay control header on the
 * way in and inject mirror directives after the mirror (and sanitisation) has
 * produced the response.
 */

const observed = vi.hoisted(() => ({ requests: [] as Request[] }));

vi.mock("./relay-control", async (importOriginal) => {
  const mod = await importOriginal<typeof import("./relay-control")>();
  return {
    ...mod,
    normalizeRelayRequest: (request: Request) => {
      const out = mod.normalizeRelayRequest(request);
      observed.requests.push(out);
      return out;
    },
  };
});

const assets = { fetch: vi.fn(async () => new Response(null, { status: 404 })) };

async function loadWorker() {
  env.BASE_DOMAIN = "mirs.uk";
  (env as any).ASSETS = assets;
  return (await import("./index")).default;
}

const req = (path: string, headers: Record<string, string> = {}) =>
  new Request(`https://mirs.uk${path}`, { headers }) as unknown as Parameters<
    Awaited<ReturnType<typeof loadWorker>>["fetch"]
  >[0];

beforeEach(() => {
  observed.requests = [];
  assets.fetch.mockReset();
  assets.fetch.mockImplementation(async () => new Response(null, { status: 404 }));
});

describe("front door — relay request cleaning", () => {
  it("strips a direct client's X-MDB-Relay before routing", async () => {
    const worker = await loadWorker();
    await worker.fetch(req("/relaytest/fresh", { "x-mdb-relay": "no-store" }));

    const seen = observed.requests.at(-1);
    expect(seen).toBeDefined();
    expect(seen!.headers.has("x-mdb-relay")).toBe(false);
  });

  it("keeps it when the relay-host marker is present", async () => {
    const worker = await loadWorker();
    await worker.fetch(
      req("/relaytest/fresh", {
        "x-mdb-relay": "no-store",
        "x-mdb-relay-host": "relay.example.com",
      }),
    );

    expect(observed.requests.at(-1)?.headers.get("x-mdb-relay")).toBe("no-store");
  });
});

describe("front door — mirror directive injection", () => {
  it("sanitises an upstream directive, then injects APT no-vary", async () => {
    const { serveMirror } = await import("./index");
    const apt = aptMirrors.find((mirror) => mirror.name === "debian");
    expect(apt).toBeDefined();

    // Stub only the upstream call: the response still passes through the real
    // sanitizer, as an APT mirror's fetch would.
    const mirror = {
      ...apt!,
      fetch: async () =>
        sanitizeResponse(
          new Response("deb", {
            status: 200,
            headers: { "content-length": "3", "x-mdb-relay": "no-store" },
          }),
        ),
    };

    const res = await serveMirror(
      mirror,
      new Request("https://mirs.uk/debian/pool/main/n/nginx/nginx_1.0_amd64.deb"),
      { relativePath: "pool/main/n/nginx/nginx_1.0_amd64.deb" },
    );

    expect(res.headers.get("x-mdb-relay")).toBe("no-vary");
  });

  it("leaves the relaytest no-store response unsanitised", async () => {
    const worker = await loadWorker();
    const res = await worker.fetch(req("/relaytest/no-store"));

    expect(res.headers.get("x-mdb-relay")).toBe("no-store");
  });
});
