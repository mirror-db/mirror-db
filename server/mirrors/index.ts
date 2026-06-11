import { createRegistryHost } from "@server/pkgs/oci/host";
import type { Mirror } from "@server/types";

import docker from "./docker";
import debian from "./debian";

// Simple anonymous-passthrough OCI registries — the proxy follows each
// registry's own token challenge, so only the upstream host differs. Mirrors
// needing extra logic (login, path rewrites, an index) live in their own
// subdirectory, like ./docker and ./debian.
const simpleRegistries: Mirror[] = [
  createRegistryHost({ host: "ghcr", upstream: "ghcr.io" }),
  createRegistryHost({ host: "gcr", upstream: "gcr.io" }),
  createRegistryHost({ host: "k8s", upstream: "registry.k8s.io" }),
  createRegistryHost({ host: "quay", upstream: "quay.io" }),
  createRegistryHost({ host: "mcr", upstream: "mcr.microsoft.com" }),
  createRegistryHost({ host: "nvcr", upstream: "nvcr.io" }),
  createRegistryHost({ host: "ocr", upstream: "container-registry.oracle.com" }),
];

/** Every mirror, regardless of how it is routed (subdomain or path prefix). */
export const mirrors: Mirror[] = [docker, ...simpleRegistries, debian];

/** Subdomain-routed mirrors, keyed by subdomain (e.g. `"dcr"`). */
export const mirrorsBySubdomain: Map<string, Mirror> = new Map(
  mirrors.filter((m) => m.host).map((m) => [m.host as string, m]),
);

/** All mirrors, keyed by name — used by the `/status/<name>` endpoint. */
export const mirrorsByName: Map<string, Mirror> = new Map(
  mirrors.map((m) => [m.name, m]),
);

// Path-routed mirrors, longest prefix first so the most specific one wins.
const pathRouted: Mirror[] = mirrors
  .filter((m) => m.path)
  .sort((a, b) => (b.path as string).length - (a.path as string).length);

/** Find the path-routed mirror whose prefix matches `pathname`, or undefined. */
export function matchMirrorPath(pathname: string): Mirror | undefined {
  return pathRouted.find((m) => pathname.startsWith(m.path as string));
}
