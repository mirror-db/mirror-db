/**
 * Catalog composition point.
 *
 * `registerAptPresets` is the standard set: distribution archives, the
 * verified third parties, and the file-tree (web/special) repositories — one
 * registry, one listing, one lookup.
 */

import { AptPresetRegistry } from "../registry";

import { registerDebianPresets, registerUbuntuPresets } from "./os";
import {
  registerGitlabRunnerPresets,
  registerHashicorpPresets,
  registerThirdPartyPresets,
} from "./third-party";
import { registerWebAptPresets } from "./web";

/** Standard presets: distribution archives, third parties, and web mounts. */
export function registerAptPresets(registry: AptPresetRegistry): AptPresetRegistry {
  registerDebianPresets(registry);
  registerUbuntuPresets(registry);
  registerThirdPartyPresets(registry);
  registerHashicorpPresets(registry);
  registerGitlabRunnerPresets(registry);
  registerWebAptPresets(registry);
  return registry;
}

/** Isolate-scoped catalog used by the `/conf/apt/` app. */
export const aptPresetRegistry = registerAptPresets(new AptPresetRegistry());
