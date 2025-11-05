<script setup lang="ts">
import { NPopover, NTag } from 'naive-ui'
import type { Release } from 'apt-parser'
import AptSuite from '@/components/data/AptSuite.vue'
import type { CollectionStats } from '@server/server-analytics/utils'

defineProps<{
  suite: Release
  stats?: CollectionStats
}>()
</script>

<template>
  <NPopover trigger="hover">
    <template #trigger>
      <NTag>{{ suite.suite }}</NTag>
    </template>
    <AptSuite v-if="stats" :suite="suite" :stats="stats" />
  </NPopover>
</template>
