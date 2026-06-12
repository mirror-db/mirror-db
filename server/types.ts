export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
}

/**
 * Context passed to every mirror's `fetch`. Carries relay metadata and
 * any future per-request state without polluting the Request itself.
 */
export interface MirrorContext {
  /** If request came through relay, the relay's public host (e.g. "cn.example.com"). */
  relay?: {
    host: string;
  };
}

export interface Mirror {
  name: string;
  host?: string;
  path?: string;
  fetch(request: Request, ctx?: MirrorContext): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
