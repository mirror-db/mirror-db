import pLimit from "p-limit";
import urlJoin from "url-join";

import { MdbMaxConcurrentRequests, MdbUserAgent } from "./const";

const limiter = pLimit(MdbMaxConcurrentRequests);

export const rateLimitedFetch: typeof fetch = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => limiter(() => fetch(input, init));

export const mdbfetch: typeof fetch = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => {
  const req = new Request(input, init);
  if (!req.headers.has("user-agent")) req.headers.set("user-agent", MdbUserAgent);
  return rateLimitedFetch(req);
};

export function buildRequest(reqInfo: RequestInfo | URL, init?: RequestInit): Request;
export function buildRequest(
  reqInfo: RequestInfo | URL,
  parts?: string[],
  init?: RequestInit,
): Request;
export function buildRequest(
  reqInfo: RequestInfo | URL,
  arg2?: RequestInit | string[],
  arg3?: RequestInit,
): Request {
  const req = new Request(reqInfo);
  let parts: string[] = [];
  let init: RequestInit = { redirect: "follow" };

  if (arg2 != undefined) {
    if (Array.isArray(arg2)) parts = arg2;
    else init = arg2;
  }

  if (arg3 != undefined) init = arg3;

  return new Request(urlJoin(req.url, ...parts), init);
}

export function ezfetch(reqInfo: RequestInfo | URL, init?: RequestInit): Promise<Response>;
export function ezfetch(
  reqInfo: RequestInfo | URL,
  parts?: string[],
  init?: RequestInit,
): Promise<Response>;
export async function ezfetch(
  reqInfo: RequestInfo | URL,
  arg2?: RequestInit | string[],
  arg3?: RequestInit,
): Promise<Response> {
  // @ts-ignore - overload dispatch handled by buildRequest
  return mdbfetch(buildRequest(reqInfo, arg2, arg3));
}
