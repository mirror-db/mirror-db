import type { RelayDirective } from "./relay-control";

export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
}

export interface MountContext {
  /**
   * Path within the mirror's mount, excluding the mount name and query. No
   * leading slash is added; the original pathname's case, escaping and empty
   * segments are preserved.
   */
  readonly relativePath: string;
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
  fetch(request: Request, context: MountContext): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
