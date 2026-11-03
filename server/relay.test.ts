import { describe, expect, it } from "vitest";

import { env } from "@server/test-stubs/cloudflare-workers";

import { normalizeRelayHost, relayInfo } from "./relay";

const withRelay = (value: string) =>
  new Request("https://mirs.uk/npm/x", { headers: { "x-mdb-relay-host": value } });

describe("normalizeRelayHost", () => {
  it("accepts a bare hostname, normalising case and a trailing dot", () => {
    expect(normalizeRelayHost("Relay.Example.COM")).toBe("relay.example.com");
    expect(normalizeRelayHost("relay.example.com.")).toBe("relay.example.com");
    expect(normalizeRelayHost("  relay.example.com  ")).toBe("relay.example.com");
  });

  it("rejects anything that is not a bare hostname", () => {
    for (const bad of [
      "",
      "evil.com/../x",
      "relay.example.com:8443",
      "user@evil.com",
      "https://evil.com",
      "evil..com",
      "-leading.example.com",
      "a".repeat(300),
      "evil.com?x=1",
      "evil.com#frag",
    ]) {
      expect(normalizeRelayHost(bad), bad).toBeUndefined();
    }
  });
});

describe("relayInfo", () => {
  it("uses the relay host for both the base domain and the origin", () => {
    env.BASE_DOMAIN = "mirs.uk";
    const info = relayInfo(withRelay("relay.example.com"));
    expect(info).toEqual({
      baseDomain: "relay.example.com",
      origin: "https://relay.example.com",
    });
  });

  it("falls back to this request's own origin without the header", () => {
    env.BASE_DOMAIN = "mirs.uk";
    const info = relayInfo(new Request("https://dcr.mirs.uk/v2/"));
    expect(info).toEqual({ baseDomain: "mirs.uk", origin: "https://dcr.mirs.uk" });
  });

  it("ignores a malformed relay host instead of building a URL from it", () => {
    env.BASE_DOMAIN = "mirs.uk";
    const info = relayInfo(withRelay("evil.com/../x"));
    expect(info.baseDomain).toBe("mirs.uk");
    expect(info.origin).toBe("https://mirs.uk");
  });
});
