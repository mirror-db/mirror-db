import { createRegistryHost, type OciHost } from "@server/pkgs/oci/host";

import docker from "./docker";

// Simple anonymous-passthrough mirrors — the proxy follows each registry's own
// token challenge, so only the upstream host differs. Hosts needing extra logic
// (login, path rewrites) live in their own subdirectory, like ./docker.
const simple: OciHost[] = [
  createRegistryHost({ host: "ghcr", upstream: "ghcr.io" }),
  createRegistryHost({ host: "gcr", upstream: "gcr.io" }),
  createRegistryHost({ host: "k8s", upstream: "registry.k8s.io" }),
  createRegistryHost({ host: "quay", upstream: "quay.io" }),
  createRegistryHost({ host: "mcr", upstream: "mcr.microsoft.com" }),
  createRegistryHost({ host: "nvcr", upstream: "nvcr.io" }),
  createRegistryHost({ host: "ocr", upstream: "container-registry.oracle.com" }),
];

/** All mirror hosts, keyed by their subdomain. */
export const hosts: Map<string, OciHost> = new Map(
  [docker, ...simple].map((h) => [h.host, h]),
);
