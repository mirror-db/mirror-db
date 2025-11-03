<template>
  <h1 class="text-2xl font-bold mb-8">Endpoints</h1>
  <NSpace vertical :size="12">
    <EndpointCard v-for="endpoint in Endpoints" :key="endpoint.id" :endpoint="endpoint"></EndpointCard>
  </NSpace>
</template>

<script setup lang="ts">
import { Endpoints } from '../../data/endpoints'
import EndpointCard from '../components/EndpointCard.vue'
import { NSpace } from 'naive-ui'
</script>
