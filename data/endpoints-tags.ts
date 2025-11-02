export enum EndpointTag {
  APT = "apt",
  Debian = "debian",
}

export const EndpointTags: { tag: EndpointTag; description: string }[] = [
  { tag: EndpointTag.APT, description: "APT Repository" },
  { tag: EndpointTag.Debian, description: "Debian Linux" },
];
