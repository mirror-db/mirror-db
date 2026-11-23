import { describe, expect, it } from "vitest";

import { aptMirrors } from "@server/mirrors/apt-mirrors";
import { LARGE_ARTIFACT_PATTERN } from "@server/relay-policy";

import { api } from "./index";
import {
  buildMirrorManifest,
  etagMatches,
  type MirrorManifest,
} from "./mirror-manifest";

const MANIFEST_URL = "https://mirs.uk/api/mirrors";

/** Subdomain mounts that speak the OCI distribution API. */
const OCI_MIRRORS = [
  "dcr",
  "ghcr",
  "gcr",
  "k8s",
  "quay",
  "mcr",
  "nvcr",
  "ocr",
  "lscr",
];

describe("mirror manifest — policy tags", () => {
  it("tags every APT mirror with `apt`", () => {
    const byName = new Map(
      buildMirrorManifest().mirrors.map((m) => [m.name, m]),
    );

    expect(aptMirrors.length).toBeGreaterThan(0);
    for (const mirror of aptMirrors) {
      expect(byName.get(mirror.name)?.policy_tags).toEqual(["apt"]);
    }
  });

  it("tags Docker Hub and every simple OCI registry with `oci`", () => {
    const byName = new Map(
      buildMirrorManifest().mirrors.map((m) => [m.name, m]),
    );

    for (const name of OCI_MIRRORS) {
      expect(byName.get(name)?.policy_tags).toEqual(["oci"]);
    }
  });

  it("advertises Pigsty as a plain-HTTP path mount", () => {
    const mirrored = buildMirrorManifest().mirrors.find(
      (m) => m.name === "pigsty",
    );

    expect(mirrored).toEqual({
      name: "pigsty",
      path: "pigsty",
      keepHTTP: true,
      policy_tags: ["apt"],
    });
  });

  it("emits `policy_tags` as an array for every mirror", () => {
    const manifest = buildMirrorManifest();
    for (const mirror of manifest.mirrors) {
      expect(Array.isArray(mirror.policy_tags)).toBe(true);
    }
    // An untagged mount is an empty array, never a missing field.
    expect(
      manifest.mirrors.find((m) => m.name === "npm")?.policy_tags,
    ).toEqual([]);
  });
});

describe("mirror manifest — policies", () => {
  it("lists policies in evaluation order", () => {
    expect(buildMirrorManifest().policies.map((p) => p.name)).toEqual([
      "oci-blobs",
      "apt-packages",
      "large-artifacts",
    ]);
  });

  it("selects speed for OCI blobs and APT pool files", () => {
    const [oci, apt] = buildMirrorManifest().policies;

    expect(oci.match).toEqual({ tag: "oci", path_pattern: "^/v2/.+/blobs/" });
    expect(oci.rules).toEqual({ traffic_policy: "speed" });

    expect(apt.match).toEqual({ tag: "apt", path_prefix: "/pool/" });
    expect(apt.rules).toEqual({ traffic_policy: "speed" });
  });

  it("applies the generic suffix rule by path only", () => {
    const large = buildMirrorManifest().policies.find(
      (p) => p.name === "large-artifacts",
    );

    expect(large?.match).toEqual({ path_pattern: LARGE_ARTIFACT_PATTERN });
    expect(large?.rules).toEqual({ traffic_policy: "speed" });
  });

  it("has no catch-all rule that would override an earlier speed selection", () => {
    // Last-match-win merging means a trailing `match: {}` latency rule would
    // undo every earlier speed rule. The relay's own default covers the miss.
    const policies = buildMirrorManifest().policies;
    expect(policies.at(-1)?.name).toBe("large-artifacts");
    expect(policies.some((p) => Object.keys(p.match).length === 0)).toBe(false);
  });

  it("uses only known traffic policies", () => {
    for (const policy of buildMirrorManifest().policies) {
      expect(["latency", "speed"]).toContain(policy.rules.traffic_policy);
    }
  });

  it("never puts response-side or per-mirror cache decisions in policy", () => {
    for (const policy of buildMirrorManifest().policies) {
      expect(policy.rules).not.toHaveProperty("cache");
      expect(policy.rules).not.toHaveProperty("cache_ttl");
      expect(policy.rules).not.toHaveProperty("no-store");
      expect(policy.rules).not.toHaveProperty("no-vary");
      expect(policy.rules).not.toHaveProperty("no-transform");
    }
  });

  it("does not expose internal relay directives on a mirror entry", () => {
    for (const mirror of buildMirrorManifest().mirrors) {
      expect(mirror).not.toHaveProperty("relayDirectives");
    }
  });
});

