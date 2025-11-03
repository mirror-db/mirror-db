export enum ServerTag {
  Origin = "origin",
  CdnCloudflare = "cdn:cloudflare",
  CdnAkamai = "cdn:akamai",
  CdnFastly = "cdn:fastly",
}

export const ServerTagList: { tag: ServerTag; description: string }[] = [];

const _add = (tag: ServerTag, description: string) =>
  ServerTagList.push({ tag, description });

_add(ServerTag.Origin, "Original Server: official, first release site");
_add(ServerTag.CdnCloudflare, "Hosted on Cloudflare");
_add(ServerTag.CdnAkamai, "Hosted on Akamai");
_add(ServerTag.CdnFastly, "Hosted on Fastly");
