import { fileURLToPath } from "node:url";

import { defineConfig } from "vitest/config";

// Standalone Vitest config: the OCI proxy core is dependency-injected (fetch +
// KV are passed in), so tests run in a plain Node environment without the
// Cloudflare Vite plugin or Workers runtime.
export default defineConfig({
  test: {
    environment: "node",
    include: ["server/**/*.test.ts"],
    exclude: ["server/**/*.e2e.test.ts", "node_modules/**"],
  },
  resolve: {
    alias: {
      "cloudflare:workers": fileURLToPath(
        new URL("./server/test-stubs/cloudflare-workers.ts", import.meta.url),
      ),
      "@server": fileURLToPath(new URL("./server", import.meta.url)),
    },
  },
});
