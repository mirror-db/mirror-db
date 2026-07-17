/**
 * Per-repo state holder — the data layer for one mirrored Helm repo. Fetches +
 * parses the upstream `index.yaml`, tracks the uuid → tarball-url map, and
 * renders a rewritten index on demand. No HTTP concerns live here: it returns
 * strings and urls; `index.ts` wraps them in `Response`s.
 *
 * Rewrites applied by `render`:
 * - chart `urls` → `<base>/charts/<repo>/res/<uuid>/<filename>` (helm pulls the
 *   tarball back through this mirror; `resolveRes` maps the uuid to the url).
 * - dependency `repository` we also mirror → `<base>/charts/<name>`.
 *
 * Dependency repos we do *not* mirror are left untouched but recorded in
 * `unknownDeps` — discovery candidates to add to `repos.ts` (see `HelmRegistry`).
 */

import { ezfetch } from "@server/pkgs/fetch";
import { load as yamlLoad, dump as yamlDump, YAML11_SCHEMA } from "js-yaml";
import { v5 as uuidv5, NIL as NIL_UUID } from "uuid";
import urlJoin from "url-join";

import type { HelmRepo } from "./repos";
import type { HelmRegistry } from "./registry";
import type { Chart, Repo } from "./helm";

// Parse and re-emit under YAML11 — the same dialect helm reads (sigs.k8s.io/yaml
// → yaml.v2, YAML 1.1) — so types round-trip faithfully: `deprecated: true`
// stays a bool, a quoted `version: "1.10"` stays a string (and dumps re-quoted
// so it isn't reread as the float 1.1). Values are parsed as their real types;
// only `annotations` gets special handling (see `stringifyAnnotations`).
const LOAD_OPTS = { schema: YAML11_SCHEMA } as const;
const DUMP_OPTS = { schema: YAML11_SCHEMA, lineWidth: -1 } as const;

// `annotations` is `map[string]string` in helm's schema — a bare `true`/`3`
// value parses as bool/number and then fails "cannot unmarshal bool into ...
// annotations of type string". Force every value to a string so the dump quotes
// it. Everything else keeps its natural type.
function stringifyAnnotations(chart: Chart) {
  const ann = chart.annotations;
  if (!ann) return;
  for (const key in ann) ann[key] = String(ann[key]);
}

/** Trim trailing slashes so upstream repo urls compare equal regardless of form. */
export const normalizeRepoUrl = (url: string) => url.replace(/\/+$/, "");

/** A chart tarball url resolved to its opaque, mirror-local addressing. */
interface ResolvedRes {
  /** Absolute upstream url (the `resMap` value used to fetch the tarball). */
  abs: string;
  /** `uuidv5(abs)` — stable across isolates/repos for the same url. */
  uuid: string;
  /** Trailing path segment, preserved in the rewritten url for readability. */
  filename: string;
}

export class HelmRepoKeeper {
  constructor(
    public readonly repoInfo: HelmRepo,
    private readonly registry: HelmRegistry,
  ) {}

  private initialized?: Promise<void>;
  /** Parsed upstream index, kept verbatim — rewriting happens at serve time. */
  private repo?: Repo;
  private resMap: Record<string, string> = {};
  /** Rendered index per mirror base; invalidated when `generated` changes. */
  private dumpCache: Record<string, string> = {};

  /** Upstream `generated` timestamp; doubles as the change/cache marker. */
  generated?: string;
  /** http(s) dependency repos referenced here that we don't yet mirror. */
  readonly unknownDeps = new Set<string>();

  /** Resolve a (possibly relative) upstream url to its mirror-local addressing. */
  private static resolveRes(rawUrl: string, upstreamBase: string): ResolvedRes {
    const abs = new URL(rawUrl, upstreamBase).toString();
    return { abs, uuid: uuidv5(abs, NIL_UUID), filename: abs.split("/").pop() ?? "" };
  }

