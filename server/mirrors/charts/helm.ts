/** Helm repository `index.yaml` schema (subset we touch). */

export interface Maintainer {
  name: string;
  email: string;
  url: string;
}

export interface Dependency {
  name: string;
  version: string;
  repository: string;
  condition: string;
  tags: string[];
  importValues: unknown[];
  alias: string;
  enabled?: boolean;
}

export interface Chart {
  apiVersion: string;
  appVersion: string;
  created: string;
  description: string;
  digest: string;
  engine: string;
  home: string;
  icon: string;
  keywords: string[];
  maintainers: Maintainer[];
  name: string;
  sources: string[];
  urls: string[];
  version: string;
  deprecated?: boolean;
  /** helm schema: `map[string]string` — values must serialize as strings. */
  annotations?: Record<string, string>;
  dependencies?: Dependency[];
}

export interface Repo {
  apiVersion: string;
  entries: { [key: string]: Chart[] };
  generated: string;
}
