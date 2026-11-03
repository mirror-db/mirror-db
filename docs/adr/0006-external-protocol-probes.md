# ADR 0006 — The mirror set is tested from outside the Worker

- **Status:** rejected (2026-11-03) — `mirrors-test.sh` stays the smoke
  test; its URL list is maintained by hand
- **Date:** 2026-11-03

## Context

Every defect that reached production in the cache and listing paths had the
same shape: the unit tests pass, and the deployed Worker answers differently.
Examples measured on `mirs.uk`:

- `GET /pypi/simple/` stalls without ever completing the body.
- `GET /cargo/` (an undeclared path) is answered by the static-site fallback
  with `200` and `text/html`, so a wrong mount name looks like success.
- Cached bytes and `Content-Length` can disagree, or the transfer can stop
  early (see ADR 0004).
- Cached documents are served in a state the client cannot decode (ADR 0003).

Existing tests are all in-process (`vitest` node project, workerd pool) or
manual (`mirrors-test.sh`, `oci-test.sh`), and the latter only checks status
codes — which is precisely why `200 text/html` and truncated bodies passed.

## Decision

1. **Every mount declares what it answers** — a machine-readable probe set per
   mirror kind lives next to the mirror definition, derived from the same
   taxonomy as ADR 0001.
2. **A probe is a contract, not a smoke test**: it asserts status, content type,
   the `Content-Length`/body agreement, the absence of upstream auth headers,
   the cache-outcome header, and — for listing mounts — both a JSON listing and
   a `PROPFIND`.
3. **Unknown mount names answer 404**, never the site fallback. The fallback
   serves only `/` and client-side routes the front end actually owns.
4. Probes run against a deployed Worker by default, and against a local
   `wrangler dev` in CI; the node/workerd unit suites stay as they are.

## Consequences

- A new mirror cannot ship without an answer to "how do we know it works".
- The smoke script stops being a hand-maintained list of URLs and becomes a
  reader of the same manifest the relay uses.
- The taxonomy (ADR 0001) gets a second consumer, which makes the kind worth
  having.

## Alternatives considered

- *Keep adding URLs to `mirrors-test.sh`.* It is how we got here: status-only
  assertions cannot see truncation, wrong content types or cache transposition.
- *Only rely on the workerd pool tests.* They do not exercise the edge cache or
  the runtime's transcoding behaviour, which is where the defects live.
