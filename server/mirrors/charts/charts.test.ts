import { describe, expect, it, vi, beforeEach } from "vitest";

// Mock the fetch helpers before importing the module under test. `ezfetch`
// serves the upstream index.yaml; `cachedfetch` serves tarballs.
vi.mock("@server/pkgs/fetch", () => ({
  ezfetch: vi.fn(),
  cachedfetch: vi.fn(),
  sanitizeResponse: vi.fn((res: Response) => res),
}));

import { charts } from "./index";
import { ezfetch, cachedfetch } from "@server/pkgs/fetch";

const mockEzfetch = ezfetch as ReturnType<typeof vi.fn>;
const mockCachedfetch = cachedfetch as ReturnType<typeof vi.fn>;

function makeRequest(path: string): Request {
  return new Request(`https://mirs.uk${path}`);
}

// A minimal upstream index.yaml with one cross-host absolute url and one
// same-host relative url — exercises both rewrite branches. The `app` chart
// carries a quoted number-like version, a real `deprecated` bool, and
// annotations (quoted + a bare bool) to exercise the type-preserving dump.
const INDEX_YAML = `apiVersion: v1
generated: "2024-01-01T00:00:00Z"
entries:
  gitlab-runner:
    - name: gitlab-runner
      version: 0.66.0
      urls:
        - https://gitlab-charts.s3.amazonaws.com/gitlab-runner-0.66.0.tgz
      dependencies:
        - name: cert-manager
          version: v1.21.0
          repository: https://charts.jetstack.io/
        - name: other
          version: 1.0.0
          repository: https://charts.example.com/other
  cert-manager:
    - name: cert-manager
      version: v1.21.0
      urls:
        - charts/cert-manager-v1.21.0.tgz
  app:
    - name: app
      version: "1.10"
      appVersion: "1.20"
      deprecated: true
      annotations:
        catalog.cattle.io/auto-install: "true"
        catalog.cattle.io/featured: "3"
        catalog.cattle.io/bare-bool: true
      urls:
        - https://example.com/app-1.10.tgz
`;

beforeEach(() => {
  vi.clearAllMocks();
  // Fresh Response per call — the keeper re-fetches the index on each refresh.
  mockEzfetch.mockImplementation(async () => new Response(INDEX_YAML));
});

