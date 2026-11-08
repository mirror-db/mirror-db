import { describe, expect, it, vi, beforeEach } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({ cachedfetch: vi.fn() }));

import { keys } from "./index";
import { debianKeyName2Url } from "./pgp/debian";
import { repo2KeyUrl } from "./pgp/well-known-apt";
import { ParseSSHKey } from "./utils/ssh-key";
import { toSourcesFormat } from "./utils/pgp-key";
import { UnknownKeyError } from "./utils/errors";
import { cachedfetch } from "@server/pkgs/fetch";

const mockEzfetch = cachedfetch as ReturnType<typeof vi.fn>;

function fetchedUrl(callIndex = 0): string {
  const arg = mockEzfetch.mock.calls[callIndex][0];
  return arg instanceof URL ? arg.toString() : String(arg);
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("debianKeyName2Url", () => {
  it("maps codename to archive key", () => {
    expect(debianKeyName2Url("bookworm").toString()).toBe("https://ftp-master.debian.org/keys/archive-key-12.asc");
  });

  it("accepts a numeric version", () => {
    expect(debianKeyName2Url("13").toString()).toBe("https://ftp-master.debian.org/keys/archive-key-13.asc");
  });

  it("handles -security and -release variants", () => {
    expect(debianKeyName2Url("bookworm-security").toString()).toBe(
      "https://ftp-master.debian.org/keys/archive-key-12-security.asc",
    );
    expect(debianKeyName2Url("bookworm-release").toString()).toBe("https://ftp-master.debian.org/keys/release-12.asc");
  });

  it("rejects unknown / too-old versions", () => {
    expect(() => debianKeyName2Url("nope")).toThrow();
    expect(() => debianKeyName2Url("7")).toThrow();
  });
});

describe("repo2KeyUrl", () => {
  it("resolves a known repo", () => {
    expect(repo2KeyUrl("hashicorp")).toBe("https://apt.releases.hashicorp.com/gpg");
  });

  it("throws on unknown repo", () => {
    expect(() => repo2KeyUrl("does-not-exist")).toThrow();
  });
});

describe("ParseSSHKey", () => {
  it("parses type, comment and fingerprints", async () => {
    // ssh-ed25519 key payload (32-byte body), arbitrary but valid base64.
    const b64 = "AAAAC3NzaC1lZDI1NTE5AAAAILb8u3Lm6dGZ4N0pYh0o2u3kQ8h0qY2yq3sQ4nQ1aB2";
    const key = `ssh-ed25519 ${b64} user@host`;
    const parsed = await ParseSSHKey(key);

    expect(parsed.type).toBe("ed25519");
    expect(parsed.comment).toBe("user@host");
    expect(parsed.fingerprint.md5).toMatch(/^([0-9a-f]{2}:)+[0-9a-f]{2}$/);
    expect(parsed.fingerprint.sha256.length).toBeGreaterThan(0);
  });

  it("throws on invalid base64 body", async () => {
    await expect(ParseSSHKey("ssh-rsa !!!notb64 c")).rejects.toThrow();
  });
});

describe("toSourcesFormat", () => {
  // Equivalent to `sed -e 's/^$/./' -e '1!s/^/ /'`.
  it("indents continuation lines and turns blank lines into '.'", () => {
    const armored = [
      "-----BEGIN PGP PUBLIC KEY BLOCK-----",
      "",
      "mQINBF5dI2sBEACy",
      "=abcd",
      "-----END PGP PUBLIC KEY BLOCK-----",
      "",
    ].join("\n");

    expect(toSourcesFormat(armored)).toBe(
      [
        "-----BEGIN PGP PUBLIC KEY BLOCK-----",
        " .",
        " mQINBF5dI2sBEACy",
        " =abcd",
        " -----END PGP PUBLIC KEY BLOCK-----",
      ].join("\n"),
    );
  });

  it("does not leave a trailing '.' line", () => {
    // A trailing newline is not an empty line for sed either.
    expect(toSourcesFormat("BEGIN\n\nEND\n")).toBe("BEGIN\n .\n END");
  });
});

describe("well-known APT keys cover the APT mirrors", () => {
  // Every APT mount should be resolvable from `/keys/apt/<name>`, so a client
  // can write one sources file per repo without consulting a table.
  const noKey = new Set(["debian-ports"]);

  it("resolves a key for every apt mirror", async () => {
    const { aptMirrors } = await import("@server/mirrors/apt-mirrors");
    for (const mirror of aptMirrors) {
      if (noKey.has(mirror.name)) continue;
      expect(() => repo2KeyUrl(mirror.name), mirror.name).not.toThrow();
    }
  });

  it("documents the exception", () => {
    // debian-ports publishes its archive key only inside a .deb keyring package.
    expect(() => repo2KeyUrl("debian-ports")).toThrow(UnknownKeyError);
  });

  it("maps the NVIDIA container repo to its signing key", () => {
    expect(repo2KeyUrl("libnvidia-container")).toBe(
      "https://nvidia.github.io/libnvidia-container/gpgkey",
    );
  });

  it("merges Debian's release keys, because the index is signed by all of them", () => {
    const urls = repo2KeyUrl("debian");
    expect(Array.isArray(urls)).toBe(true);
    expect(urls).toContain("https://ftp-master.debian.org/keys/archive-key-12.asc");
    expect(urls).toContain("https://ftp-master.debian.org/keys/archive-key-13.asc");
    expect(urls).toContain("https://ftp-master.debian.org/keys/release-13.asc");
  });
});

describe("keys app routing", () => {
  function get(path: string): Promise<Response> {
    return Promise.resolve(keys.fetch(new Request(`https://mirs.uk${path}`)));
  }

  it("proxies SSH key list and joins raw keys", async () => {
    const raw =
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILb8u3Lm6dGZ4N0pYh0o2u3kQ8h0qY2yq3sQ4nQ1aB2 a\n" +
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMb8u3Lm6dGZ4N0pYh0o2u3kQ8h0qY2yq3sQ4nQ1aB3 b\n";
    mockEzfetch.mockResolvedValue(new Response(raw));

    const res = await get("/keys/gh/octocat");
    expect(fetchedUrl()).toBe("https://github.com/octocat.keys");
    const body = await res.text();
    expect(body.split("\n")).toHaveLength(2);
  });

  it("gitlab route hits gitlab.com", async () => {
    mockEzfetch.mockResolvedValue(new Response(""));
    await get("/keys/gitlab/someone");
    expect(fetchedUrl()).toBe("https://gitlab.com/someone.keys");
  });

  it("serves a binary key instead of failing to parse it", async () => {
    // syncthing.net publishes a binary key file. Parsing it used to throw
    // (`readKey` was called with `armoredKey`), surfacing as a 500 for a key apt
    // clients need in order to trust the repository.
    const binary = new Uint8Array([0xc6, 0x00, 0x00]); // not parseable on purpose
    // A fresh Response per call: a body can only be read once.
    mockEzfetch.mockImplementation(
      async () =>
        new Response(binary, { headers: { "content-type": "application/octet-stream" } }),
    );

    const res = await get("/keys/apt/hashicorp");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("application/pgp-keys");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(binary);

    // Metadata needs a parse; an unparseable key answers 502 rather than 500.
    const info = await get("/keys/apt/hashicorp/info");
    expect(info.status).toBe(502);
  });

  it("merges every certificate of a multi-source key into one keyring", async () => {
    // Syncthing signs with two certificates and publishes only one of them; the
    // mirror serves both so `sqv` (and therefore apt) accepts the index.
    const first = new Uint8Array([0xc6, 0x00, 0x01]);
    const second = new Uint8Array([0xc6, 0x00, 0x02]);
    mockEzfetch
      .mockImplementationOnce(async () => new Response(first))
      .mockImplementationOnce(async () => new Response(second));

    const res = await get("/keys/apt/syncthing");
    expect(res.status).toBe(200);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(
      new Uint8Array([0xc6, 0x00, 0x01, 0xc6, 0x00, 0x02]),
    );
    expect(fetchedUrl(0)).toBe("https://syncthing.net/release-key.gpg");
    expect(fetchedUrl(1)).toContain("keyserver.ubuntu.com/pks/lookup");
  });

  it("apt key route resolves via well-known table", async () => {
    // Minimal armored block; openpgp parse failure surfaces as a 500, but the
    // upstream URL resolution is what we assert here.
    mockEzfetch.mockResolvedValue(new Response("not-a-real-key"));
    await get("/keys/apt/hashicorp").catch(() => {});
    expect(fetchedUrl()).toBe("https://apt.releases.hashicorp.com/gpg");
  });

  it("answers 502 for /in-sources when the key cannot be parsed", async () => {
    mockEzfetch.mockImplementation(async () => new Response("not-a-real-key"));
    const res = await get("/keys/apt/gitlab-runner/in-sources");
    expect(res.status).toBe(502);
    expect(fetchedUrl()).toBe("https://packages.gitlab.com/gpgkey/gpg.key");
  });

  it("debian route resolves codename to archive key URL", async () => {
    mockEzfetch.mockResolvedValue(new Response("not-a-real-key"));
    await get("/keys/debian/bookworm").catch(() => {});
    expect(fetchedUrl()).toBe("https://ftp-master.debian.org/keys/archive-key-12.asc");
  });
});
