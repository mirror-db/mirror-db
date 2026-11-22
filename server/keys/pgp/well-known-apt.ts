/**
 * Well-known APT signing keys, keyed by a short repo name. Mirrors the APT
 * repos served under `mirs.uk/<name>/`.
 *
 * A repo name may map to several URLs: the certificates are merged into one
 * keyring. That is not a convenience — Debian signs its index with three
 * certificates at once, and `sqv` refuses the whole file when one of them is
 * unknown, so a partial keyring breaks `apt-get update`.
 *
 * Names match the mount where possible; the historical aliases stay so old
 * links keep working.
 */
import { UnknownKeyError } from "../utils/errors";

const DebianSecurityKeys = [
  "https://ftp-master.debian.org/keys/archive-key-12-security.asc",
  "https://ftp-master.debian.org/keys/archive-key-13-security.asc",
];

const SyncthingKeys = [
  "https://syncthing.net/release-key.gpg",
  "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xD26E6ED000654A3E",
];

const GitLabKeys = "https://packages.gitlab.com/gpgkey/gpg.key";

/**
 * Debian Ports publishes one archive key per year and keeps the previous one
 * available while a transition runs, so both are served: a client pinned to a
 * suite signed by either certificate then validates.
 */
const DebianPortsKeys = [
  "https://www.ports.debian.org/archive_2026.key",
  "https://www.ports.debian.org/archive_2027.key",
];

/**
 * NVIDIA publishes one certificate per (distro, architecture) tree, and each
 * tree's `InRelease` is signed by its own. CUDA's `debian12`, `debian13` and
 * `ubuntu2604` x86_64 trees are the ones this mirror serves, so all three
 * certificates are merged — otherwise apt reports `Missing key` for the two
 * trees it did not get.
 */
const CudaBase = "https://developer.download.nvidia.com/compute/cuda/repos/";
const CudaKeys = [
  `${CudaBase}debian12/x86_64/3bf863cc.pub`,
  `${CudaBase}debian13/x86_64/8793F200.pub`,
  `${CudaBase}ubuntu2604/x86_64/60DF8A40.pub`,
];

/** Microsoft rotates its repository key; the rolling one is the current signer. */
const MicrosoftKeys = [
  "https://packages.microsoft.com/keys/microsoft-rolling.asc",
  "https://packages.microsoft.com/keys/microsoft.asc",
];

/** OpenTofu signs providers and the repository index with different keys. */
const OpenTofuKeys = [
  "https://get.opentofu.org/opentofu.gpg",
  "https://packages.opentofu.org/opentofu/tofu/gpgkey",
];

const WellKnownAptKeyUrl: Record<string, string | string[]> = {
  // Signers of both bookworm and trixie: the two archive keys plus the two
  // stable-release keys. Serving only one makes apt report the others as
  // `Missing key`.
  debian: [
    "https://ftp-master.debian.org/keys/archive-key-12.asc",
    "https://ftp-master.debian.org/keys/archive-key-13.asc",
    "https://ftp-master.debian.org/keys/release-12.asc",
    "https://ftp-master.debian.org/keys/release-13.asc",
  ],
  "debian-security": DebianSecurityKeys,
  // Debian Ports signs its suites with a yearly archive key, published as a
  // plain file at the project site — no `.deb` extraction needed.
  "debian-ports": DebianPortsKeys,
  ubuntu: "https://archive.ubuntu.com/ubuntu/project/ubuntu-archive-keyring.gpg",
  "ubuntu-ports": "https://archive.ubuntu.com/ubuntu/project/ubuntu-archive-keyring.gpg",
  mise: "https://mise.jdx.dev/gpg-key.pub",
  // Syncthing signs with two certificates and publishes only the current one,
  // so a client following upstream's instructions gets `Missing key 37C84554…`.
  syncthing: SyncthingKeys,
  "syncthing-pkgs": SyncthingKeys,
  opentofu: OpenTofuKeys,
  "opentofu-repo": "https://packages.opentofu.org/opentofu/tofu/gpgkey",
  "github-cli": "https://cli.github.com/packages/githubcli-archive-keyring.gpg",
  // Flat repository (no `dists/`): the index lives next to the packages at
  // `/stable/deb/<arch>/InRelease`, which the same key signs.
  "libnvidia-container": "https://nvidia.github.io/libnvidia-container/gpgkey",
  nvidia: "https://nvidia.github.io/libnvidia-container/gpgkey",
  // One certificate per upstream tree; see `CudaKeys`.
  cuda: CudaKeys,
  // Microsoft rotates its signing certificate; see `MicrosoftKeys`.
  microsoft: MicrosoftKeys,
  // Versioned key path: pkgs.k8s.io publishes one per minor, and the URL must
  // match the suite a preset points at.
  kubernetes: "https://pkgs.k8s.io/core:/stable:/v1.37/deb/Release.key",
  // The `docker-ce` tree is the Debian repository; `docker-ubuntu` stays as the
  // alias for callers that already use the Ubuntu key URL.
  "docker-ce": "https://download.docker.com/linux/debian/gpg",
  "gitlab-runner": GitLabKeys,
  "gitlab-runner-deb": GitLabKeys,
  gitlab: GitLabKeys,
  hashicorp: "https://apt.releases.hashicorp.com/gpg",
  // One certificate signs both the `infra` repo and every `pgsql/<codename>`
  // repo, so the same key serves either `Signed-By:`.
  pigsty: "https://repo.pigsty.io/key",

  // Keys for repositories this project does not mirror (kept for callers that
  // already rely on them).
  docker: "https://download.docker.com/linux/debian/gpg",
  "docker-ubuntu": "https://download.docker.com/linux/ubuntu/gpg",
  nodesource: "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key",
};

export function repo2KeyUrl(keyname: string) {
  const url = WellKnownAptKeyUrl[keyname];
  if (!url) throw new UnknownKeyError(keyname);
  return url;
}
