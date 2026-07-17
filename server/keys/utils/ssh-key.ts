import { Context, Hono } from "hono";
import SparkMD5 from "spark-md5";

import { cachedfetch } from "@server/pkgs/fetch";

function md5(s: ArrayBuffer): string {
  return SparkMD5.ArrayBuffer.hash(s).replace(/(.{2})(?=.)/g, "$1:");
}

async function hash256(s: ArrayBuffer): Promise<string> {
  const hashBuffer = await crypto.subtle.digest("SHA-256", s);
  const base64 = btoa(String.fromCharCode(...new Uint8Array(hashBuffer)));
  return base64.replace(/=+$/, "");
}

export interface SSHKey {
  key: string;
  type: string;
  comment: string;
  fingerprint: {
    md5: string;
    sha256: string;
  };
}

export async function ParseSSHKey(key: string): Promise<SSHKey> {
  const [type, b64key, comment] = key.split(" ");
  let buf: ArrayBuffer | null = null;

  try {
    buf = Uint8Array.from(atob(b64key), (c) => c.charCodeAt(0)).buffer;
  } catch (error) {
    throw new Error("Invalid key");
  }

  return {
    key,
    type: type.replace("ssh-", ""),
    comment,
    fingerprint: {
      md5: md5(buf),
      sha256: await hash256(buf),
    },
  };
}

export class SSHKeyList {
  keys: SSHKey[];

  constructor(keys: SSHKey[]) {
    this.keys = keys;
  }

  static async fromUrl(url: URL | string) {
    const res = await cachedfetch(typeof url === "string" ? url : url.toString());
    if (!res.ok) throw new Error(`upstream ${res.status}`);
    const data = await res.text();
    const keys = data.split("\n").filter((line) => line.trim() !== "");
    return new SSHKeyList(
      await Promise.all(keys.map((key) => ParseSSHKey(key)))
    );
  }
}

/**
 * Hono sub-app serving a user's public SSH key list resolved by `urlProvider`
 * (e.g. `https://github.com/<name>.keys`).
 *
 * Routes (relative to mount point):
 * - `GET /:keyname`      → raw `authorized_keys`-style text.
 * - `GET /:keyname/info` → parsed keys with md5/sha256 fingerprints.
 */
export function SrvSSHKeyList(urlProvider: (keyname: string) => URL | string) {
  const route = new Hono();

  const getHandle = async (c: Context) => {
    const keyname = c.req.param("keyname");
    if (!keyname) throw new Error("Missing keyname");
    const url = urlProvider(keyname);
    return SSHKeyList.fromUrl(url);
  };

  route.get("/:keyname", async (c) => {
    const h = await getHandle(c);
    return c.text(h.keys.map((key) => key.key).join("\n"));
  });

  route.get("/:keyname/info", async (c) => {
    const h = await getHandle(c);
    return c.json(h.keys);
  });

  return route;
}
