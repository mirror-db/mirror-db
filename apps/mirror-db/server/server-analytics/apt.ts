import { Release } from "apt-parser";
import { CollectionCounter, subfetch } from "./utils";
import type { CollectionStats } from "mirror-types";

export async function ResolveAptRepo(url: string) {
  const response = await subfetch(url, "dists/");
  if (!response.ok)
    throw new Error(`Failed to fetch ${response.url}: ${response.statusText}`);
  const suitesNames: string[] = [];
  // @ts-ignore
  await new HTMLRewriter()
    .on("a", {
      // @ts-ignore
      element(element) {
        let href = element.getAttribute("href");
        if (!href) return;
        if (href.startsWith("/")) return;
        if (!href.endsWith("/")) return;
        if (href.startsWith("http://") || href.startsWith("https://")) return;

        href = href.replace(/\/$/, "");
        if (href == "." || href == "..") return;

        suitesNames.push(href);
      },
    })
    .transform(response)
    .text();

  const suites: Release[] = [];
  for (const suite of suitesNames) {
    const response = await subfetch(url, "dists", suite, "Release");
    if (!response.ok)
      throw new Error(
        `Failed to fetch ${response.url}: ${response.statusText}`
      );
    const release = new Release(await response.text());
    if (release.suite != suite) continue;
    suites.push(release);
  }

  return suites;
}

export function AnalyzeAptRepo(
  suites: Awaited<ReturnType<typeof ResolveAptRepo>>
) {
  const counter = new CollectionCounter();

  let suitesStats: Record<string, CollectionStats> = {};

  let date = 0;

  for (const suite of suites) {
    if (suite.date) date = Math.max(date, new Date(suite.date).getTime());

    const hashs = suite.sha512 || suite.sha256 || suite.sha1 || suite.md5;
    if (!hashs) continue;

    const suiteCounter = new CollectionCounter(
      hashs.map((hash) => [hash.hash, hash.size])
    );
    counter.merge(suiteCounter);

    if (!suite.suite) continue;
    suitesStats[suite.suite] = suiteCounter.export();
  }

  return {
    updateTime: new Date().toISOString(),
    date: new Date(date).toISOString(),
    stats: counter.export(),
    suitesStats,
  };
}
