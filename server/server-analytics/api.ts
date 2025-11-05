import { Server } from "@data/servers";
import { Endpoint, EndpointsList } from "@data/endpoints";
import { EndpointTag } from "@data/endpoints-tags";
import { ResolveAptRepo, AnalyzeAptRepo } from "./apt";
import type { CollectionStats } from "./utils";
import type { Release } from "apt-parser";

interface ServerStatBase {
  server: Server;
  endpoint: Endpoint;
  stats?: CollectionStats;
}

interface ServerStatApt extends ServerStatBase {
  stats: CollectionStats;
  suitesStats: Record<string, CollectionStats>;
  suites: Release[];
}

export type ServerStatResult = ServerStatApt;

export async function StatServer(
  server: Server,
  detailed: boolean
): Promise<ServerStatResult | string> {
  const endpoint = EndpointsList.find((e) => e.name === server.endpoint_name);
  if (!endpoint) return "Endpoint not found";

  if (endpoint.tags.includes(EndpointTag.APT)) {
    const suites = await ResolveAptRepo(server.url);

    const result: ServerStatApt = {
      server,
      endpoint,
      suites,
      ...AnalyzeAptRepo(suites),
    };

    if (!detailed) {
      for (const suite of result.suites) {
        delete suite.sha512;
        delete suite.sha256;
        delete suite.sha1;
        delete suite.md5;
      }
    }

    return result;
  } else {
    return "Endpoint not supported";
  }
}
