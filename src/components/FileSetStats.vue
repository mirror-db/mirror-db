<script setup lang="ts">
import { NIcon } from 'naive-ui'
import type { CollectionStats } from '../../server/server-analytics/utils'
import { filesize } from 'filesize'
import { Database as IconDatabase, FileText as IconFile } from '@vicons/tabler'

defineProps<{
  stats: CollectionStats
}>()
</script>

<template>
  <div class="flex items-center gap-3">
    <div class="flex items-center gap-1.5">
      <NIcon :component="IconDatabase" class="w-5 h-5 text-blue-500" />
      <span title="Total Size" class="font-medium">{{ filesize(stats.size) }}</span>
      <span v-if="stats.deduplicatedSize !== stats.size" title="Total Size after deduplication by hash"
        class="text-gray-500">
        ({{ filesize(stats.deduplicatedSize) }})
      </span>
    </div>
    <div class="flex items-center gap-1.5">
      <NIcon :component="IconFile" class="w-5 h-5 text-green-500" />
      <span title="Total Files" class="font-medium">{{ stats.files }}</span>
      <span v-if="stats.deduplicatedFiles !== stats.files" title="Total Files after deduplication by hash"
        class="text-gray-500">
        ({{ stats.deduplicatedFiles }})
      </span>
    </div>
  </div>
</template>

<style scoped></style>
