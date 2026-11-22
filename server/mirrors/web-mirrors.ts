import {
  createWebListMirror,
  createPassthroughMirror,
  type WebMirrorConfig,
} from "./web-mirror";

/**
 * Web-list mirrors — upstream has autoindex, we expose full directory browsing.
 *
 * A file tree that also happens to be an APT repository carries the same
 * metadata `createAptMirror` would set: `keepHTTP` (apt has no HTTPS upgrade
 * path of its own) and the `apt` policy tag (pool/<arch>/x.deb goes to the
 * speed transport).
 */
const webListConfigs: WebMirrorConfig[] = [
  {
    name: "cuda",
    base: "https://developer.download.nvidia.com/compute/cuda/repos/",
    keepHTTP: true,
    policyTags: ["apt"],
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
    keepHTTP: true,
    policyTags: ["apt"],
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
    keepHTTP: true,
    policyTags: ["apt"],
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
    // `gh-proxy.com` now answers `403 Access to proxy rules is forbidden on
    // this hostname`; the `.org` host serves the same path-preserving API.
    name: "gh-proxy",
    base: "https://gh-proxy.org/",
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
    keepHTTP: true,
    policyTags: ["apt"],
  },
  {
    name: "libnvidia-container",
    base: "https://nvidia.github.io/libnvidia-container/",
    keepHTTP: true,
    policyTags: ["apt"],
  },
  {
    name: "nodejs",
    base: "https://nodejs.org/dist/",
  },
  {
    // One mount carries two independent repository roots — `infra/` (suite
    // `generic`) and `pgsql/<codename>/` (each codename its own root) — which
    // `AptRepo` (base + `dists/<suite>`) cannot address, so the client's path
    // is forwarded verbatim. Upstream advertises `Cache-Control: no-store`, so
    // the read-through cache stores nothing: a relay for packages, not a cache.
    name: "pigsty",
    base: "https://repo.pigsty.io/apt/",
    keepHTTP: true,
    policyTags: ["apt"],
  },
];

/** All non-APT path-routed mirrors. */
export const webMirrors = [
  ...webListConfigs.map(createWebListMirror),
  ...passthroughConfigs.map(createPassthroughMirror),
];
