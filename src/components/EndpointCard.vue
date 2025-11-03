<script setup lang="ts">
import { Endpoint } from '../../data/endpoints'
import { NCard, NTag, NButton, NSpace, NIcon } from 'naive-ui'
import {
  Link as IconLink
} from '@vicons/tabler'
import { computed } from 'vue'
import { IconURL } from '@/utils/icon'
import AptServerStats from './AptServerStats.vue'
import { EndpointTag } from '../../data/endpoints-tags'
import CopyButton from './CopyButton.vue'
import { RouterLink } from 'vue-router'

const props = defineProps<{ endpoint: Endpoint }>()

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
    <AptServerStats v-if="endpoint.tags?.includes(EndpointTag.APT)" :endpoint="endpoint" class="mt-4" />
    <template #action>
      <NSpace justify="space-between" align="center">
        <NSpace>
          <NButton v-if="endpoint.link" tag="a" :href="endpoint.link" target="_blank" type="primary" ghost>
            <template #icon>
              <IconLink />
            </template>
            Official Site
          </NButton>
          <RouterLink :to="{ path: '/servers', query: { endpoint: endpoint.name.toLowerCase() } }">
            <NButton type="primary" ghost>
              Servers
            </NButton>
          </RouterLink>
        </NSpace>
        <NSpace>
          <CopyButton :content="endpoint.id" label="Copy ID" />
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
