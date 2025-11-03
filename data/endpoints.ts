import { EndpointTag } from "./endpoints-tags";

export class Endpoint {
  id: string = "";
  name: string = "";
  domain: string = "";
  path: string = "";
  tags: EndpointTag[] = [];
  brand: string = "";
  description: string = "";
  link: string = "";
  health_check_path: string = "";
  speedtest_path: string = "";

  constructor(options: Endpoint) {
    Object.assign(this, options);
  }
}

export const EndpointsList: Endpoint[] = [];
const _add = (options: Endpoint) => EndpointsList.push(new Endpoint(options));

_add({
  id: "7fcde656-2cbd-4996-9538-5c425f965161",
  name: "debian",
  domain: "",
  path: "/debian/",
  tags: [EndpointTag.APT, EndpointTag.Debian],
  brand: "sh:debian",
  description: "Debian APT Repository",
  link: "http://deb.debian.org/debian/",
  health_check_path: "dists/bullseye/InRelease",
  speedtest_path:
    "pool/main/r/r-bioc-genelendatabase/r-bioc-genelendatabase_1.26.0-1_all.deb",
});

_add({
  id: "04835ae5-c58a-4d5f-a866-0cbb5f09c29b",
  name: "ubuntu",
  domain: "",
  path: "/ubuntu/",
  tags: [EndpointTag.APT, EndpointTag.Ubuntu],
  brand: "sh:ubuntu",
  description: "Ubuntu APT Repository",
  link: "http://mirrors.aliyun.com/ubuntu/",
  health_check_path: "dists/plucky/InRelease",
  speedtest_path:
    "pool/main/l/linux-aws/linux-modules-extra-6.14.0-1005-aws_6.14.0-1005.5_amd64.deb",
});
