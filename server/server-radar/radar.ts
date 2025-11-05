import type { Server } from "@data/servers";
import { DebianMirrorsRadar } from "./mirrors-debian";
import { UbuntuMirrorsRadar } from "./mirrors-ubuntu";

export interface Radar {
  id: string;
  link: string;
  name: string;
  description: string;
  endpoints: string[];

  scan: () => Promise<Server[]>;
}

export type RadarInfo = Omit<Radar, "scan">;

export const RadarList: Radar[] = [];
export const RadarByEndpointName = (name: string) =>
  RadarList.find((r) => r.endpoints.includes(name));

const _add = (radar: Radar) => RadarList.push(radar);

_add(DebianMirrorsRadar);
_add(UbuntuMirrorsRadar);
