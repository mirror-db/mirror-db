<template>
  <h1 class="text-2xl font-bold mb-8">Servers for {{ endpoint || 'All Endpoints' }}</h1>
  <NSpace vertical :size="12" v-if="filteredServers.length > 0">
    <ServerCard v-for="server in filteredServers" :key="server.id" :server="server" />
  </NSpace>
  <NResult v-else status="404" title="No Servers Found" description="No servers were found for this endpoint.">
  </NResult>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useRoute } from 'vue-router';
import { ServerList } from '../../data/servers';
import ServerCard from '../components/ServerCard.vue';
import { NSpace, NResult } from 'naive-ui';

const route = useRoute();
const endpoint = computed(() => route.query.endpoint as string);

const filteredServers = computed(() => {
  if (!endpoint.value) {
    return ServerList;
  }
  return ServerList.filter(server => server.endpoint_name === endpoint.value);
});
</script>

<style scoped>
.server-list {
  padding: 16px;
}
</style>
