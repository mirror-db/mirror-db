import { EndpointTag } from "./endpoints-tags";

export class Endpoint {
  id?: string;
  name?: string;
  domain?: string;
  path?: string;
  tags?: EndpointTag[];
  icon?: string;
  description?: string;
  link?: string;
  health_check_path?: string;
  speedtest_path?: string;

  constructor(options: Endpoint) {
    Object.assign(this, options);
  }
}

export const Endpoints: Endpoint[] = [];

Endpoints.push({
  id: "7fcde656-2cbd-4996-9538-5c425f965161",
  name: "debian",
  domain: "",
  path: "/debian/",
  tags: [EndpointTag.APT, EndpointTag.Debian],
  icon: "debian",
  description: "Debian APT Repository",
  link: "http://deb.debian.org/debian/",
  health_check_path: "dists/bullseye/InRelease",
  speedtest_path:
    "pool/main/r/r-bioc-genelendatabase/r-bioc-genelendatabase_1.26.0-1_all.deb",
});
