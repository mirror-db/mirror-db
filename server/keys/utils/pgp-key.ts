import * as openpgp from "openpgp";
import { Hono } from "hono";

import { cachedfetch } from "@server/pkgs/fetch";

export interface PGPPubKeyInfo {
  asc: string;
  algorithm: string;
  bits: number;
  fingerprint: string;
  creationTime: Date;
  /** Every served certificate, in order (the first is the one detailed above). */
  fingerprints: string[];
}

/** True when the bytes are an ASCII-armored key rather than binary packets. */
const isArmored = (bytes: Uint8Array): boolean => {
  const head = new TextDecoder().decode(bytes.subarray(0, 64));
  return head.includes("-----BEGIN PGP");
};

/**
 * A served PGP public key.
 *
 * The bytes are kept **exactly as the upstream publishes them** and served
 * verbatim: some keys are binary (`application/pgp-keys` packet stream), some
 * are armored, and re-serialising through the parser would silently change what
 * clients install. Parsing is only needed for `/info` and signature
 * verification, so a key the parser rejects is still downloadable.
 */
export class PGPPubKeyHandler {
  private constructor(
    /** Bytes to serve: one or more certificates, in binary when parseable. */
    readonly raw: Uint8Array,
    /** Every certificate we could parse, in the order the upstreams listed them. */
    readonly keys: openpgp.Key[],
    /**
     * The armored form to serve. Re-armoring is lossy (packet ordering, third
     * party certifications), so when every source was already armored its text
     * is served as published.
     */
    readonly armoredText: string | undefined,
  ) {}

  /** Armored key text (as published when possible), or undefined. */
  get armored(): string | undefined {
    return this.armoredText;
  }

  /** Parsed metadata of the first certificate, or undefined when nothing parsed. */
  get info(): PGPPubKeyInfo | undefined {
    const key = this.keys[0];
    if (!key) return;
    const algo = key.getAlgorithmInfo();
    return {
      asc: key.armor(),
      algorithm: algo.algorithm || "",
      bits: algo.bits || 0,
      fingerprint: key.getFingerprint(),
      creationTime: key.getCreationTime(),
      fingerprints: this.keys.map((k) => k.getFingerprint()),
    };
  }

  /** Fetches one or more key files and merges them into a single keyring. */
  static async fromUrls(urls: (URL | string)[]) {
    const fetched = await Promise.all(
      urls.map(async (url) => {
        const res = await cachedfetch(typeof url === "string" ? url : url.toString());
        if (!res.ok) throw new Error(`upstream ${res.status}`);
        return new Uint8Array(await res.arrayBuffer());
      }),
    );

    const keys: openpgp.Key[] = [];
    let allParsed = true;
    for (const bytes of fetched) {
      try {
        // `readKeys` (plural): a published keyring may hold several
        // certificates — Ubuntu's archive keyring has three.
        const parsed = isArmored(bytes)
          ? await openpgp.readKeys({ armoredKeys: new TextDecoder().decode(bytes) })
          : await openpgp.readKeys({ binaryKeys: bytes });
        keys.push(...parsed);
      } catch {
        // Unparseable key (unknown algorithm, no user ID): keep going, and fall
        // back to the published bytes when serving.
        allParsed = false;
      }
    }

    // A keyring is one binary packet stream: normalising every certificate keeps
    // a mixed armored/binary pair from producing a malformed file.
    const raw = allParsed && keys.length
      ? concat(keys.map((k) => k.write()))
      : concat(fetched);

    const armoredText = fetched.every(isArmored)
      ? fetched.map((b) => new TextDecoder().decode(b).trimEnd()).join("\n")
      : keys.length
        ? keys.map((k) => k.armor().trimEnd()).join("\n")
        : undefined;

    return new PGPPubKeyHandler(raw, keys, armoredText);
  }

  async verifyCleartextMessage(cleartextMessage: string) {
    if (!this.keys.length) throw new Error("key could not be parsed for verification");
    const message = await openpgp.readCleartextMessage({ cleartextMessage });
    // Verification succeeds if **any** provided certificate signed the message.
    return openpgp.verify({ message, verificationKeys: this.keys });
  }
}

