import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({ cachedfetch: vi.fn() }));

import { env } from "@server/test-stubs/cloudflare-workers";
import { cachedfetch } from "@server/pkgs/fetch";

import { conf } from "@server/conf";

const mockFetch = cachedfetch as unknown as ReturnType<typeof vi.fn>;

const ARMORED =
  "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBF5dI2sBEACy\n-----END PGP PUBLIC KEY BLOCK-----\n";

const get = (
  path: string,
  headers: Record<string, string> = {},
  method = "GET",
): Promise<Response> =>
  Promise.resolve(
    conf.fetch(new Request(`https://mirs.uk${path}`, { method, headers })),
  );

beforeEach(() => {
  vi.clearAllMocks();
  env.BASE_DOMAIN = "mirs.uk";
  mockFetch.mockResolvedValue(new Response(ARMORED));
});

describe("/conf/apt/ — listing", () => {
  it("returns every registered path as JSON for non-browser clients", async () => {
    const res = await get("/conf/apt/", { "user-agent": "curl/8.5.0" });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");

    const body = (await res.json()) as { sources: { name: string; path: string }[] };
    expect(body.sources.length).toBeGreaterThan(10);
    expect(body.sources).toContainEqual({
      name: "debian-bookworm",
      path: "/conf/apt/debian-bookworm.sources",
    });
    expect(body.sources).toContainEqual({
      name: "ubuntu-26.10",
      path: "/conf/apt/ubuntu-26.10.sources",
    });

    const names = body.sources.map((source) => source.name);
    expect(names).toEqual([...names].sort());
    // Security is a stanza inside each Debian release, not a second path.
    expect(names).not.toContain("debian-security-bookworm");
  });

  it("treats a missing user agent as a non-browser client", async () => {
    const res = await get("/conf/apt/");
    expect(res.headers.get("content-type")).toContain("application/json");
  });

  it("accepts the listing path without a trailing slash", async () => {
    const res = await get("/conf/apt", { "user-agent": "curl/8.5.0" });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");
  });

  it("returns a linked <pre> list for browser user agents", async () => {
    for (const userAgent of [
      "Mozilla/5.0 (X11; Linux x86_64)",
      "Mozilla/5.0 Chrome/131.0.0.0 Safari/537.36",
      "Mozilla/5.0 (Macintosh) Safari/605.1.15",
      "Mozilla/5.0 Firefox/133.0",
      "Mozilla/5.0 Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
    ]) {
      const res = await get("/conf/apt/", { "user-agent": userAgent });
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type")).toContain("text/html");
      const html = await res.text();
      expect(html).toContain("<pre>");
      expect(html).toContain(
        '<a href="/conf/apt/debian-12.sources">debian-12.sources</a>',
      );
      expect(html).toContain(
        '<a href="/conf/apt/ubuntu-jammy.sources">ubuntu-jammy.sources</a>',
      );
    }
  });
});

describe("/conf/apt/<name>.sources", () => {
  it("serves a rendered source as text/plain with an explicit charset", async () => {
    const res = await get("/conf/apt/debian-12.sources");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(await res.text()).toBe(
      [
        "Types: deb",
        "URIs: https://mirs.uk/debian/",
        "Suites: bookworm bookworm-updates bookworm-backports",
        "Components: main contrib non-free non-free-firmware",
        "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg",
        "",
        "Types: deb",
        "URIs: https://mirs.uk/debian-security/",
        "Suites: bookworm-security",
        "Components: main contrib non-free non-free-firmware",
        "Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg",
        "",
      ].join("\n"),
    );
  });

  it("answers HEAD with the same headers and no body", async () => {
    const res = await get("/conf/apt/debian-12.sources", {}, "HEAD");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(await res.text()).toBe("");
  });

  it("renders aliases identically", async () => {
    const version = await (await get("/conf/apt/debian-12.sources")).text();
    const codename = await (await get("/conf/apt/debian-bookworm.sources")).text();
    expect(version).toBe(codename);
  });

  it("inlines a third-party key from the key proxy", async () => {
    const body = await (await get("/conf/apt/mise.sources")).text();
    expect(body).toContain("Signed-By: -----BEGIN PGP PUBLIC KEY BLOCK-----");
    expect(body).toContain("\n .\n");
    expect(body).toContain("\n -----END PGP PUBLIC KEY BLOCK-----\n");
  });

  it("404s an unknown preset", async () => {
    const res = await get("/conf/apt/debian.sources");
    expect(res.status).toBe(404);
    expect(res.headers.get("content-type")).toContain("application/json");
  });

  it("404s anything below a preset path", async () => {
    expect((await get("/conf/apt/debian-12.sources/extra")).status).toBe(404);
    expect((await get("/conf/other")).status).toBe(404);
  });

  it("rejects methods other than GET/HEAD", async () => {
    const res = await get("/conf/apt/debian-12.sources", {}, "POST");
    expect(res.status).toBe(405);
  });
});

