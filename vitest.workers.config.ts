import { fileURLToPath } from "node:url";

import { cloudflarePool, cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

// Workers-pool project: `*.workers.test.ts` files run inside workerd, so real
// `HTMLRewriter` / `caches` are available (the web-list listing parser needs
// HTMLRewriter, which has no Node equivalent).
//
// vitest-pool-workers v4 dropped `defineWorkersConfig`: wire the pool by adding
// `cloudflareTest()` as a Vite plugin and `cloudflarePool()` as `test.pool`.
const poolOptions = {
  miniflare: {
    compatibilityDate: "2026-06-10",
    compatibilityFlags: ["nodejs_compat"],
  },
};

export default defineConfig({
  plugins: [cloudflareTest(poolOptions)],
  test: {
    name: "workers",
    include: ["server/**/*.workers.test.ts"],
    pool: cloudflarePool(poolOptions),
  },
  resolve: {
    alias: {
      "@server": fileURLToPath(new URL("./server", import.meta.url)),
    },
  },
});
