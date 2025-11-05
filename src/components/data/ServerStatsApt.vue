<script setup lang="ts">
import { computed } from 'vue'
import type { Release } from 'apt-parser'
import { useFetch } from '@vueuse/core'
import { NSpin, NAlert, NCollapse, NCollapseItem, NSpace } from 'naive-ui'
import type { Server } from '@data/servers'
import type { Endpoint } from '@data/endpoints'
import type { ServerStatResult } from '@server/server-analytics/api'
import AptSuiteTag from '@/components/data/AptSuiteTag.vue'
import FileSetStats from '@/components/data/FileSetStats.vue'

const props = defineProps<{
  endpoint?: Endpoint,
  server?: Server,
}>()

const url = computed(() => {
  if (props.server) {
    return `/api/servers/${props.server.id}/stats`
  }
  if (props.endpoint) {
    return `/api/endpoints/${props.endpoint.name}/stats`
  }
  return ''
})

const { data, isFetching, error, statusCode } = useFetch(url, { refetch: true }).json<ServerStatResult>()

const suitesByCodename = computed(() => {
  if (!data.value?.suites) {
    return {}
  }
  return data.value.suites.reduce(
    (acc, suite) => {
      const codename = suite.codename || 'unknown'
      acc[codename] ??= []
      acc[codename].push(suite)
      return acc
    },
    {} as Record<string, Release[]>,
  )
})
</script>

<template>
  <div v-if="url">
    <div v-if="isFetching">
      <NSpin />
    </div>
    <div v-else-if="error">
      <NAlert type="error" title="Error">
        Failed to load stats: {{ error.message }} ({{ statusCode }})
      </NAlert>
    </div>
    <div v-else-if="data">
      <NCollapse>
        <NCollapseItem>
          <template #header>
            <FileSetStats :stats="data.stats" />
          </template>
          <NSpace vertical :size="12" class="p-2">
            <NSpace v-for="(suites, codename) in suitesByCodename" :key="codename" align="center">
              <span class="font-semibold text-sm">{{ codename }}:</span>
              <AptSuiteTag v-for="suite in suites" :key="suite.suite" :suite="suite"
                :stats="data.suitesStats[suite.suite || '']" />
            </NSpace>
          </NSpace>
        </NCollapseItem>
      </NCollapse>
    </div>
  </div>
</template>
