/**
 * Turn a registered preset into a `.sources` file for one request.
 *
 * Validation lives here rather than in the HTTP layer: the same rules apply to
 * every caller (tests included), and a preset never sees overrides it must
 * silently fix up.
 */

import { loadInlineKey } from "./keys";
import { parseAptQuery } from "./params";
import { renderAptSources } from "./render";
import type {
  AptKeyLoader,
  AptOverrides,
  AptPresetEntry,
  AptProtocol,
} from "./types";

/** Overrides a preset cannot honour: the HTTP layer answers 400. */
export class AptSourceError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AptSourceError";
  }
}

export interface AptSourceOptions {
  /** Relay-aware origin, without a trailing slash. */
  origin: string;
  query: URLSearchParams;
  /** Defaults to the real key proxy; tests inject a stub. */
  inlineKey?: AptKeyLoader;
}

/**
 * Force the origin's scheme and keep everything else. Presets build every URI
 * from this origin, so one rewrite covers distribution, third-party and
 * custom paths alike. HTTPS is the default, including when the Worker itself
 * was reached over plain HTTP.
 */
function withProtocol(origin: string, protocol: AptProtocol): string {
  return origin.replace(/^[a-z][a-z0-9+.-]*:\/\//i, `${protocol}://`);
}

export async function renderAptSource(
  entry: AptPresetEntry,
  options: AptSourceOptions,
): Promise<string> {
  const overrides = parseAptQuery(options.query);
  validateOverrides(entry, overrides);

  const stanzas = await entry.preset({
    origin: withProtocol(
      options.origin.replace(/\/+$/, ""),
      overrides.protocol ?? "https",
    ),
    overrides,
    inlineKey: options.inlineKey ?? loadInlineKey,
  });

  return renderAptSources(stanzas);
}

function validateOverrides(entry: AptPresetEntry, overrides: AptOverrides) {
  if (!entry.flat) return;

  // A flat repository has its indexes beside its packages: exactly one suite
  // (`/`) and no components. Anything else is a malformed source, not a
  // preference.
  if (
    overrides.suites &&
    (overrides.suites.length !== 1 || overrides.suites[0] !== "/")
  ) {
    throw new AptSourceError('flat repository only accepts "suites=/"');
  }
  if (overrides.components?.length) {
    throw new AptSourceError("flat repository has no components");
  }
}
