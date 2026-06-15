import { fileURLToPath, URL } from "node:url";
import { relative } from "node:path";
import { globSync } from "node:fs";
import type { Plugin } from "vite";

import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import vueDevTools from "vite-plugin-vue-devtools";

import { cloudflare } from "@cloudflare/vite-plugin";
import tailwindcss from "@tailwindcss/vite";

const root = fileURLToPath(new URL(".", import.meta.url));
const entriesDir = "src/entries";

// Auto-discover HTML entries in src/entries/<dir>/<file>.html
// src/entries/weblist/index.html → output at weblist/index.html
// src/entries/foo/bar.html       → output at foo/bar.html
const entryFiles = globSync(`${entriesDir}/**/*.html`, { cwd: root });
const entries: Record<string, string> = {
  main: fileURLToPath(new URL("./index.html", import.meta.url)),
};
const htmlRemap: Record<string, string> = {};

for (const file of entryFiles) {
  const rel = relative(entriesDir, file); // e.g. "weblist/index.html"
  const name = rel.replace(/[\/\\]/g, "-").replace(/\.html$/, "");
  entries[name] = fileURLToPath(new URL(file, import.meta.url));
  htmlRemap[file] = rel;
}

/**
 * Remap HTML output paths in the bundle.
 */
function htmlOutputRemap(map: Record<string, string>): Plugin {
  return {
    name: "html-output-remap",
    enforce: "post",
    apply: "build",
    generateBundle(_, bundle) {
      for (const [src, dest] of Object.entries(map)) {
        if (bundle[src]) {
          bundle[src].fileName = dest;
        }
      }
    },
  };
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    vue(),
    vueDevTools(),
    tailwindcss(),
    cloudflare(),
    htmlOutputRemap(htmlRemap),
  ],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      "@server": fileURLToPath(new URL("./server", import.meta.url)),
    },
  },
  environments: {
    client: {
      build: {
        rollupOptions: {
          input: entries,
        },
      },
    },
  },
  server: {
    allowedHosts: ["localhost", "mirs.uk", "*.mirs.uk"],
  },
});
