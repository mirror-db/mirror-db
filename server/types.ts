export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
}

export interface Mirror {
  name: string;
  host?: string;
  path?: string;
  /** Allow plain HTTP without redirecting to HTTPS (e.g. APT mirrors). */
  keepHTTP?: boolean;
  fetch(request: Request): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
