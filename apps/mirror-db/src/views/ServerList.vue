<template>
  <h1 class="text-2xl font-bold mb-8">Servers for {{ _endpointName || 'All Endpoints' }}</h1>
  <NSpace vertical :size="12" v-if="filteredServers.length > 0">
    <ServerCard v-for="server in filteredServers" :key="server.id" :server="server" />
  </NSpace>
  <NEmpty v-else description="No Servers Found" />
  <NSpace vertical :size="12" v-if="radars && radars.length > 0" class="mt-8">
    <ServerRadarCard v-for="radar in radars" :key="radar.id" :radar="radar" />
  </NSpace>
</template>

<script setup lang="ts">
import { computed, watch } from 'vue';
import { useRoute } from 'vue-router';
import { useFetch } from '@vueuse/core';
import { NSpace, NEmpty } from 'naive-ui';
import * as md from 'mirror-data';
import type { RadarInfo } from '@server/server-radar/radar';
import ServerCard from '@/components/data/ServerCard.vue';
import ServerRadarCard from '@/components/data/ServerRadarCard.vue';

const route = useRoute();
const _endpointName = computed(() => route.query.endpoint as string);

const filteredServers = computed(() => {
  if (!_endpointName.value) {
    return md.ServerList;
  }
  return md.ServerList.filter(server => server.endpoint_name === _endpointName.value);
});

const endpoint = computed(() => md.EndpointsList.find(e => e.name === _endpointName.value));

const { data: radars, execute, abort } = useFetch(
  () => `/api/endpoints/${endpoint.value?.id}/radars`,
  { immediate: false, refetch: true, initialData: [] }
).json<RadarInfo[]>()

watch(endpoint, () => {
  if (endpoint.value) {
    abort()
    execute()
  } else {
    radars.value = []
  }
}, { immediate: true })
</script>
