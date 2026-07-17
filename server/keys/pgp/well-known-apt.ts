/**
 * Well-known APT signing keys, keyed by a short repo name. Mirrors the APT
 * repos served under `mirs.uk/<name>/` plus a few common third-party repos.
 */
const WellKnownAptKeyUrl: Record<string, string> = {
  hashicorp: "https://apt.releases.hashicorp.com/gpg",
  mise: "https://mise.jdx.dev/gpg-key.pub",
  opentofu: "https://get.opentofu.org/opentofu.gpg",
  "opentofu-repo": "https://packages.opentofu.org/opentofu/tofu/gpgkey",
  syncthing: "https://syncthing.net/release-key.gpg",
  docker: "https://download.docker.com/linux/debian/gpg",
  "docker-ubuntu": "https://download.docker.com/linux/ubuntu/gpg",
  nodesource: "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key",
};

export function repo2KeyUrl(keyname: string) {
  const url = WellKnownAptKeyUrl[keyname];
  if (!url) throw new Error(`Unknown apt key: ${keyname}`);
  return url;
}
