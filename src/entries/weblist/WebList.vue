<template>
  <div class="min-h-screen bg-background text-foreground font-sans text-sm px-4 py-5">
    <div class="max-w-4xl mx-auto">
      <div class="px-4 py-2 text-sm text-muted-foreground font-mono">/{{ path }}</div>

      <div class="border border-border bg-card overflow-hidden">
        <!-- Header -->
        <div
          class="grid grid-cols-[1fr_10rem_5rem] gap-2 px-4 py-2 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border"
        >
          <span>Name</span>
          <span class="hidden sm:block">Modified</span>
          <span class="text-right">Size</span>
        </div>

        <!-- File list -->
        <div class="divide-y divide-border/40">
          <a
            v-for="entry in entries"
            :key="entry.name"
            :href="entry.href"
            class="grid grid-cols-[1fr_10rem_5rem] gap-2 px-4 py-2 items-center hover:bg-accent/50 transition-colors"
          >
            <span class="flex items-center gap-2 min-w-0">
              <span class="shrink-0">{{ entry.type === "directory" ? "📁" : "📄" }}</span>
              <span class="truncate">{{ entry.name }}{{ entry.type === "directory" ? "/" : "" }}</span>
            </span>
            <span class="text-muted-foreground tabular-nums text-xs hidden sm:block">
              {{ formatDate(entry.lastModified) }}
            </span>
            <span class="text-muted-foreground tabular-nums text-xs text-right">
              {{ entry.size != null ? formatSize(entry.size) : "" }}
            </span>
          </a>
        </div>
      </div>

      <div class="mt-2 px-1 text-xs text-muted-foreground/60">
        {{ entries.length }} items
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { format } from "date-fns";
import prettyBytes from "pretty-bytes";
import type { WebListEntry } from "@server/pkgs/web-list/types";

defineProps<{
  entries: (Omit<WebListEntry, "lastModified"> & { lastModified: string | null })[];
  path: string;
}>();

function formatDate(raw: string | null): string {
  if (!raw) return "";
  const d = new Date(raw);
  if (isNaN(d.getTime())) return "";
  return format(d, "yyyy-MM-dd HH:mm");
}

function formatSize(bytes: number): string {
  return prettyBytes(bytes);
}
</script>
