export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
}

export interface Mirror {
  name: string;
  host?: string;
  path?: string;
  fetch(request: Request): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
