<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NTag, NSpace } from 'naive-ui'
import { Server } from '@data/servers'
import { EndpointsList } from '@data/endpoints'
import BtnCopy from '@/components/common/BtnCopy.vue'
import ServerStatsApt from '@/components/data/ServerStatsApt.vue';

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
        <BtnCopy :content="server.url" icon text />
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
    <ServerStatsApt :server="server" class="mt-4" />

    <template #action>
      <NSpace justify="end" align="center">
        <BtnCopy :content="server.id" icon label="Copy ID" />
      </NSpace>
    </template>
  </NCard>
</template>