/**
 * Hono sub-app serving PGP public keys resolved by `urlProvider`.
 *
 * Routes (relative to mount point):
 * - `GET /:keyname`        → key bytes; `keyname` may carry `.pgp`/`.asc` ext.
 * - `GET /:keyname/pgp`    → key as published (`application/pgp-keys`).
 * - `GET /:keyname/asc`    → armored key (parsed keys only).
 * - `GET /:keyname/in-sources` → armored key formatted for a deb822 `Signed-By:`
 *   field (blank lines become `.`, continuation lines are indented).
 * - `GET /:keyname/info`   → `{ algorithm, bits, fingerprint, ... }`.
 * - `POST /:keyname`       → verify a cleartext-signed message body.
 */
export function SrvPGPKey(urlProvider: (keyname: string) => URL | string | (URL | string)[]) {
  const route = new Hono();

  const _handler = (kn: string) => PGPPubKeyHandler.fromUrls([urlProvider(kn)].flat());

  const param = (c: { req: { param(k: string): string | undefined } }) => {
    const kn = c.req.param("keyname");
    if (!kn) throw new Error("Missing keyname");
    return kn;
  };

  route.get("/:keyname/info", async (c) => {
    const h = await _handler(param(c));
    const info = h.info;
    if (!info) return unparsable(c);
    return c.json(info);
  });

  route.get("/:keyname", async (c) => {
    const [keyname, ext] = param(c).split(".");

    const h = await _handler(keyname);
    c.header("Content-Type", "application/pgp-keys");

    switch (ext ?? "pgp") {
      case "pgp":
        return c.body(toBody(h.raw));
      case "asc": {
        const asc = armored(h);
        return asc ? c.body(asc) : unparsable(c);
      }
      default:
        return c.json({ error: "Invalid extension" }, 400);
    }
  });

  route.get("/:keyname/pgp", async (c) => {
    const h = await _handler(param(c));
    c.header("Content-Type", "application/pgp-keys");
    return c.body(toBody(h.raw));
  });

  route.get("/:keyname/in-sources", async (c) => {
    const h = await _handler(param(c));
    const asc = armored(h);
    if (!asc) return unparsable(c);
    // Trailing newline: matches `sed`'s output, so a redirect to a file or a
    // `$(...)` substitution behaves exactly like the documented pipeline.
    return c.text(`${toSourcesFormat(asc)}\n`, 200, {
      "content-type": "text/plain; charset=utf-8",
    });
  });

  route.get("/:keyname/asc", async (c) => {
    const h = await _handler(param(c));
    const asc = armored(h);
    if (!asc) return unparsable(c);
    c.header("Content-Type", "application/pgp-keys");
    return c.body(asc);
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

/**
 * Format an armored key for a deb822 `Signed-By:` field.
 *
 * deb822 continuation lines must be indented and cannot be empty, so the
 * equivalent shell pipeline is
 * `sed -e 's/^$/./' -e '1!s/^/ /'`: blank lines become a lone `.`, and every
 * line but the first is prefixed with one space. The caller supplies the indent
 * for the first line, which is how the whole block ends up aligned:
 *
 *     Signed-By:
 *      $(/keys/apt/gitlab-runner/in-sources)
 */
export function toSourcesFormat(armored: string): string {
  const lines = armored.replace(/\r\n/g, "\n").trimEnd().split("\n");
  return lines
    .map((line, i) => {
      const body = line === "" ? "." : line;
      return i === 0 ? body : ` ${body}`;
    })
    .join("\n");
}

/** All certificates as one armored block, or undefined when nothing parsed. */
function armored(h: PGPPubKeyHandler): string | undefined {
  return h.armored ? `${h.armored}\n` : undefined;
}

/** 502 with a reason, for routes that need a parsed key. */
const unparsable = (c: any) => c.json({ error: "key could not be parsed" }, 502);

/** Concatenate byte arrays. */
function concat(chunks: Uint8Array[]): Uint8Array {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const c of chunks) {
    out.set(c, at);
    at += c.length;
  }
  return out;
}

/** `Uint8Array` → body accepted by Hono (ArrayBuffer keeps binary intact). */
const toBody = (bytes: Uint8Array): ArrayBuffer =>
  bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
