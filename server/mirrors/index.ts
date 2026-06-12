import { createRegistryHost } from "@server/pkgs/oci/host";
import type { Mirror } from "@server/types";

import docker from "./docker";
import { npm } from "./npm";
import { pypi } from "./pypi";
import { aptMirrors } from "./apt-mirrors";
import { webMirrors } from "./web-mirrors";

// Simple anonymous-passthrough OCI registries — the proxy follows each
// registry's own token challenge, so only the upstream host differs. Mirrors
// needing extra logic (login, path rewrites, an index) live in their own
// subdirectory, like ./docker.
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
export const mirrors: Mirror[] = [docker, ...simpleRegistries, ...aptMirrors, ...webMirrors, pypi, npm];

/** Subdomain-routed mirrors, keyed by subdomain (e.g. `"dcr"`). */
export const mirrorsBySubdomain: Map<string, Mirror> = new Map(
  mirrors.filter((m) => m.host).map((m) => [m.host as string, m]),
);

/** All mirrors, keyed by name — used by the `/api/status/:name` endpoint. */
export const mirrorsByName: Map<string, Mirror> = new Map(
  mirrors.map((m) => [m.name, m]),
);

/** Path-routed mirrors, keyed by path segment (e.g. `"npm"`). */
export const mirrorsByPath: Map<string, Mirror> = new Map(
  mirrors.filter((m) => m.path).map((m) => [m.path as string, m]),
);
