import type { RelayDirective } from "./relay-control";

export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
}

export interface Mirror {
  name: string;
  host?: string;
  path?: string;
  /** Allow plain HTTP without redirecting to HTTPS (e.g. APT mirrors). */
  keepHTTP?: boolean;
  /**
   * Opaque tags selecting relay policies (see `server/relay-policy.ts`).
   * Tags are not a mirror-kind taxonomy; they only group mirrors for transport
   * selection.
   */
  policyTags?: string[];
  /**
   * `X-MDB-Relay` directives injected on every response by the front-door
   * middleware. Response-side decisions only; these never enter the manifest.
   */
  relayDirectives?: RelayDirective[];
  fetch(request: Request): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
