/**
 * Well-known APT signing keys, keyed by a short repo name. Mirrors the APT
 * repos served under `mirs.uk/<name>/` plus a few common third-party repos.
 */
const WellKnownAptKeyUrl: Record<string, string | string[]> = {
  hashicorp: "https://apt.releases.hashicorp.com/gpg",
  mise: "https://mise.jdx.dev/gpg-key.pub",
  opentofu: "https://get.opentofu.org/opentofu.gpg",
  "opentofu-repo": "https://packages.opentofu.org/opentofu/tofu/gpgkey",
  // Syncthing signs its index with two certificates: the current release key
  // (published at syncthing.net) and the long-standing 2014 key. Upstream ships
  // only the former, so a client following their instructions still gets
  // "Missing key 37C84554…" from `sqv`, and apt drops the repository. Both are
  // served here as one keyring.
  //
  // The 2014 certificate is taken from keyserver.ubuntu.com because it carries
  // the full self-signatures (keys.openpgp.org strips them). Note its self-sig
  // is SHA-1, which sequoia refuses to treat as bound — that is a warning, not
  // an error: `sqv` still exits 0 and apt accepts the index.
  syncthing: [
    "https://syncthing.net/release-key.gpg",
    "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xD26E6ED000654A3E",
  ],
  // GitLab's package repository (runner + others), served at
  // `/gitlab-runner-deb/`. The published file is already armored, which is what
  // `/in-sources` needs.
  "gitlab-runner": "https://packages.gitlab.com/gpgkey/gpg.key",
  gitlab: "https://packages.gitlab.com/gpgkey/gpg.key",
  docker: "https://download.docker.com/linux/debian/gpg",
  "docker-ubuntu": "https://download.docker.com/linux/ubuntu/gpg",
  nodesource: "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key",
};

export function repo2KeyUrl(keyname: string) {
  const url = WellKnownAptKeyUrl[keyname];
  if (!url) throw new Error(`Unknown apt key: ${keyname}`);
  return url;
}
