import { Server } from "@data/servers";
import { type Radar } from "./radar";
import { subfetch } from "../server-analytics/utils";

export const DebianMirrorsRadar: Radar = {
  id: "4409cbe0-9b9b-45a5-a12c-4c146e92dedb",
  name: "Official Debian Mirrors Scanner",
  link: "https://mirror-master.debian.org/status/mirror-status.html",
  description:
    "Scan Debian mirrors from mirror-master.debian.org. Filter all server updated to lastest, have 100 scores, daily updates between 3 and 5, and pick top 100 by delay sigma.",
  endpoints: ["debian"],
  scan: ScanDebianMirrors,
};

interface MirrorInfo {
  url: string;
  sigma: number;
}

async function ScanDebianMirrors(): Promise<Server[]> {
  const resp = await subfetch(
    "https://mirror-master.debian.org/status/mirror-status.html"
  );
  if (!resp.ok) {
    throw new Error(`Failed to fetch ${resp.url}: ${resp.statusText}`);
  }

  const html = await resp.text();
  const rows = html
    .match(/<tbody.+?>(.+)<\/tbody>/ms)?.[1]
    ?.replace(/\s*\n\s*/g, "")
    .split("</tr>");
  if (!rows) throw new Error("Parse Failed: No rows found");

  let mirrors: MirrorInfo[] = [];
  for (const row of rows) {
    const cells = row.split("</td>");
    if (cells.length < 6) continue;

    const url = cells[0]?.match(/<a.+?href="(.+?)project\/trace\/"/)?.[1];
    if (!url) continue;
    if (!cells[1]?.match(/>(current)$/)) continue;
    if (Number(cells[4]?.match(/>([0-9.]+)$/)?.[1]) !== 100) continue;

    const dailyUpdates = cells[5]?.match(
      /<td .+?data-text="([0-9.]+)".*?>/
    )?.[1];
    if (!dailyUpdates) continue;
    if (Number(dailyUpdates) < 3 || Number(dailyUpdates) > 5) continue;

    const _sigma = cells[6]?.match(/<td .+?>.+?\/.+?(\d+:\d+)/)?.[1];
    if (!_sigma) continue;
    const sigma = Number(_sigma.replace(":", "."));

    mirrors.push({ url, sigma });
  }

  mirrors = mirrors.sort((a, b) => a.sigma - b.sigma);

  mirrors = mirrors.slice(0, 100);

  return mirrors.map(
    ({ url }) =>
      new Server({
        id: crypto.randomUUID(),
        endpoint_name: "debian",
        url: url,
        tier: 10,
        host: "",
        tags: [],
      })
  );
}
