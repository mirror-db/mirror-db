import { describe, expect, it } from "vitest";

import { aptMirrors } from "@server/mirrors/apt-mirrors";
import { buildMirrorManifest } from "@server/api/mirror-manifest";

import {
  mergeRelayDirectives,
  normalizeRelayRequest,
  withRelayDirectives,
  RELAY_CONTROL_HEADER,
} from "./relay-control";

const EXPECTED_APT_MIRRORS = [
  "debian",
  "debian-ports",
  "debian-security",
  "ubuntu",
  "ubuntu-ports",
  "mise",
  "syncthing-pkgs",
  "hashicorp",
  "opentofu",
  "github-cli",
  "gitlab-runner-deb",
];

describe("relay directives — merge", () => {
  it("adds directives when the response has none", () => {
    expect(mergeRelayDirectives(null, ["no-vary"])).toBe("no-vary");
    expect(mergeRelayDirectives("", ["no-store", "no-vary"])).toBe(
      "no-store, no-vary",
    );
  });

  it("keeps an existing directive instead of overriding it", () => {
    expect(mergeRelayDirectives("no-store", ["no-vary"])).toBe(
      "no-store, no-vary",
    );
    expect(mergeRelayDirectives("ttl=60", ["no-vary"])).toBe(
      "ttl=60, no-vary",
    );
  });

  it("is idempotent and keyword-aware, value or not", () => {
    expect(mergeRelayDirectives("no-vary", ["no-vary"])).toBe("no-vary");
    expect(mergeRelayDirectives("ttl=60", ["ttl=600"])).toBe("ttl=60");
    expect(
      mergeRelayDirectives("no-vary, no-store", ["no-store", "no-vary"]),
    ).toBe("no-vary, no-store");
  });

  it("never produces an empty header from empty input", () => {
    expect(mergeRelayDirectives(null, [])).toBe("");
    expect(mergeRelayDirectives("  ,  ", [])).toBe("");
  });
});

describe("relay directives — response middleware", () => {
  it("merges into a response without touching status, body or other headers", async () => {
    const response = new Response("body", {
      status: 201,
      headers: { "content-type": "text/plain", "cache-control": "no-cache" },
    });

    const wrapped = withRelayDirectives(response, ["no-vary"]);

    expect(wrapped.status).toBe(201);
    expect(wrapped.headers.get(RELAY_CONTROL_HEADER)).toBe("no-vary");
    expect(wrapped.headers.get("content-type")).toBe("text/plain");
    expect(await wrapped.text()).toBe("body");
  });

  it("does not override a directive already set on the response", () => {
    const response = new Response("x", {
      headers: { [RELAY_CONTROL_HEADER]: "no-store" },
    });

    expect(withRelayDirectives(response, ["no-vary"]).headers.get(
      RELAY_CONTROL_HEADER,
    )).toBe("no-store, no-vary");
  });

  it("returns the original response when the mirror declares nothing", () => {
    const response = new Response("x");
    expect(withRelayDirectives(response, undefined)).toBe(response);
    expect(withRelayDirectives(response, [])).toBe(response);
  });
});

describe("relay request normalization", () => {
  it("strips a client-supplied X-MDB-Relay without a relay host", () => {
    const request = new Request("https://mirs.uk/debian/", {
      headers: { [RELAY_CONTROL_HEADER]: "no-store" },
    });

    expect(normalizeRelayRequest(request).headers.has(RELAY_CONTROL_HEADER)).toBe(
      false,
    );
  });

  it("keeps it when a valid relay host marks the request as relay-originated", () => {
    const request = new Request("https://mirs.uk/debian/", {
      headers: {
        [RELAY_CONTROL_HEADER]: "no-store",
        "x-mdb-relay-host": "relay.example.com",
      },
    });

    expect(normalizeRelayRequest(request).headers.get(RELAY_CONTROL_HEADER)).toBe(
      "no-store",
    );
  });

  it("strips it when the relay host is malformed", () => {
    const request = new Request("https://mirs.uk/debian/", {
      headers: {
        [RELAY_CONTROL_HEADER]: "no-store",
        "x-mdb-relay-host": "relay.example.com:8443",
      },
    });

    expect(normalizeRelayRequest(request).headers.has(RELAY_CONTROL_HEADER)).toBe(
      false,
    );
  });

  it("leaves requests without the control header untouched", () => {
    const request = new Request("https://mirs.uk/debian/");
    expect(normalizeRelayRequest(request)).toBe(request);
  });
});

describe("relay directives — APT mounts", () => {
  it("marks all 11 createAptMirror instances `no-vary`", () => {
    expect(aptMirrors.map((m) => m.name)).toEqual(EXPECTED_APT_MIRRORS);
    expect(aptMirrors).toHaveLength(11);
    for (const mirror of aptMirrors) {
      expect(mirror.relayDirectives, mirror.name).toEqual(["no-vary"]);
    }
  });

  it("does not leak the internal field into the manifest", () => {
    const manifest = buildMirrorManifest();
    for (const name of EXPECTED_APT_MIRRORS) {
      const entry = manifest.mirrors.find((m) => m.name === name);
      expect(entry).toBeDefined();
      expect(entry).not.toHaveProperty("relayDirectives");
    }
  });
});
