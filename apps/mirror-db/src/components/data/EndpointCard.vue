<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NTag, NSpace } from 'naive-ui'
import { IconURL } from '@/utils/icon'
import * as md from 'mirror-data'
import BtnCopy from '@/components/common/BtnCopy.vue'
import BtnLink from '@/components/common/BtnLink.vue'
import BtnRoute from '@/components/common/BtnRoute.vue'
import AptServerStats from '@/components/data/ServerStatsApt.vue'

const props = defineProps<{ endpoint: md.Endpoint }>()

const cardStyle = computed(() => {
  if (!props.endpoint.brand) return {}
  return { '--bg-image': `url('${IconURL(props.endpoint.brand)}')` }
})
</script>

<template>
  <NCard :style="cardStyle" class="endpoint-card isolate">
    <template #header>
      <NSpace align="center">
        <h2 class="text-lg font-bold">{{ endpoint.name }}</h2>
        <p class="text-sm text-gray-500">{{ endpoint.path }}</p>
        <BtnLink v-if="endpoint.link" :href="endpoint.link" text icon ghost />
      </NSpace>
    </template>
    <template #header-extra>
      <NSpace>
        <NTag v-if="endpoint.tags" v-for="tag in endpoint.tags" :key="tag" type="info">
          {{ tag }}
        </NTag>
      </NSpace>
    </template>
    <p class="text-gray-600">{{ endpoint.description }}</p>
    <AptServerStats v-if="endpoint.tags?.includes(md.EndpointTag.APT)" :endpoint="endpoint" class="mt-4" />
    <template #action>
      <NSpace justify="space-between" align="center">
        <NSpace>
          <BtnRoute label="Servers" :to="{ path: '/servers', query: { endpoint: endpoint.name.toLowerCase() } }"
            type="primary" ghost />
        </NSpace>
        <NSpace>
          <BtnCopy :content="endpoint.id" label="Copy ID" />
        </NSpace>
      </NSpace>
    </template>
  </NCard>
</template>

<style scoped>
.endpoint-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  background-image: var(--bg-image);
  background-repeat: no-repeat;
  background-position: right 1.5rem center;
  background-size: contain;
  opacity: 0.3;
  z-index: -1;
}
</style>