describe("large-artifact pattern", () => {
  const matches = (path: string) => new RegExp(LARGE_ARTIFACT_PATTERN).test(path);

  it.each([
    "/debian-cd/12.9.0/amd64/iso-cd/debian-12.9.0-amd64-netinst.iso",
    "/ubuntu-releases/24.04/ubuntu-24.04.1-live-server-amd64.iso",
    "/go-releases/go1.24.0.linux-amd64.tar.gz",
    "/nodejs/v22.12.0/node-v22.12.0-linux-x64.tar.xz",
    "/nodejs/v22.12.0/node-v22.12.0-linux-x64.tar.zst",
    "/some/file.tar.bz2",
    "/some/file.tbz2",
    "/some/file.txz",
    "/some/file.tgz",
    "/some/file.tar",
    "/some/file.zip",
    "/some/file.7z",
    "/some/file.img",
    "/some/file.squashfs",
    "/some/file.bin",
    "/pool/main/n/nginx/nginx_1.26.0-1_amd64.deb",
    "/pool/main/n/nginx/nginx-dbgsym_1.26.0-1_amd64.ddeb",
    "/dists/bookworm/main/installer-amd64/current/images/netboot/udeb/foo.udeb",
    "/rpm/x86_64/package-1.0-1.x86_64.rpm",
    "/alpine/v3.20/main/x86_64/APKINDEX.apk",
    "/gh-proxy/releases/download/v1/tool.exe",
    "/gh-proxy/releases/download/v1/tool.msi",
    "/gh-proxy/releases/download/v1/tool.dmg",
    "/gh-proxy/releases/download/v1/tool.pkg",
  ])("matches %s", (path) => {
    expect(matches(path)).toBe(true);
  });

  it.each([
    // A bare `.gz` must not match: `Packages.gz` is a small, latency-sensitive index.
    "/dists/bookworm/main/binary-amd64/Packages.gz",
    "/dists/bookworm/main/binary-amd64/Packages.xz",
    "/dists/bookworm/Release.gz",
    "/some/file.gz",
    "/some/file.tar.gz.asc",
    "/some/file.tar.gz.sig",
    "/some/file.zip.sha256",
    "/some/file.iso.md5",
    "/some/file",
    "/",
  ])("does not match %s", (path) => {
    expect(matches(path)).toBe(false);
  });

  it("is case-sensitive", () => {
    expect(matches("/some/FILE.ISO")).toBe(false);
    expect(matches("/some/file.ZIP")).toBe(false);
    expect(matches("/some/file.DEB")).toBe(false);
  });
});

describe("mirror manifest — GET/HEAD and ETag", () => {
  it("serves GET with a quoted digest ETag", async () => {
    const res = await api.request(MANIFEST_URL);

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("application/json");
    expect(res.headers.get("etag")).toMatch(/^"[0-9a-f]{64}"$/);

    const body = (await res.json()) as MirrorManifest;
    expect(Array.isArray(body.mirrors)).toBe(true);
    expect(Array.isArray(body.policies)).toBe(true);
  });

  it("returns the same ETag for identical content", async () => {
    const [first, second] = await Promise.all([
      api.request(MANIFEST_URL),
      api.request(MANIFEST_URL),
    ]);

    expect(first.headers.get("etag")).toBe(second.headers.get("etag"));
  });

  it("answers HEAD with the GET ETag and no body", async () => {
    const get = await api.request(MANIFEST_URL);
    const head = await api.request(MANIFEST_URL, { method: "HEAD" });

    expect(head.status).toBe(200);
    expect(head.headers.get("etag")).toBe(get.headers.get("etag"));
    expect(await head.text()).toBe("");
  });

  it("answers a matching If-None-Match with 304 and the ETag", async () => {
    const get = await api.request(MANIFEST_URL);
    const etag = get.headers.get("etag") as string;

    const res = await api.request(MANIFEST_URL, {
      headers: { "if-none-match": etag },
    });

    expect(res.status).toBe(304);
    expect(res.headers.get("etag")).toBe(etag);
    expect(await res.text()).toBe("");
  });

  it("answers HEAD + matching If-None-Match with 304", async () => {
    const get = await api.request(MANIFEST_URL);
    const etag = get.headers.get("etag") as string;

    const res = await api.request(MANIFEST_URL, {
      method: "HEAD",
      headers: { "if-none-match": etag },
    });

    expect(res.status).toBe(304);
    expect(await res.text()).toBe("");
  });

  it("accepts weak and list-form validators", async () => {
    const etag = (await api.request(MANIFEST_URL)).headers.get("etag") as string;

    for (const header of [`W/${etag}`, `"nope", ${etag}`, "*"]) {
      const res = await api.request(MANIFEST_URL, {
        headers: { "if-none-match": header },
      });
      expect(res.status).toBe(304);
    }
  });

  it("serves a full 200 for a non-matching If-None-Match", async () => {
    const res = await api.request(MANIFEST_URL, {
      headers: { "if-none-match": '"not-the-manifest"' },
    });

    expect(res.status).toBe(200);
    expect(((await res.json()) as MirrorManifest).policies).toHaveLength(3);
  });
});

describe("etagMatches", () => {
  it("ignores absent headers and weak prefixes", () => {
    expect(etagMatches(null, '"abc"')).toBe(false);
    expect(etagMatches('W/"abc"', '"abc"')).toBe(true);
    expect(etagMatches('"abc"', '"abc"')).toBe(true);
    expect(etagMatches(' "x" , "abc" ', '"abc"')).toBe(true);
    expect(etagMatches('"abcd"', '"abc"')).toBe(false);
  });
});
