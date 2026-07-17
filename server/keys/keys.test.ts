import { describe, expect, it, vi, beforeEach } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({ cachedfetch: vi.fn() }));

import { keys } from "./index";
import { debianKeyName2Url } from "./pgp/debian";
import { repo2KeyUrl } from "./pgp/well-known-apt";
import { ParseSSHKey } from "./utils/ssh-key";
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

  it("apt key route resolves via well-known table", async () => {
    // Minimal armored block; openpgp parse failure surfaces as a 500, but the
    // upstream URL resolution is what we assert here.
    mockEzfetch.mockResolvedValue(new Response("not-a-real-key"));
    await get("/keys/apt/hashicorp").catch(() => {});
    expect(fetchedUrl()).toBe("https://apt.releases.hashicorp.com/gpg");
  });

  it("debian route resolves codename to archive key URL", async () => {
    mockEzfetch.mockResolvedValue(new Response("not-a-real-key"));
    await get("/keys/debian/bookworm").catch(() => {});
    expect(fetchedUrl()).toBe("https://ftp-master.debian.org/keys/archive-key-12.asc");
  });
});
