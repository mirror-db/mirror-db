/**
 * The common single-stanza preset.
 *
 * Most repositories are one root with one key: `mount`, `suites`,
 * `components`, architectures, and signing material. Presets that need more
 * (several stanzas, an architecture in the URI) write their own function.
 */

import type {
  AptPreset,
  AptPresetDefaults,
  AptRenderContext,
  AptSourceStanza,
} from "./types";

/** `https://relay.example` + `debian` → `https://relay.example/debian/`. */
const mirrorUri = (origin: string, mount: string) =>
  `${origin.replace(/\/+$/, "")}/${mount.replace(/^\/+|\/+$/g, "")}/`;

export function aptPreset(defaults: AptPresetDefaults): AptPreset {
  const preset = async ({ origin, overrides, inlineKey }: AptRenderContext) => {
    const suites = defaults.flat
      ? (overrides.suites ?? ["/"])
      : (overrides.suites ?? defaults.suites);
    if (!suites?.length) {
      throw new Error(`apt preset for ${defaults.mount} has no default suites`);
    }

    const components = defaults.flat
      ? undefined
      : (overrides.components ?? defaults.components);
    const architectures = overrides.architectures ?? defaults.architectures;

    const stanza: AptSourceStanza = {
      comment: defaults.comment,
      uris: mirrorUri(origin, defaults.mount),
      suites,
      // An explicit empty override means "omit the field", not "emit it empty".
      components: components?.length ? components : undefined,
      architectures: architectures?.length ? architectures : undefined,
      signedBy: defaults.key
        ? { sources: await inlineKey(defaults.key) }
        : defaults.keyring,
    };

    return [stanza];
  };

  return Object.assign(preset, { flat: defaults.flat ?? false });
}
