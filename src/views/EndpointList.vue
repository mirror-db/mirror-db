<template>
  <h1 class="text-2xl font-bold mb-8">Endpoints</h1>
  <NSpace vertical :size="12" v-if="EndpointsList.length > 0">
    <EndpointCard v-for="endpoint in EndpointsList" :key="endpoint.id" :endpoint="endpoint"></EndpointCard>
  </NSpace>
  <NResult v-else status="404" title="No Endpoints Found" description="There are no endpoints available at the moment.">
  </NResult>
</template>

<script setup lang="ts">
import { EndpointsList } from '../../data/endpoints'
import EndpointCard from '../components/EndpointCard.vue'
import { NSpace, NResult } from 'naive-ui'
</script>
