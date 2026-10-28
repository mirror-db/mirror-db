import { Context } from "hono";
import { env } from "cloudflare:workers";

/** Header the relay client sends to declare its public hostname. */
const RELAY_HOST_HEADER = "x-mdb-relay-host";

export const relayInfo = (reqOrCtx: Request | Context) => {
  const request = reqOrCtx instanceof Request ? reqOrCtx : reqOrCtx.req.raw;
  const relayHost = request.headers.get(RELAY_HOST_HEADER);
  const baseDomain = relayHost || env.BASE_DOMAIN;
  
  // Build full origin: use relay host if present, else request's own origin
  const origin = relayHost 
    ? `https://${relayHost}`
    : new URL(request.url).origin;
    
  return { baseDomain, origin };
};

export type RelayContext = ReturnType<typeof relayInfo>;
