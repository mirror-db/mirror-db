/**
 * Runtime facts established by a request path and declared to the relay.
 *
 * Mirrors keep their own path grammars and construct a fact only inside a
 * branch that has established immutability. This module owns the shared digest
 * validation, response-status gate and directive merge; it never infers a path
 * fact from a response header, and a digest alone never implies immutability.
 */

import { withRelayDirectives, type RelayDirective } from "./relay-control";

type PathDigestAlgorithm = "sha256" | "sha1";

interface PathDigest {
  readonly algorithm: PathDigestAlgorithm;
  readonly hex: string;
}

/** A request path whose bytes are immutable, with an optional content digest. */
export interface PathFact {
  readonly immutable: true;
  readonly digest?: PathDigest;
}

const IMMUTABLE_PATH_FACT: PathFact = { immutable: true };
const DECLARABLE_STATUSES: ReadonlySet<number> = new Set([200, 206, 304]);
const DIGEST_HEX: Record<PathDigestAlgorithm, RegExp> = {
  sha256: /^[0-9a-f]{64}$/,
  sha1: /^[0-9a-f]{40}$/,
};

/** Construct an immutable fact for a path whose publication contract pins its bytes. */
export function immutablePathFact(): PathFact {
  return IMMUTABLE_PATH_FACT;
}

/**
 * Construct the fact for an explicitly content-addressed path.
 *
 * Only full SHA-256 and SHA-1 hex digests are declared to the relay. Other
 * algorithms and malformed values retain the path's immutable fact without
 * inventing a digest the relay cannot use.
 */
export function contentAddressedPathFact(algorithm: string, hex: string): PathFact {
  const normalizedAlgorithm = algorithm.toLowerCase();
  const normalizedHex = hex.toLowerCase();

  if (
    (normalizedAlgorithm === "sha256" || normalizedAlgorithm === "sha1") &&
    DIGEST_HEX[normalizedAlgorithm].test(normalizedHex)
  ) {
    return {
      immutable: true,
      digest: { algorithm: normalizedAlgorithm, hex: normalizedHex },
    };
  }
  return IMMUTABLE_PATH_FACT;
}

/**
 * Declare a path fact on a delivered response. A body, partial body or
 * revalidation describes the same immutable bytes; other statuses and requests
 * without a fact stay untouched. Existing response directives always win.
 */
export function declarePathFact(
  response: Response,
  fact?: PathFact | null,
): Response {
  if (!fact || !DECLARABLE_STATUSES.has(response.status)) return response;

  const directives: RelayDirective[] = ["immutable"];
  if (fact.digest?.algorithm === "sha256") {
    directives.push(`sha256=${fact.digest.hex}`);
  } else if (fact.digest?.algorithm === "sha1") {
    directives.push(`sha1=${fact.digest.hex}`);
  }
  return withRelayDirectives(response, directives);
}
