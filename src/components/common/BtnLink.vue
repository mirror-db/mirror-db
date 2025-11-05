<template>
  <NButton v-bind="$attrs" tag="a" :href="href" target="_blank">
    <template #icon>
      <NIcon v-if="icon">
        <slot name="icon">
          <IconLink />
        </slot>
      </NIcon>
    </template>
    <slot v-if="label">
      {{ label }}
    </slot>
  </NButton>
</template>

<script setup lang="ts">
import { NButton, NIcon } from 'naive-ui'
import { ExternalLink as IconLink } from '@vicons/tabler'

withDefaults(defineProps<{
  label?: string
  href: string
  icon?: boolean
}>(), {
  icon: false,
})
</script>