describe("charts mirror", () => {
  it("has correct name and path", () => {
    expect(charts.name).toBe("charts");
    expect(charts.path).toBe("charts");
  });

  it("returns repo metadata as JSON", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab"));
    expect(res.status).toBe(200);
    const body = (await res.json()) as { name: string; url: string };
    expect(body.name).toBe("gitlab");
    expect(body.url).toBe("https://charts.gitlab.io");
  });

  it("root JSON includes chart list with latest version", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab"));
    const body = (await res.json()) as {
      charts: { name: string; latest: string }[];
    };
    expect(body.charts).toEqual(
      expect.arrayContaining([
        { name: "gitlab-runner", latest: "0.66.0" },
        { name: "app", latest: "1.10" },
      ]),
    );
  });

  it("serves index.yaml as application/yaml for CLI clients", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    expect(res.headers.get("content-type")).toBe("application/yaml");
  });

  it("serves index.yaml as text/plain for browser requests", async () => {
    const req = new Request("https://mirs.uk/charts/gitlab/index.yaml", {
      headers: { accept: "text/html,application/xhtml+xml" },
    });
    const res = await charts.fetch(req);
    expect(res.headers.get("content-type")).toContain("text/plain");
  });

  it("404s on unknown repo", async () => {
    const res = await charts.fetch(makeRequest("/charts/does-not-exist"));
    expect(res.status).toBe(404);
  });

  it("fetches upstream index.yaml from the repo's url", async () => {
    await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    expect(mockEzfetch).toHaveBeenCalledWith("https://charts.gitlab.io", [
      "index.yaml",
    ]);
  });

  it("rewrites chart urls to absolute mirror res/<uuid>/<filename> urls", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("application/yaml");

    const body = await res.text();
    // No upstream urls leak through.
    expect(body).not.toContain("https://gitlab-charts.s3.amazonaws.com");
    expect(body).not.toContain("charts/cert-manager-v1.21.0.tgz");
    // Both charts become full mirror urls preserving the filename.
    expect(body).toMatch(
      /https:\/\/mirs\.uk\/charts\/gitlab\/res\/[0-9a-f-]+\/gitlab-runner-0\.66\.0\.tgz/,
    );
    expect(body).toMatch(
      /https:\/\/mirs\.uk\/charts\/gitlab\/res\/[0-9a-f-]+\/cert-manager-v1\.21\.0\.tgz/,
    );
  });

  it("resolves chart urls to the relay host when relayed", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"), {
      relay: { host: "cn.example.com" },
    });
    const body = await res.text();
    expect(body).toMatch(
      /https:\/\/cn\.example\.com\/charts\/gitlab\/res\/[0-9a-f-]+\/gitlab-runner-0\.66\.0\.tgz/,
    );
    expect(body).not.toContain("https://mirs.uk/charts/gitlab/res/");
  });

  it("rewrites dependency repositories (seed + auto-mounted) to mirror urls", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await res.text();
    // charts.jetstack.io is a seed (`cert-manager`) — matched despite the
    // trailing slash — and rewritten to this mirror's own origin.
    expect(body).toContain("repository: https://mirs.uk/charts/cert-manager");
    expect(body).not.toContain("https://charts.jetstack.io");
    // An un-seeded dep repo is auto-mounted under a url-derived slug and
    // likewise rewritten — no upstream repo url leaks through.
    expect(body).toContain(
      "repository: https://mirs.uk/charts/charts-example-com-other",
    );
    expect(body).not.toContain("https://charts.example.com/other");
  });

  it("serves an auto-mounted dependency repo as its own mirror", async () => {
    // Discover the mount by rendering a seed index that references it.
    await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    // The slug is now routable like any seed repo.
    const res = await charts.fetch(
      makeRequest("/charts/charts-example-com-other"),
    );
    expect(res.status).toBe(200);
    const body = (await res.json()) as { name: string; url: string };
    expect(body.name).toBe("charts-example-com-other");
    expect(body.url).toBe("https://charts.example.com/other");
  });

  it("resolves dependency repositories to the relay host when relayed", async () => {
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"), {
      relay: { host: "cn.example.com" },
    });
    const body = await res.text();
    expect(body).toContain(
      "repository: https://cn.example.com/charts/cert-manager",
    );
    expect(body).not.toContain("https://mirs.uk/charts/cert-manager");
  });

  it("preserves quoted number-like versions as strings", async () => {
    // Parsed and re-emitted under YAML11: a quoted `version: "1.10"` stays a
    // string and dumps re-quoted, so a consumer doesn't reread it as float 1.1.
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await res.text();
    expect(body).toMatch(/version:\s*['"]1\.10['"]/m);
    expect(body).not.toMatch(/version:\s*1\.1(\D|$)/m);
    expect(body).toMatch(/appVersion:\s*['"]1\.20['"]/m);
  });

  it("forces annotation values to quoted strings", async () => {
    // annotations is map[string]string; a value that looks like a bool/number
    // must stay a string or helm fails "cannot unmarshal bool into ... string".
    // Quoted values round-trip; a bare `true` is coerced to a string too.
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await res.text();
    expect(body).toMatch(/catalog\.cattle\.io\/auto-install:\s*['"]true['"]/);
    expect(body).toMatch(/catalog\.cattle\.io\/featured:\s*['"]3['"]/);
    expect(body).toMatch(/catalog\.cattle\.io\/bare-bool:\s*['"]true['"]/);
    expect(body).not.toMatch(/auto-install:\s*true\s*$/m);
  });

  it("keeps genuine bool fields (deprecated) unquoted", async () => {
    // Counterpart to annotation quoting: `deprecated` is a real bool in helm's
    // schema, parsed as a bool and emitted bare. Quoting it would fail "cannot
    // unmarshal string into ... bool".
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await res.text();
    expect(body).toMatch(/deprecated:\s*true\s*$/m);
    expect(body).not.toMatch(/deprecated:\s*['"]true['"]/);
  });

  it("returns 502 when upstream index.yaml is unreachable", async () => {
    mockEzfetch.mockImplementation(async () => new Response("nope", { status: 503 }));
    const res = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    expect(res.status).toBe(502);
  });

  it("serves generated timestamp after index is built", async () => {
    await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const res = await charts.fetch(makeRequest("/charts/gitlab/generated"));
    expect(await res.text()).toBe("2024-01-01T00:00:00Z");
  });

  it("resolves res/<uuid> back to the original upstream url and streams it", async () => {
    // Build the index so the resMap is populated.
    const idx = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await idx.text();
    const uuid = body.match(/res\/([0-9a-f-]+)\/gitlab-runner/)?.[1];
    expect(uuid).toBeTruthy();

    mockCachedfetch.mockResolvedValue(new Response("tarball-bytes"));
    const res = await charts.fetch(
      makeRequest(`/charts/gitlab/res/${uuid}/gitlab-runner-0.66.0.tgz`),
    );

    expect(mockCachedfetch).toHaveBeenCalledWith(
      "https://gitlab-charts.s3.amazonaws.com/gitlab-runner-0.66.0.tgz",
    );
    expect(await res.text()).toBe("tarball-bytes");
  });

  it("exposes the original url via res/<uuid>/info", async () => {
    const idx = await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const body = await idx.text();
    const uuid = body.match(/res\/([0-9a-f-]+)\/cert-manager/)?.[1];

    const res = await charts.fetch(
      makeRequest(`/charts/gitlab/res/${uuid}/info`),
    );
    expect(await res.text()).toBe(
      "https://charts.gitlab.io/charts/cert-manager-v1.21.0.tgz",
    );
  });

  it("404s on unknown res uuid", async () => {
    await charts.fetch(makeRequest("/charts/gitlab/index.yaml"));
    const res = await charts.fetch(
      makeRequest("/charts/gitlab/res/00000000-0000-0000-0000-000000000000/x.tgz"),
    );
    expect(res.status).toBe(404);
  });
});
