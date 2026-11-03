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
    name: "debian-cloud",
    base: "https://cloud.debian.org/images/cloud/",
  },
  {
    name: "docker-ce",
    base: "https://download.docker.com/",
  },
  {
    name: "ubuntu-cd",
    base: "https://cdimage.ubuntu.com/",
  },
  {
    name: "ubuntu-cloud",
    base: "https://cloud-images.ubuntu.com/",
  },
  {
    name: "ubuntu-releases",
    base: "https://releases.ubuntu.com/",
  },
  {
    name: "microsoft",
    // The listing root is `repos/`: the site root also holds config/, keys/
    // and yumrepos/, which are chrome around the repositories themselves.
    base: "https://packages.microsoft.com/repos/",
  },
];

/**
 * Passthrough mirrors — no directory listing, just forward requests to upstream.
 */
const passthroughConfigs: WebMirrorConfig[] = [
  {
    name: "anaconda",
    base: "https://repo.anaconda.com/",
  },
  {
    name: "gh-proxy",
    base: "https://gh-proxy.com/",
  },
  {
    name: "go-releases",
    base: "https://go.dev/dl/",
  },
  {
    name: "kube-releases",
    base: "https://dl.k8s.io/",
  },
  {
    name: "kubernetes",
    base: "https://prod-cdn.packages.k8s.io/repositories/isv:/kubernetes:/",
  },
  {
    name: "libnvidia-container",
    base: "https://nvidia.github.io/libnvidia-container/",
  },
  {
    name: "nodejs",
    base: "https://nodejs.org/dist/",
  },
];

/** All non-APT path-routed mirrors. */
export const webMirrors = [
  ...webListConfigs.map(createWebListMirror),
  ...passthroughConfigs.map(createPassthroughMirror),
];
