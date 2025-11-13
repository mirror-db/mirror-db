<script setup lang="ts">
import { computed } from 'vue'
import type { Release } from 'apt-parser'
import { useFetch } from '@vueuse/core'
import { NSpin, NAlert, NCollapse, NCollapseItem, NSpace } from 'naive-ui'
import * as md from 'mirror-data'
import type { ServerStatResult } from '@server/server-analytics/api'
import AptSuiteTag from '@/components/data/AptSuiteTag.vue'
import FileSetStats from '@/components/data/FileSetStats.vue'

const props = defineProps<{
  endpoint?: md.Endpoint,
  server?: md.Server,
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

const groupedSuites = computed(() => {
  if (!data.value?.suites) {
    return {}
  }

  const prefixes: string[] = data.value.suites.map(s => s.codename)
    .filter(i => i !== undefined)
    .map(i => {
      const match = i.match(/^[a-zA-Z0-9]+$/)
      if (!match) return false
      return match[0]
    })
    .filter(i => i !== false)

  const grouped = data.value.suites.reduce(
    (acc, suite) => {
      let groupName = ""
      for (const prefix of prefixes) {
        if (suite.codename?.startsWith(prefix)) {
          groupName = prefix
          break
        }
      }
      acc[groupName] ??= []
      acc[groupName]!.push(suite)
      return acc
    },
    {} as Record<string, Release[]>,
  )

  for (const key of Object.keys(grouped)) {
    const group = grouped[key] as Release[]
    if (group.length === 1) {
      grouped["others"] ??= []
      grouped["others"]!.push(...group)
      delete grouped[key]
    }
  }

  return grouped
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
          <table class="w-full border-separate border-spacing-y-2">
            <tbody>
              <tr v-for="(suites, groupName) in groupedSuites" :key="groupName" class="border-0 hover:bg-gray-200/5">
                <td class="fit-content align-top">
                  <p class="text-bold whitespace-nowrap">{{ groupName }}</p>
                </td>
                <td class="flex flex-wrap gap-2 ml-4">
                  <AptSuiteTag v-for="suite in suites" :key="suite.suite" :suite="suite"
                    :stats="data.suitesStats[suite.suite || '']" />
                </td>
              </tr>
            </tbody>
          </table>
        </NCollapseItem>
      </NCollapse>
    </div>
  </div>
</template>