  async refresh() {
    const resp = await ezfetch(this.repoInfo.url, ["index.yaml"]);
    if (!resp.ok) {
      throw new Error(
        `upstream index.yaml fetch failed: ${resp.status} ${this.repoInfo.url}`,
      );
    }

    const repo = yamlLoad(await resp.text(), LOAD_OPTS) as Repo;
    if (this.generated === repo.generated) return;

    // Walk the parsed index once to (a) build the uuid → absolute-url map and
    // (b) collect un-mirrored dependency repos as discovery candidates. The
    // tree itself is kept verbatim and rewritten per request in `render`.
    const upstreamBase = urlJoin(this.repoInfo.url, "/");
    const resMap: Record<string, string> = {};
    const unknownDeps = new Set<string>();
    for (const chartName in repo.entries) {
      for (const chart of repo.entries[chartName]) {
        stringifyAnnotations(chart);
        for (const url of chart.urls ?? []) {
          const { uuid, abs } = HelmRepoKeeper.resolveRes(url, upstreamBase);
          resMap[uuid] = abs;
        }
        for (const dep of chart.dependencies ?? []) {
          const repoUrl = dep.repository ?? "";
          // Only real repo urls are candidates — skip `@alias`, `file://`, blank.
          if (!/^https?:\/\//i.test(repoUrl)) continue;
          if (this.registry.mirrorNameFor(repoUrl)) continue; // already mirrored
          unknownDeps.add(normalizeRepoUrl(repoUrl));
        }
      }
    }

    this.resMap = resMap;
    this.repo = repo;
    this.generated = repo.generated;
    this.dumpCache = {};
    this.unknownDeps.clear();
    for (const u of unknownDeps) this.unknownDeps.add(u);
  }

  /** Ensure the index has been fetched at least once (for `res`/`generated`). */
  waitInitialized() {
    if (this.repo) return Promise.resolve();
    this.initialized ??= this.refresh().catch((e) => {
      this.initialized = undefined;
      throw e;
    });
    return this.initialized;
  }

  /** Absolute upstream tarball url for a `res/<uuid>`, or undefined if unknown. */
  resolveRes(uuid: string): string | undefined {
    return this.resMap[uuid];
  }

  /** Chart summaries for the repo root JSON response. */
  chartSummaries(): { name: string; latest: string }[] {
    if (!this.repo) return [];
    const summaries: { name: string; latest: string }[] = [];
    for (const chartName in this.repo.entries) {
      const versions = this.repo.entries[chartName];
      if (!versions.length) continue;
      // entries are not guaranteed sorted; pick the first non-prerelease
      // version, or fallback to the first entry.
      summaries.push({ name: chartName, latest: versions[0].version });
    }
    return summaries;
  }

  /**
   * Render the index for a given mirror base, rewriting chart `urls` and
   * mirrored dependency `repository` fields to absolute mirror urls. Operates
   * on the parsed tree (no textual find/replace) and caches the dump per base.
   * Requires a prior successful `refresh`/`waitInitialized`.
   *
   * @param mirrorBase Absolute origin the client reached us on (e.g.
   * `https://mirs.uk` or a relay host), so rewritten urls route back through
   * the same host rather than jumping straight to the upstream.
   */
  render(mirrorBase: string): string {
    if (!this.repo) throw new Error("render before refresh");

    const base = mirrorBase.replace(/\/+$/, "");
    const cached = this.dumpCache[base];
    if (cached) return cached;

    const repo = this.repo;
    const upstreamBase = urlJoin(this.repoInfo.url, "/");
    const repoBase = `${base}/charts/${this.repoInfo.name}`;

    const entries: Record<string, Chart[]> = {};
    for (const chartName in repo.entries) {
      entries[chartName] = repo.entries[chartName].map((chart) => {
        const urls = (chart.urls ?? []).map((url) => {
          const { uuid, filename } = HelmRepoKeeper.resolveRes(url, upstreamBase);
          return `${repoBase}/res/${uuid}/${filename}`;
        });

        if (!chart.dependencies) return { ...chart, urls };

        // Point dependency repos we also mirror at their sibling under
        // `/charts/`. `repository` must be a full url helm can append
        // `/index.yaml` to, so emit an absolute one; leave others untouched.
        const dependencies = chart.dependencies.map((dep) => {
          const name = this.registry.mirrorNameFor(dep.repository ?? "");
          return name
            ? { ...dep, repository: `${base}/charts/${name}` }
            : dep;
        });
        return { ...chart, urls, dependencies };
      });
    }

    const dumped = yamlDump({ ...repo, entries }, DUMP_OPTS);
    this.dumpCache[base] = dumped;
    return dumped;
  }
}
