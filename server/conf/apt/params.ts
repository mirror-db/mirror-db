/**
 * Query-string handling for APT source presets.
 *
 * Groups, each with aliases:
 *   - suite:       `suite`, `suites`
 *   - component:   `component`, `components`, `comp`, `comps`
 *   - architecture:`architecture`, `architectures`, `arch`, `archs`
 *   - protocol:    `protocol`, `proto`
 *
 * List values are comma-separated, trimmed, deduplicated, and order-preserving;
 * protocol is a single token, `http` or `https`. Anything else — an unknown
 * parameter, aliases of one group selecting different values, an empty suite or
 * architecture, an invalid protocol — is an error, not input to silently
 * correct.
 */

import type { AptOverrides, AptProtocol } from "./types";

/** A bad query string: the HTTP layer answers 400 with the message. */
export class AptQueryError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AptQueryError";
  }
}

/** Fields that carry a space-separated deb822 list. */
type AptListField = Exclude<keyof AptOverrides, "protocol">;

export const APT_QUERY_ALIASES = {
  suites: ["suite", "suites"],
  components: ["component", "components", "comp", "comps"],
  architectures: ["architecture", "architectures", "arch", "archs"],
} as const satisfies Record<AptListField, readonly string[]>;

export const APT_PROTOCOL_ALIASES = ["protocol", "proto"] as const;

const KNOWN_PARAMS = new Set<string>([
  ...Object.values(APT_QUERY_ALIASES).flat(),
  ...APT_PROTOCOL_ALIASES,
]);

const PROTOCOLS: ReadonlySet<string> = new Set(["http", "https"]);

/** One token acceptable in a space-separated deb822 list field. */
const DEB822_TOKEN = /^[^\s\x00-\x1f\x7f]+$/;

/**
 * Split every raw value on commas, trim, drop empties, dedupe in order.
 *
 * A value lands in a deb822 list field, so it must be one token: whitespace or
 * control characters (`%0A` in a query string decodes to a newline) would let a
 * caller inject an extra field into the generated file.
 */
function parseCsv(values: string[], field: AptListField): string[] {
  const out: string[] = [];
  for (const value of values) {
    for (const part of value.split(",")) {
      const trimmed = part.trim();
      if (!trimmed) continue;
      if (!DEB822_TOKEN.test(trimmed)) {
        throw new AptQueryError(`invalid ${field} value: ${trimmed}`);
      }
      if (!out.includes(trimmed)) out.push(trimmed);
    }
  }
  return out;
}

const sameValues = (a: string[], b: string[]) =>
  a.length === b.length && a.every((value, i) => value === b[i]);

/**
 * One protocol token, not a list: `http,https` is malformed, not a selection.
 * Aliases and repeated occurrences must all agree.
 */
function parseProtocol(params: URLSearchParams): AptProtocol | undefined {
  const present = APT_PROTOCOL_ALIASES.filter((alias) => params.has(alias));
  if (!present.length) return undefined;

  const values = present.flatMap((alias) => params.getAll(alias));
  for (const value of values) {
    if (!PROTOCOLS.has(value)) {
      throw new AptQueryError(`protocol must be http or https: ${value}`);
    }
  }

  const [first, ...rest] = values;
  if (rest.some((other) => other !== first)) {
    throw new AptQueryError(
      `conflicting protocol parameters: ${present.join(", ")}`,
    );
  }
  return first as AptProtocol;
}

export function parseAptQuery(params: URLSearchParams): AptOverrides {
  for (const key of params.keys()) {
    if (!KNOWN_PARAMS.has(key)) {
      throw new AptQueryError(
        `unknown query parameter: ${key} (expected suite, component, architecture, or protocol)`,
      );
    }
  }

  const overrides: AptOverrides = {};
  const protocol = parseProtocol(params);
  if (protocol) overrides.protocol = protocol;

  for (const [field, aliases] of Object.entries(APT_QUERY_ALIASES) as [
    AptListField,
    readonly string[],
  ][]) {
    const present = aliases.filter((alias) => params.has(alias));
    if (!present.length) continue;

    const parsed = present.map((alias) =>
      parseCsv(params.getAll(alias), field),
    );
    const [first, ...rest] = parsed;
    if (rest.some((other) => !sameValues(first, other))) {
      throw new AptQueryError(
        `conflicting ${field} parameters: ${present.join(", ")}`,
      );
    }

    // An explicitly empty `components` is how a client asks for no Components.
    if (field !== "components" && !first.length) {
      throw new AptQueryError(`${field} must not be empty`);
    }

    overrides[field] = first;
  }

  return overrides;
}
