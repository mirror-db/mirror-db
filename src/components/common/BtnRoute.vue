<template>
  <RouterLink :to="to">
    <NButton v-bind="$attrs">
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
  </RouterLink>
</template>

<script setup lang="ts">
import { RouterLink, type RouteLocationRaw } from 'vue-router'
import { NButton, NIcon } from 'naive-ui'
import { Link as IconLink } from '@vicons/tabler'

withDefaults(defineProps<{
  label?: string
  to: RouteLocationRaw
  icon?: boolean
}>(), {
  icon: false,
})
</script>
