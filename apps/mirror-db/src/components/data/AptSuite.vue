<script setup lang="ts">
import type { Release } from 'apt-parser'
import { NDescriptions, NDescriptionsItem, NTag, NSpace } from 'naive-ui'
import type { CollectionStats } from "mirror-types";
import FileSetStats from '@/components/data/FileSetStats.vue'

defineProps<{
  suite: Release
  stats: CollectionStats
}>()

const kv_keys: (keyof Release)[] = [
  'origin',
  'label',
  'codename',
  'version',
  'date',
  'description',
]
</script>

<template>
  <NDescriptions label-placement="left" :column="1" size="small">
    <NDescriptionsItem label="stats">
      <FileSetStats :stats="stats" />
    </NDescriptionsItem>
    <NDescriptionsItem v-for="key in kv_keys" :key="key" :label="key">
      {{ suite[key] }}
    </NDescriptionsItem>
    <NDescriptionsItem label="architectures">
      <NSpace size="small">
        <NTag v-for="item in suite.architectures" :key="item" size="small">{{ item }}</NTag>
      </NSpace>
    </NDescriptionsItem>
    <NDescriptionsItem label="components">
      <NSpace size="small">
        <NTag v-for="item in suite.components" :key="item" size="small">{{ item }}</NTag>
      </NSpace>
    </NDescriptionsItem>
  </NDescriptions>
</template>
