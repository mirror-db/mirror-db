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
  // published at syncthing.net, and the long-standing 2014 key. Upstream ships
  // only the former, so a client following their instructions still gets
  // "Missing key 37C84554…" from `sqv` and apt drops the repository. Both are
  // served here, as one keyring.
  syncthing: [
    "https://syncthing.net/release-key.gpg",
    "https://keys.openpgp.org/vks/v1/by-fingerprint/37C84554E7E0A261E4F76E1ED26E6ED000654A3E",
  ],
  docker: "https://download.docker.com/linux/debian/gpg",
  "docker-ubuntu": "https://download.docker.com/linux/ubuntu/gpg",
  nodesource: "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key",
};

export function repo2KeyUrl(keyname: string) {
  const url = WellKnownAptKeyUrl[keyname];
  if (!url) throw new Error(`Unknown apt key: ${keyname}`);
  return url;
}
