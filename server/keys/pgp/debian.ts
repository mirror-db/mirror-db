const ArchiveVersionMap: Record<string, number> = {
  jessie: 8,
  stretch: 9,
  buster: 10,
  bullseye: 11,
  bookworm: 12,
  trixie: 13,
};

const KeyBaseUrl = "https://ftp-master.debian.org/keys/";

/**
 * Resolve a Debian archive key by codename or version, with an optional
 * variant suffix:
 * - `bookworm` / `12`        → `archive-key-12.asc`
 * - `bookworm-security`      → `archive-key-12-security.asc`
 * - `bookworm-release`       → `release-12.asc`
 */
export function debianKeyName2Url(keyname: string) {
  const [codename, variant] = keyname.split("-");

  const version = ArchiveVersionMap[codename] || Number(codename);
  if (isNaN(version)) throw new Error(`Invalid version: ${codename}`);
  if (version < 8) throw new Error(`Invalid version: ${version}`);

  let file = `archive-key-${version}.asc`;
  if (variant === "security") file = `archive-key-${version}-security.asc`;
  if (variant === "release") file = `release-${version}.asc`;

  return new URL(file, KeyBaseUrl);
}
