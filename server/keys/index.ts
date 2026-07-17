/**
 * Key proxy — served under `/keys/` on the bare domain.
 *
 * Fronts common PGP signing keys and SSH public-key lists so domestic clients
 * can fetch them through the mirror instead of reaching upstreams directly.
 *
 * Routes (under `/keys/`):
 * - `/debian/:name`   → Debian archive keys by codename/version (see debian.ts)
 * - `/apt/:name`      → well-known APT repo keys (see well-known-apt.ts)
 * - `/keybase/:name`  → `keybase.io/<name>/pgp_keys.asc`
 * - `/gh|github/:name`→ `github.com/<name>.keys` (SSH)
 * - `/gitlab/:name`   → `gitlab.com/<name>.keys` (SSH)
 *
 * Each PGP route also exposes `/:name/{asc,pgp,info}` and a `POST /:name`
 * cleartext-signature verifier; SSH routes expose `/:name/info`.
 */

import { Hono } from "hono";

import { SrvPGPKey } from "./utils/pgp-key";
import { SrvSSHKeyList } from "./utils/ssh-key";

import { debianKeyName2Url } from "./pgp/debian";
import { repo2KeyUrl } from "./pgp/well-known-apt";

export const keys = new Hono().basePath("/keys");

keys.route("/debian", SrvPGPKey(debianKeyName2Url));
keys.route("/apt", SrvPGPKey(repo2KeyUrl));
keys.route(
  "/keybase",
  SrvPGPKey((n) => `https://keybase.io/${n}/pgp_keys.asc`)
);

keys.route(
  "/gh",
  SrvSSHKeyList((n) => `https://github.com/${n}.keys`)
);
keys.route(
  "/github",
  SrvSSHKeyList((n) => `https://github.com/${n}.keys`)
);
keys.route(
  "/gitlab",
  SrvSSHKeyList((n) => `https://gitlab.com/${n}.keys`)
);
