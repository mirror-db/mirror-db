/**
 * Third-party APT presets.
 *
 * These repositories are not distribution archives: apt has no native
 * architecture here, so `amd64` is the default (override with `?arch=`). Signing
 * material is embedded inline, exactly as `/keys/apt/<name>/in-sources` prints
 * it, because the key is not part of any installed keyring.
 *
 * HashiCorp and GitLab Runner publish one suite per Debian/Ubuntu codename, so
 * each codename is an explicit preset rather than a guessed fallback.
 */

import { aptPreset } from "../preset";
import type { AptPresetRegistry } from "../registry";

/** Architecture for repositories that are not part of a distribution archive. */
const ThirdPartyArchitectures = ["amd64"];

/** One repository, one suite, one component, one inline key. */
const ThirdPartyRepos = [
  { name: "mise", suites: ["stable"], components: ["main"], key: "mise" },
  {
    name: "syncthing-pkgs",
    suites: ["syncthing"],
    components: ["stable-v2"],
    key: "syncthing-pkgs",
  },
  { name: "opentofu", suites: ["any"], components: ["main"], key: "opentofu" },
  {
    name: "github-cli",
    suites: ["stable"],
    components: ["main"],
    key: "github-cli",
  },
];

/** Codename suites of the HashiCorp repository (Debian and Ubuntu). */
const HashicorpReleases = [
  { distro: "debian", version: "12", codename: "bookworm" },
  { distro: "debian", version: "13", codename: "trixie" },
  { distro: "ubuntu", version: "22.04", codename: "jammy" },
  { distro: "ubuntu", version: "24.04", codename: "noble" },
  { distro: "ubuntu", version: "26.04", codename: "resolute" },
];

/** Codename suites of `packages.gitlab.com/runner/gitlab-runner/debian`. */
const GitlabRunnerReleases = [
  { version: "12", codename: "bookworm" },
  { version: "13", codename: "trixie" },
  { version: "14", codename: "forky" },
];

export function registerThirdPartyPresets(registry: AptPresetRegistry): void {
  for (const repo of ThirdPartyRepos) {
    registry.register(
      repo.name,
      aptPreset({
        mount: repo.name,
        suites: repo.suites,
        components: repo.components,
        architectures: ThirdPartyArchitectures,
        key: repo.key,
      }),
    );
  }
}

export function registerHashicorpPresets(registry: AptPresetRegistry): void {
  for (const release of HashicorpReleases) {
    registry.register(
      [
        `hashicorp-${release.distro}-${release.version}`,
        `hashicorp-${release.distro}-${release.codename}`,
      ],
      aptPreset({
        mount: "hashicorp",
        suites: [release.codename],
        components: ["main"],
        architectures: ThirdPartyArchitectures,
        key: "hashicorp",
      }),
    );
  }
}

export function registerGitlabRunnerPresets(registry: AptPresetRegistry): void {
  for (const release of GitlabRunnerReleases) {
    registry.register(
      [
        `gitlab-runner-debian-${release.version}`,
        `gitlab-runner-debian-${release.codename}`,
      ],
      aptPreset({
        mount: "gitlab-runner-deb",
        suites: [release.codename],
        components: ["main"],
        architectures: ThirdPartyArchitectures,
        key: "gitlab-runner-deb",
      }),
    );
  }
}
