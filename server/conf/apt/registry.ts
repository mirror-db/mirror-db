/**
 * Preset registry.
 *
 * A registry holds names, not a single canonical name plus a lookup table:
 * every alias is a registered path and appears in the `/conf/apt/` listing.
 * Aliases of one preset render identical output.
 */

import type { AptPreset, AptPresetEntry } from "./types";

/** Preset names become URL segments, so keep them to a safe alphabet. */
const NAME_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;

export class AptPresetRegistry {
  private readonly presets = new Map<string, AptPresetEntry>();

  /** Register one or more names for the same preset. Duplicates fail loudly. */
  register(names: string | string[], preset: AptPreset): this {
    const batch = Array.isArray(names) ? names : [names];
    // Validate the whole batch before inserting, so a typo in the second alias
    // cannot leave the first one half-registered.
    const seen = new Set<string>();
    for (const name of batch) {
      if (!NAME_PATTERN.test(name)) {
        throw new Error(`invalid apt preset name: ${name}`);
      }
      if (this.presets.has(name) || seen.has(name)) {
        throw new Error(`duplicate apt preset name: ${name}`);
      }
      seen.add(name);
    }
    for (const name of batch) {
      this.presets.set(name, { name, flat: preset.flat === true, preset });
    }
    return this;
  }

  get(name: string): AptPresetEntry | undefined {
    return this.presets.get(name);
  }

  has(name: string): boolean {
    return this.presets.has(name);
  }

  /** Every registered path, stable-sorted by name. */
  entries(): AptPresetEntry[] {
    return [...this.presets.values()].sort((a, b) =>
      a.name < b.name ? -1 : a.name > b.name ? 1 : 0,
    );
  }
}

/** Suffix every generated source path carries. */
export const APT_SOURCE_SUFFIX = ".sources";

/** Public URL of a registered preset. */
export const aptSourcePath = (name: string): string =>
  `/conf/apt/${name}${APT_SOURCE_SUFFIX}`;
