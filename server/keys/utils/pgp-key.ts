import * as openpgp from "openpgp";
import { Hono } from "hono";

import { cachedfetch } from "@server/pkgs/fetch";

export interface PGPPubKeyInfo {
  asc: string;
  algorithm: string;
  bits: number;
  fingerprint: string;
  creationTime: Date;
}

export class PGPPubKeyHandler {
  info: PGPPubKeyInfo;
  key: openpgp.Key;

  constructor(key: openpgp.Key) {
    this.key = key;
    const algo = key.getAlgorithmInfo();
    this.info = {
      asc: key.armor(),
      algorithm: algo.algorithm || "",
      bits: algo.bits || 0,
      fingerprint: key.getFingerprint(),
      creationTime: key.getCreationTime(),
    };
  }

  static async fromAsc(asc: string) {
    const key = await openpgp.readKey({ armoredKey: asc });
    return new PGPPubKeyHandler(key);
  }

  static async fromUrl(url: URL | string) {
    const res = await cachedfetch(typeof url === "string" ? url : url.toString());
    if (!res.ok) throw new Error(`upstream ${res.status}`);
    const asc = await res.text();
    return this.fromAsc(asc);
  }

  async verifyCleartextMessage(cleartextMessage: string) {
    const message = await openpgp.readCleartextMessage({ cleartextMessage });
    return openpgp.verify({ message, verificationKeys: this.key });
  }
}

/**
 * Hono sub-app serving PGP public keys resolved by `urlProvider`.
 *
 * Routes (relative to mount point):
 * - `GET /:keyname`        → key bytes; `keyname` may carry `.pgp`/`.asc` ext.
 * - `GET /:keyname/pgp`    → binary key (`application/pgp-keys`).
 * - `GET /:keyname/asc`    → armored key.
 * - `GET /:keyname/info`   → `{ algorithm, bits, fingerprint, ... }`.
 * - `POST /:keyname`       → verify a cleartext-signed message body.
 */
export function SrvPGPKey(urlProvider: (keyname: string) => URL | string) {
  const route = new Hono();

  const _handler = (kn: string) => PGPPubKeyHandler.fromUrl(urlProvider(kn));

  const param = (c: { req: { param(k: string): string | undefined } }) => {
    const kn = c.req.param("keyname");
    if (!kn) throw new Error("Missing keyname");
    return kn;
  };

  route.get("/:keyname/info", async (c) => {
    const h = await _handler(param(c));
    return c.json(h.info);
  });

  route.get("/:keyname", async (c) => {
    const [keyname, ext] = param(c).split(".");

    const h = await _handler(keyname);
    c.header("Content-Type", "application/pgp-keys");

    switch (ext ?? "pgp") {
      case "pgp":
        return c.body(h.key.write() as any);
      case "asc":
        return c.body(h.info.asc);
      default:
        return c.json({ error: "Invalid extension" }, 400);
    }
  });

  route.get("/:keyname/pgp", async (c) => {
    const h = await _handler(param(c));
    c.header("Content-Type", "application/pgp-keys");
    return c.body(h.key.write() as any);
  });

  route.get("/:keyname/asc", async (c) => {
    const h = await _handler(param(c));
    c.header("Content-Type", "application/pgp-keys");
    return c.body(h.info.asc);
  });

  route.post("/:keyname", async (c) => {
    const h = await _handler(param(c));
    const cleartextMessage = await c.req.text();

    const result = await h.verifyCleartextMessage(cleartextMessage);
    for (const i of result.signatures) {
      if (!(await i.verified.catch(() => false))) continue;
      return c.json({
        keyID: i.keyID,
        signature: await i.signature.catch(() => null),
      });
    }

    return c.json({ error: "No valid signature found" }, 400);
  });

  return route;
}
