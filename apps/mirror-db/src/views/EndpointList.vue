<template>
  <h1 class="text-2xl font-bold mb-8">Endpoints</h1>
  <NSpace vertical :size="12" v-if="EndpointsList.length > 0">
    <EndpointCard v-for="endpoint in EndpointsList" :key="endpoint.id" :endpoint="endpoint"></EndpointCard>
  </NSpace>
  <NEmpty v-else description="No Endpoints Found" />
</template>

<script setup lang="ts">
import { NSpace, NEmpty } from 'naive-ui'
import { EndpointsList } from 'mirror-data'
import EndpointCard from '@/components/data/EndpointCard.vue'
</script>
