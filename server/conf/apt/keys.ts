/**
 * Inline key loading for APT source presets.
 *
 * Single source of truth: the same `/keys/apt/<name>` provider and the same
 * armored-key handling as the key proxy, so a `.sources` file embeds what
 * `/keys/apt/<name>/in-sources` prints. Keys are fetched from the public
 * upstreams, never from this Worker.
 */

import { PGPPubKeyHandler } from "@server/keys/utils/pgp-key";
import { repo2KeyUrl } from "@server/keys/pgp/well-known-apt";

import type { AptKeyLoader } from "./types";

/**
 * Resolve a key-proxy name to a deb822 `Signed-By:` value (a multi-certificate
 * keyring stays one armored block).
 */
export const loadInlineKey: AptKeyLoader = async (name) => {
  const handler = await PGPPubKeyHandler.fromUrls([repo2KeyUrl(name)].flat());
  const sources = handler.inSources();
  // A key that cannot be armored has no usable form in a deb822 field; that is
  // a failure rather than an unsigned source.
  if (!sources) throw new Error(`key ${name} could not be armored`);
  return sources;
};
