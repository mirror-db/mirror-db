/**
 * Types for `/conf/apt/` — deb822 APT source generation from explicit presets.
 *
 * A preset is a function that returns stanzas; it never guesses a repository
 * layout from a directory listing. See
 * `docs/adr/0010-explicit-apt-source-presets.md`.
 */

/** An armored public key, embedded in a `Signed-By:` field. */
export interface AptInlineKey {
  /**
   * The key formatted as a deb822 field value: first line unindented, every
   * continuation line indented by one space, blank lines as `.` — exactly what
   * `PGPPubKeyHandler.inSources()` (and `/keys/apt/<name>/in-sources`) produce.
   */
  sources: string;
}

/**
 * One deb822 stanza. `uris` are absolute and already built from the active
 * relay origin; the renderer adds no URLs of its own.
 */
export interface AptSourceStanza {
  /** `#` comment lines printed above the stanza. */
  comment?: string | string[];
  /** Defaults to `deb`. */
  types?: string | string[];
  uris: string | string[];
  suites: string[];
  components?: string[];
  architectures?: string[];
  /** An archive keyring path, or an armored key to embed inline. */
  signedBy?: string | AptInlineKey;
}

/** Scheme every emitted URI uses. HTTPS unless the client asks otherwise. */
export type AptProtocol = "http" | "https";

/**
 * Client overrides from the query string. A missing field means "use the
 * preset default"; an empty array is an explicit "omit this field", which is
 * how `?components=` turns off Components.
 */
export interface AptOverrides {
  suites?: string[];
  components?: string[];
  architectures?: string[];
  protocol?: AptProtocol;
}

/** Resolves an inline key name to a deb822 `Signed-By:` value. Rejects on failure. */
export type AptKeyLoader = (name: string) => Promise<string>;

/** Everything a preset needs to build its stanzas. */
export interface AptRenderContext {
  /** Origin the client reached (relay-aware), without a trailing slash. */
  origin: string;
  overrides: AptOverrides;
  inlineKey: AptKeyLoader;
}

/** A preset: pure-ish, may await an inline key fetch. */
export interface AptPreset {
  (context: AptRenderContext): AptSourceStanza[] | Promise<AptSourceStanza[]>;
  /**
   * `true` for flat repositories: `Suites: /`, no `Components`, and no client
   * override may change either.
   */
  readonly flat?: boolean;
}

/** A registered name (possibly an alias) and the preset it renders. */
export interface AptPresetEntry {
  name: string;
  flat: boolean;
  preset: AptPreset;
}

/** Declarative defaults for the common single-stanza preset. */
export interface AptPresetDefaults {
  /** Mirror mount name below the origin, e.g. `debian`. */
  mount: string;
  /** Default suites. Ignored when `flat` — Suites is then `/`. */
  suites?: string[];
  components?: string[];
  /** Omit to let apt use its native architecture. */
  architectures?: string[];
  /** Installed archive keyring path (Debian, Ubuntu). */
  keyring?: string;
  /** Key-proxy name to embed inline (third-party repositories). */
  key?: string;
  flat?: boolean;
  comment?: string | string[];
}
