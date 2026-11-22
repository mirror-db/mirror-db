/**
 * Debian and Ubuntu presets.
 *
 * Version and codename are both registered names for the same preset, never
 * an unversioned `debian.sources` / `ubuntu.sources`: a public configuration
 * URL must not change meaning when a release moves.
 *
 * Neither emits `Architectures`, so apt uses its native architecture selection.
 */

import { aptPreset } from "../preset";
import type { AptPresetRegistry } from "../registry";
import type { AptPreset, AptRenderContext } from "../types";

const DebianKeyring = "/usr/share/keyrings/debian-archive-keyring.gpg";
const UbuntuKeyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg";

const DebianComponents = ["main", "contrib", "non-free", "non-free-firmware"];
const UbuntuComponents = ["main", "restricted", "universe", "multiverse"];

/** Debian archive releases, with the suite set each one publishes. */
const DebianReleases = [
  {
    version: "12",
    codename: "bookworm",
    suites: ["bookworm", "bookworm-updates", "bookworm-backports"],
  },
  {
    version: "13",
    codename: "trixie",
    suites: ["trixie", "trixie-updates", "trixie-backports"],
  },
  {
    // A new stable has no `-updates` or `-backports` suite yet; emitting them
    // would make `apt-get update` fail on the first poll.
    version: "14",
    codename: "forky",
    suites: ["forky"],
  },
];

const UbuntuReleases = [
  { version: "22.04", codename: "jammy" },
  { version: "24.04", codename: "noble" },
  { version: "26.04", codename: "resolute" },
  { version: "26.10", codename: "stonking" },
];

/**
 * One Debian release is two stanzas: the archive and its security pocket.
 *
 * Every release publishes exactly one security suite, `<codename>-security`;
 * the other names below `dists/` belong to other releases or rolling aliases,
 * so there is no `-updates` or `-backports` security pocket to add. A `suites`
 * override selects archive suites only — the security stanza keeps its fixed
 * suite — while Components and Architectures overrides apply to both.
 */
function debianReleasePreset(
  release: (typeof DebianReleases)[number],
): AptPreset {
  const archive = aptPreset({
    mount: "debian",
    suites: release.suites,
    components: DebianComponents,
    keyring: DebianKeyring,
  });
  const security = aptPreset({
    mount: "debian-security",
    suites: [`${release.codename}-security`],
    components: DebianComponents,
    keyring: DebianKeyring,
  });

  return async (context: AptRenderContext) => {
    const { suites: _archiveOnly, ...securityOverrides } = context.overrides;
    return [
      ...(await archive(context)),
      ...(await security({ ...context, overrides: securityOverrides })),
    ];
  };
}

export function registerDebianPresets(registry: AptPresetRegistry): void {
  for (const release of DebianReleases) {
    registry.register(
      [`debian-${release.version}`, `debian-${release.codename}`],
      debianReleasePreset(release),
    );
  }
}

export function registerUbuntuPresets(registry: AptPresetRegistry): void {
  for (const release of UbuntuReleases) {
    const { codename } = release;
    registry.register(
      [`ubuntu-${release.version}`, `ubuntu-${codename}`],
      aptPreset({
        mount: "ubuntu",
        suites: [
          codename,
          `${codename}-updates`,
          `${codename}-backports`,
          `${codename}-security`,
        ],
        components: UbuntuComponents,
        keyring: UbuntuKeyring,
      }),
    );
  }
}
