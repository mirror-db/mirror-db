import { createAptMirror, type AptMirrorConfig } from "./apt-mirror";

/**
 * All APT mirror definitions sourced from mirror-db.net/api/endpoints?tag=apt.
 *
 * Each entry maps 1:1 to an upstream APT repository and is served at
 * `mirs.uk/<name>/`. Suites are curated where the full listing would
 * overwhelm the Worker's CPU budget; omit to auto-discover.
 */
const aptConfigs: AptMirrorConfig[] = [
  {
    name: "debian",
    base: "http://ftp.us.debian.org/debian/",
    suites: [
      "stable",
      "testing",
      "unstable",
      "sid",
      "trixie",
      "trixie-updates",
      "trixie-backports",
      "bookworm",
      "bookworm-updates",
      "bookworm-backports",
    ],
  },
  {
    name: "debian-ports",
    base: "http://deb.debian.org/debian-ports/",
    suites: ["sid", "unstable"],
  },
  {
    name: "debian-security",
    base: "https://security.debian.org/debian-security/",
    suites: [
      "trixie-security",
      "bookworm-security",
    ],
  },
  {
    name: "ubuntu",
    base: "http://archive.ubuntu.com/ubuntu/",
    suites: [
      "plucky",
      "plucky-updates",
      "plucky-backports",
      "plucky-security",
      "noble",
      "noble-updates",
      "noble-backports",
      "noble-security",
      "jammy",
      "jammy-updates",
      "jammy-backports",
      "jammy-security",
    ],
  },
  {
    name: "ubuntu-ports",
    base: "http://ports.ubuntu.com/ubuntu-ports/",
    suites: [
      "plucky",
      "plucky-updates",
      "plucky-backports",
      "plucky-security",
      "noble",
      "noble-updates",
      "noble-backports",
      "noble-security",
      "jammy",
      "jammy-updates",
      "jammy-backports",
      "jammy-security",
    ],
  },
  {
    name: "mise",
    base: "https://mise.jdx.dev/deb/",
    suites: ["stable"],
  },
  {
    name: "syncthing-pkgs",
    base: "https://apt.syncthing.dev/",
    suites: ["syncthing"],
  },
  {
    name: "opentofu",
    base: "https://packages.opentofu.org/opentofu/tofu/any/",
    suites: ["any"],
  },
];

/** All APT mirrors, instantiated from the config table above. */
export const aptMirrors = aptConfigs.map(createAptMirror);
