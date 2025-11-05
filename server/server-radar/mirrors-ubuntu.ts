import { Server } from "@data/servers";
import { type Radar } from "./radar";
import { subfetch } from "../server-analytics/utils";
import bytes from "bytes";

export const UbuntuMirrorsRadar: Radar = {
  id: "b57b26ab-04d1-40e7-99a0-5e251cdfcf4c",
  name: "Official Ubuntu Archive Mirrors Scanner",
  link: "https://launchpad.net/ubuntu/+archivemirrors",
  description:
    "Scan Ubuntu mirrors from launchpad.net. Filter all server has http or https protocol, faster then 100Mbps, updated to lastest, and pick top 100 by speed.",
  endpoints: ["ubuntu"],
  scan: ScanUbuntuMirrors,
};

interface MirrorInfo {
  country: string;
  name: string;
  url: string;
  score: number;
}

const speedLimit: number = bytes("100Mb") ?? 0;

async function ScanUbuntuMirrors(): Promise<Server[]> {
  const resp = await subfetch("https://launchpad.net/ubuntu/+archivemirrors");
  if (!resp.ok) {
    throw new Error(`Failed to fetch ${resp.url}: ${resp.statusText}`);
  }

  const html = await resp.text();
  const table = html
    .replace(/\s*\n\s*/g, "")
    ?.match(/<table.+?id="mirrors_list".*?>.*?<tbody>(.+?)<\/tbody>/ms)?.[1];
  if (!table) throw new Error("Parse Failed: No rows found");

  const sections = table.split(/<tr[^<]+?"section-break".+?<\/tr>/);

  let mirrors: MirrorInfo[] = [];
  for (const section of sections) {
    const [head, ...lines] = section.split("</tr>");
    if (!head) continue;
    let country = head.match(/>([^<>]+)</)?.[1];
    if (!country) continue;
    country = country
      .toLowerCase()
      .replace("'s", "s")
      .replace(/[^a-z0-9]/g, "-");

    for (const line of lines) {
      const [_name, _url, _speed, _updated] = line.split("</td>");
      if (!_updated) continue;
      if (!_updated.includes("Up to date")) continue;
      const name = _name?.match(/>([^<>]+)</)?.[1];
      if (!name) continue;
      let url =
        _url?.match(/<a[^<>]*href="([^<>"]+)"[^<>]*>http<\/a>/)?.[1] ||
        _url?.match(/<a[^<>]*href="([^<>"]+)"[^<>]*>https<\/a>/)?.[1];
      if (!url) continue;
      const speedstr = _speed?.match(/>([0-9.]+ \w+)ps$/)?.[1];
      if (!speedstr) continue;
      const score: number = (bytes(speedstr) as any as number) ?? 0;
      if (score < speedLimit) continue;
      mirrors.push({ country, name, url, score });
    }
  }

  mirrors = mirrors.sort((a, b) => a.score - b.score);
  mirrors = mirrors.slice(0, 100);

  return mirrors.map(
    ({ url, name, country }) =>
      new Server({
        id: crypto.randomUUID(),
        endpoint_name: "ubuntu",
        url: url,
        tier: 10,
        host: "",
        tags: [`region:${country}`],
        remark: name,
      })
  );
}
