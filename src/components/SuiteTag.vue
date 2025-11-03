<script setup lang="ts">
import { NPopover, NTag, NCard } from 'naive-ui'
import type { Release } from 'apt-parser'
import AptSuite from './AptSuite.vue'
import type { CollectionStats } from '../../server/server-analytics/utils'

defineProps<{
  suite: Release
  stats?: CollectionStats
}>()
</script>

<template>
  <NPopover trigger="hover">
    <template #trigger>
      <NTag>{{ suite.suite || suite.codename }}</NTag>
    </template>
    <AptSuite v-if="stats" :suite="suite" :stats="stats" />
  </NPopover>
</template>

<style scoped></style>
