<script setup lang="ts">
import { Server } from '../../data/servers'
import { NCard, NTag, NSpace } from 'naive-ui'
import CopyButton from './CopyButton.vue'
import AptServerStats from './AptServerStats.vue'
import { EndpointsList } from '../../data/endpoints'
import { computed } from 'vue'

const props = defineProps<{ server: Server }>()

const endpoint = computed(() => {
  return EndpointsList.find((e) => e.name === props.server.endpoint_name)
})
</script>

<template>
  <NCard>
    <template #header>
      <NSpace align="center">
        <h2 class="text-lg font-bold">{{ server.url }}</h2>
        <CopyButton :content="server.url" icon text />
      </NSpace>
    </template>
    <template #header-extra>
      <NSpace>
        <NTag type="info">Tier: {{ server.tier }}</NTag>
        <NTag v-for="tag in server.tags" :key="tag" type="info">
          {{ tag }}
        </NTag>
      </NSpace>
    </template>
    <p class="text-gray-600" v-if="server.host">Host: {{ server.host }}</p>
    <p class="text-gray-600" v-if="server.remark">{{ server.remark }}</p>
    <AptServerStats :server="server" class="mt-4" />

    <template #action>
      <NSpace justify="end" align="center">
        <CopyButton :content="server.id" icon label="Copy ID" />
      </NSpace>
    </template>
  </NCard>
</template>

<style scoped>
.server-card {
  border: 1px solid #ccc;
  border-radius: 8px;
  padding: 16px;
  margin: 8px;
}

.tags {
  margin-top: 8px;
}

.tag {
  display: inline-block;
  background-color: #eee;
  border-radius: 4px;
  padding: 2px 6px;
  margin-right: 4px;
}
</style>