describe("/conf/apt/<name>.sources — query handling", () => {
  it("applies suite, component, and architecture overrides", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources?suite=trixie&comp=main&arch=arm64")
    ).text();
    expect(body).toContain("Suites: trixie\n");
    expect(body).toContain("Components: main\n");
    expect(body).toContain("Architectures: arm64\n");
  });

  it("answers 400 JSON for conflicting aliases", async () => {
    const res = await get("/conf/apt/debian-12.sources?suite=bookworm&suites=trixie");
    expect(res.status).toBe(400);
    expect(((await res.json()) as { error: string }).error).toMatch(/conflicting/);
  });

  it("answers 400 JSON for an unknown parameter", async () => {
    const res = await get("/conf/apt/debian-12.sources?trusted=yes");
    expect(res.status).toBe(400);
    expect(((await res.json()) as { error: string }).error).toMatch(/unknown query/);
  });

  it("answers 400 JSON for an empty suite or architecture", async () => {
    expect((await get("/conf/apt/debian-12.sources?suites=")).status).toBe(400);
    expect((await get("/conf/apt/debian-12.sources?arch=")).status).toBe(400);
  });

  it("answers 400 JSON for a value that would inject a field", async () => {
    const res = await get(
      "/conf/apt/debian-12.sources?suite=bookworm%0ASigned-By:%20/tmp/evil.gpg",
    );
    expect(res.status).toBe(400);
  });

  it("keeps the security suite fixed when suites are overridden", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources?suites=bookworm")
    ).text();
    expect(body).toContain("Suites: bookworm\n");
    expect(body).toContain("Suites: bookworm-security\n");
  });

  it("answers 400 JSON for a conflicting protocol", async () => {
    const res = await get(
      "/conf/apt/debian-12.sources?protocol=http&proto=https",
    );
    expect(res.status).toBe(400);
    expect(((await res.json()) as { error: string }).error).toMatch(
      /conflicting protocol/,
    );
  });

  it("answers 400 JSON for an invalid protocol", async () => {
    for (const search of [
      "protocol=",
      "proto=ftp",
      "protocol=http,https",
    ]) {
      const res = await get(`/conf/apt/debian-12.sources?${search}`);
      expect(res.status).toBe(400);
      expect(res.headers.get("content-type")).toContain("application/json");
    }
  });
});

describe("/conf/apt/<name>.sources — protocol", () => {
  it("defaults every URI to HTTPS even on an HTTP request URL", async () => {
    const res = await conf.fetch(
      new Request("http://mirs.uk/conf/apt/debian-12.sources"),
    );
    const body = await res.text();
    expect(body).toContain("URIs: https://mirs.uk/debian/\n");
    expect(body).toContain("URIs: https://mirs.uk/debian-security/\n");
    expect(body).not.toContain("URIs: http://");
  });

  it("keeps HTTPS through a relay host", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources", {
        "x-mdb-relay-host": "relay.example.com",
      })
    ).text();
    expect(body).toContain("URIs: https://relay.example.com/debian/\n");
    expect(body).toContain(
      "URIs: https://relay.example.com/debian-security/\n",
    );
  });

  it("switches every stanza URI to HTTP for ?proto=http", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources?proto=http", {
        "x-mdb-relay-host": "relay.example.com",
      })
    ).text();
    expect(body).toContain("URIs: http://relay.example.com/debian/\n");
    expect(body).toContain(
      "URIs: http://relay.example.com/debian-security/\n",
    );
    expect(body).not.toContain("URIs: https://");
  });

  it("applies the scheme to third-party inline-key presets too", async () => {
    const body = await (await get("/conf/apt/mise.sources?protocol=http")).text();
    expect(body).toContain("URIs: http://mirs.uk/mise/\n");
  });
});

describe("/conf/apt/<name>.sources — origins and failures", () => {
  it("builds URIs from the relay host the client reached", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources", {
        "x-mdb-relay-host": "relay.example.com",
      })
    ).text();
    expect(body).toContain("URIs: https://relay.example.com/debian/\n");
  });

  it("ignores a relay header that is not a bare hostname", async () => {
    const body = await (
      await get("/conf/apt/debian-12.sources", {
        "x-mdb-relay-host": "evil.example/../x",
      })
    ).text();
    expect(body).toContain("URIs: https://mirs.uk/debian/\n");
  });

  it("answers 502 when the signing key publisher fails", async () => {
    mockFetch.mockResolvedValue(new Response("upstream down", { status: 503 }));
    const res = await get("/conf/apt/mise.sources");
    expect(res.status).toBe(502);
    const body = (await res.json()) as { error: string };
    expect(body.error).toMatch(/signing key/);
  });
});
