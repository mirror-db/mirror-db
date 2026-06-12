import {
  createWebListMirror,
  createPassthroughMirror,
  type WebMirrorConfig,
} from "./web-mirror";

/**
 * Web-list mirrors — upstream has autoindex, we expose full directory browsing.
 */
const webListConfigs: WebMirrorConfig[] = [
  {
    name: "cuda",
    base: "https://developer.download.nvidia.com/compute/cuda/repos/",
  },
  {
    name: "debian-cd",
    base: "https://cdimage.debian.org/debian-cd/",
  },
  {
    name: "docker-ce",
    base: "https://download.docker.com/",
  },
  {
    name: "ubuntu-cdimages",
    base: "https://cdimage.ubuntu.com/",
  },
  {
    name: "ubuntu-cloud-images",
    base: "https://cloud-images.ubuntu.com/",
  },
  {
    name: "ubuntu-releases",
    base: "https://releases.ubuntu.com/",
  },
  {
    name: "microsoft",
    base: "https://packages.microsoft.com/",
  },
];

/**
 * Passthrough mirrors — no directory listing, just forward requests to upstream.
 */
const passthroughConfigs: WebMirrorConfig[] = [
  {
    name: "hashicorp",
    base: "https://apt.releases.hashicorp.com/",
  },
  {
    name: "kubernetes",
    base: "https://prod-cdn.packages.k8s.io/repositories/isv:/kubernetes:/",
  },
  {
    name: "libnvidia-container",
    base: "https://nvidia.github.io/libnvidia-container/",
  },
];

/** All non-APT path-routed mirrors. */
export const webMirrors = [
  ...webListConfigs.map(createWebListMirror),
  ...passthroughConfigs.map(createPassthroughMirror),
];
