/**
 * Credential storage for upstream registries.
 *
 * Credentials are persisted in KV as JSON. They are written by the admin
 * `login` flow and read by the proxy when authenticating to an upstream. The
 * password never leaves this module except inside an outbound `Authorization`
 * header — it must never be logged or returned to clients.
 */

export interface Credentials {
  username: string;
  password: string;
}

/** Minimal KV surface used here — keeps the module testable with a mock. */
export interface KvLike {
  get(key: string): Promise<string | null>;
  put(key: string, value: string, options?: { expirationTtl?: number }): Promise<void>;
}

export async function getCreds(
  kv: KvLike,
  key: string,
): Promise<Credentials | null> {
  const raw = await kv.get(key);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<Credentials>;
    if (typeof parsed.username === "string" && typeof parsed.password === "string") {
      return { username: parsed.username, password: parsed.password };
    }
  } catch {
    // fall through
  }
  return null;
}

export async function putCreds(
  kv: KvLike,
  key: string,
  creds: Credentials,
): Promise<void> {
  await kv.put(key, JSON.stringify(creds));
}

/** Build an HTTP Basic `Authorization` header value from credentials. */
export function basicAuthHeader(creds: Credentials): string {
  const token = btoa(`${creds.username}:${creds.password}`);
  return `Basic ${token}`;
}
