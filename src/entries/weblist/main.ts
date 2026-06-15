import "./index.css";

import { createApp } from "vue";
import WebList from "./WebList.vue";

declare const PAGE_META: {
  path: string;
  entries: Array<{
    name: string;
    href: string;
    type: "file" | "directory";
    lastModified: string | null;
    size: number | null;
  }>;
};

const app = createApp(WebList, {
  path: PAGE_META.path,
  entries: PAGE_META.entries,
});

app.mount("#app");
