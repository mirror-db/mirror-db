export enum EndpointTag {
  APT = "apt",
  Debian = "debian",
  Ubuntu = "ubuntu",
}

export const EndpointTagList: { tag: EndpointTag; description: string }[] = [];

const _add = (tag: EndpointTag, description: string) =>
  EndpointTagList.push({ tag, description });

_add(EndpointTag.APT, "APT Repository");
_add(EndpointTag.Debian, "Debian Linux");
_add(EndpointTag.Ubuntu, "Ubuntu Linux");
