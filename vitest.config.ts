import { fileURLToPath } from "node:url";

import { defineConfig } from "vitest/config";

// Two projects:
//  - "node": the dependency-injected core (OCI/APT proxies, pure parsers) runs
//    in plain Node with `cloudflare:workers` stubbed. The default for *.test.ts.
//  - "workers": tests that need real Workers globals (HTMLRewriter, caches) run
//    inside workerd via `@cloudflare/vitest-pool-workers`. Opt in by naming the
//    file `*.workers.test.ts`. Defined in vitest.workers.config.ts.
export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "node",
          environment: "node",
          include: ["server/**/*.test.ts"],
          exclude: [
            "server/**/*.e2e.test.ts",
            "server/**/*.workers.test.ts",
            "node_modules/**",
          ],
        },
        resolve: {
          alias: {
            "cloudflare:workers": fileURLToPath(
              new URL("./server/test-stubs/cloudflare-workers.ts", import.meta.url),
            ),
            "@server": fileURLToPath(new URL("./server", import.meta.url)),
          },
        },
      },
      "./vitest.workers.config.ts",
    ],
  },
});
