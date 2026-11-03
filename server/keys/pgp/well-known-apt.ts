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
const DebianSecurityKeys = [
  "https://ftp-master.debian.org/keys/archive-key-12-security.asc",
  "https://ftp-master.debian.org/keys/archive-key-13-security.asc",
];

const SyncthingKeys = [
  "https://syncthing.net/release-key.gpg",
  "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xD26E6ED000654A3E",
];

const GitLabKeys = "https://packages.gitlab.com/gpgkey/gpg.key";

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
  // `debian-ports` has no key here on purpose: its archive key is only
  // distributed inside debian-ports-archive-keyring_*.deb (no plain file on
  // ftp.ports.debian.org, and no keyserver carries a bound copy). Install that
  // package, or extract its keyring.
  ubuntu: "https://archive.ubuntu.com/ubuntu/project/ubuntu-archive-keyring.gpg",
  "ubuntu-ports": "https://archive.ubuntu.com/ubuntu/project/ubuntu-archive-keyring.gpg",
  mise: "https://mise.jdx.dev/gpg-key.pub",
  // Syncthing signs with two certificates and publishes only the current one,
  // so a client following upstream's instructions gets `Missing key 37C84554…`.
  syncthing: SyncthingKeys,
  "syncthing-pkgs": SyncthingKeys,
  opentofu: "https://get.opentofu.org/opentofu.gpg",
  "opentofu-repo": "https://packages.opentofu.org/opentofu/tofu/gpgkey",
  "github-cli": "https://cli.github.com/packages/githubcli-archive-keyring.gpg",
  "gitlab-runner": GitLabKeys,
  "gitlab-runner-deb": GitLabKeys,
  gitlab: GitLabKeys,
  hashicorp: "https://apt.releases.hashicorp.com/gpg",

  // Keys for repositories this project does not mirror (kept for callers that
  // already rely on them).
  docker: "https://download.docker.com/linux/debian/gpg",
  "docker-ubuntu": "https://download.docker.com/linux/ubuntu/gpg",
  nodesource: "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key",
};

export function repo2KeyUrl(keyname: string) {
  const url = WellKnownAptKeyUrl[keyname];
  if (!url) throw new Error(`Unknown apt key: ${keyname}`);
  return url;
}
