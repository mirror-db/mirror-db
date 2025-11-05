<script setup lang="ts">
import { useFetch } from '@vueuse/core'
import { NCard, NButton, NSpace, NSpin, NAlert, NIcon } from 'naive-ui'
import { ListSearch as IconRadar } from '@vicons/tabler'
import type { RadarInfo } from '@server/server-radar/radar'
import type { Server } from '@data/servers'
import BtnCopy from '@/components/common/BtnCopy.vue'
import BtnLink from '@/components/common/BtnLink.vue'

const props = defineProps<{ radar: RadarInfo }>()

const {
  isFetching: loading,
  error,
  data,
  execute,
} = useFetch(`/api/radars/${props.radar.id}/scan`, { immediate: false, initialData: [], }).json<Server[]>()
</script>

<template>
  <NCard>
    <template #header>
      <NSpace align="center">
        <NIcon>
          <IconRadar />
        </NIcon>
        <h2 class="text-lg font-bold">{{ radar.name }}</h2>
        <BtnLink v-if="radar.link" :href="radar.link" text icon ghost />
      </NSpace>
    </template>
    <p class="text-gray-600">{{ radar.description }}</p>

    <div v-if="loading" class="flex justify-center p-4">
      <NSpin />
    </div>
    <div v-if="data && data.length > 0" class="mt-4">
      <NSpace vertical class="w-full">
        <div v-for="server in data" :key="server.id"
          class="w-full flex align-center font-mono px-2 py-1 rounded transition-colors duration-300 hover:bg-gray-200/50">
          <div class="flex-1">
            <p>{{ server.url }}</p>
            <p v-if="server.remark" class="text-gray-500 text-sm wrap-break-word max-w-[90%]!">
              {{ server.remark }}
            </p>
          </div>
          <NSpace>
            <BtnCopy :content="server.url" text icon />
            <BtnLink :href="server.url" text icon />
          </NSpace>
        </div>
      </NSpace>
    </div>
    <div v-if="error" class="flex justify-center p-4">
      <NAlert type="error" :title="error.message" />
    </div>

    <template #action>
      <NSpace justify="end">
        <NButton @click="() => execute()" :loading="loading" type="primary" ghost>Scan</NButton>
      </NSpace>
    </template>
  </NCard>
</template>
