import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@server/pkgs/fetch", () => ({ cachedfetch: vi.fn() }));

import { cachedfetch } from "@server/pkgs/fetch";

import { loadInlineKey } from "./keys";

const mockFetch = cachedfetch as unknown as ReturnType<typeof vi.fn>;

const fetchedUrl = (index = 0): string => String(mockFetch.mock.calls[index][0]);

beforeEach(() => {
  vi.clearAllMocks();
});

describe("loadInlineKey", () => {
  it("returns the key as a deb822 value, from the key-proxy table", async () => {
    const armored =
      "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBF5dI2sBEACy\n-----END PGP PUBLIC KEY BLOCK-----\n";
    mockFetch.mockResolvedValue(new Response(armored));

    await expect(loadInlineKey("hashicorp")).resolves.toBe(
      [
        "-----BEGIN PGP PUBLIC KEY BLOCK-----",
        " .",
        " mQINBF5dI2sBEACy",
        " -----END PGP PUBLIC KEY BLOCK-----",
      ].join("\n"),
    );
    expect(fetchedUrl()).toBe("https://apt.releases.hashicorp.com/gpg");
  });

  it("fails when the upstream publisher does not answer with a key", async () => {
    mockFetch.mockResolvedValue(new Response("nope", { status: 502 }));
    await expect(loadInlineKey("mise")).rejects.toThrow(/upstream 502/);
  });

  it("fails for a key name the key proxy does not know", async () => {
    await expect(loadInlineKey("does-not-exist")).rejects.toThrow(/unknown key/);
    expect(mockFetch).not.toHaveBeenCalled();
  });
});
