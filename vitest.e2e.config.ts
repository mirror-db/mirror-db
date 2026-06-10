import { fileURLToPath } from "node:url";

import { defineConfig } from "vitest/config";

// E2e suite: the test boots the real worker (wrangler dev --remote) in beforeAll
// and exercises it over HTTP against live Docker Hub. Kept separate from the
// fast, offline unit suite (vitest.config.ts).
export default defineConfig({
  test: {
    include: ["server/**/*.e2e.test.ts"],
    testTimeout: 60_000,
    // beforeAll builds the worker and boots `wrangler dev --remote`.
    hookTimeout: 300_000,
    // Single worker dev server shared across files — run serially.
    fileParallelism: false,
  },
  resolve: {
    alias: {
      "@server": fileURLToPath(new URL("./server", import.meta.url)),
    },
  },
});
