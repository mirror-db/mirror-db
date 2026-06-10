/**
 * Minimal parser for APT `Release` / `InRelease` index files.
 *
 * A Release file is an RFC822-style document describing one suite (`dists/<suite>/`).
 * It carries a `Valid-Until` freshness window and lists every metadata file in
 * the suite with its checksum and size under `SHA256:` (and weaker `SHA1:` /
 * `MD5Sum:` blocks we ignore). `InRelease` is the same document wrapped in an
 * inline PGP clear-signature.
 *
 * This is intentionally dependency-free (no `apt-parser`) and side-effect-free so
 * it unit-tests in plain Node.
 */

/** One file entry from a Release checksum block. Path is relative to `dists/<suite>/`. */
export interface ReleaseFile {
  /** Checksum block this came from, as written: `MD5Sum` | `SHA1` | `SHA256` | `SHA512`. */
  algo: string;
  path: string;
  hash: string;
  size: number;
}

/**
 * Checksum block headers APT publishes, lower-cased for matching. A suite lists
 * the same files under each, and `by-hash/<algo>/<hash>` can address any of
 * them — so all are collected, not just SHA256.
 */
const CHECKSUM_BLOCKS: Record<string, string> = {
  md5sum: "MD5Sum",
  sha1: "SHA1",
  sha256: "SHA256",
  sha512: "SHA512",
};

export interface ReleaseIndex {
  suite: string | null;
  codename: string | null;
  /** `Valid-Until` as epoch ms, or null when the field is absent. */
  validUntil: number | null;
  files: ReleaseFile[];
}

/**
 * Strip the inline PGP clear-signature wrapper from an `InRelease` document,
 * returning the bare RFC822 body. Plain `Release` text is returned unchanged.
 *
 * Clear-signed layout:
 *   -----BEGIN PGP SIGNED MESSAGE-----
 *   Hash: SHA512
 *   <blank line>
 *   <body...>
 *   -----BEGIN PGP SIGNATURE-----
 *   ...
 */
function stripPgpArmor(text: string): string {
  if (!text.includes("BEGIN PGP SIGNED MESSAGE")) return text;

  // Body starts after the blank line that follows the armor + Hash headers.
  const blank = text.indexOf("\n\n");
  if (blank === -1) return text;
  let body = text.slice(blank + 2);

  const sig = body.indexOf("-----BEGIN PGP SIGNATURE-----");
  if (sig !== -1) body = body.slice(0, sig);
  return body;
}

/** Parse an RFC822 `Valid-Until` / `Date` value to epoch ms, or null. */
function parseDate(value: string | undefined): number | null {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
}

/**
 * Parse a Release / InRelease document into its suite metadata and SHA256 file
 * list. Unknown fields and the weaker checksum blocks are ignored.
 */
export function parseRelease(text: string): ReleaseIndex {
  const body = stripPgpArmor(text);
  const lines = body.split("\n");

  const fields: Record<string, string> = {};
  const files: ReleaseFile[] = [];
  let block: string | null = null; // active checksum block (canonical algo), or null

  for (const raw of lines) {
    if (raw === "") continue;

    // Continuation lines (leading whitespace) belong to the active multiline
    // block. We only care about the checksum lists.
    const indented = raw[0] === " " || raw[0] === "\t";
    if (indented) {
      if (!block) continue;
      const parts = raw.trim().split(/\s+/);
      if (parts.length >= 3) {
        const [hash, size, path] = parts;
        const bytes = Number(size);
        if (/^[0-9a-f]{32,128}$/i.test(hash) && Number.isFinite(bytes)) {
          files.push({ algo: block, hash: hash.toLowerCase(), size: bytes, path });
        }
      }
      continue;
    }

    // Top-level `Field: value` line — also closes any open multiline block.
    const colon = raw.indexOf(":");
    if (colon === -1) {
      block = null;
      continue;
    }
    const key = raw.slice(0, colon).trim();
    const value = raw.slice(colon + 1).trim();
    block = CHECKSUM_BLOCKS[key.toLowerCase()] ?? null;
    if (!block) fields[key.toLowerCase()] = value;
  }

  return {
    suite: fields["suite"] ?? null,
    codename: fields["codename"] ?? null,
    validUntil: parseDate(fields["valid-until"]),
    files,
  };
}
