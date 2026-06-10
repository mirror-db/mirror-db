export interface MirrorStatus {
  health: "initializing" | "healthy" | "unhealthy";
  [key: string]: any;
}

export interface Mirror {
  host?: string;
  path?: string;
  fetch(request: Request, ctx?: ExecutionContext): Promise<Response> | Response;
  status?(): Promise<MirrorStatus> | MirrorStatus;
}
