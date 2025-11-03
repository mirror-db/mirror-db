<template>
  <NButton v-bind="$attrs" @click="copy()">
    <template #icon>
      <NIcon v-if="icon">
        <IconCopy />
      </NIcon>
    </template>
    <slot name="copied" v-if="copied">
      Copied!
    </slot>
    <slot v-else :copied="copied">
      <span v-if="label">
        {{ label }}
      </span>
      <span v-if="!icon && !label">
        Copy
      </span>
    </slot>
  </NButton>
</template>

<script setup lang="ts">
import { toRef } from 'vue'
import { NButton, NIcon } from 'naive-ui'
import { useClipboard } from '@vueuse/core'
import { Copy as IconCopy } from '@vicons/tabler'

const props = withDefaults(defineProps<{
  label?: string
  content: string
  icon?: boolean
}>(), {
  icon: false,
})

const { copy, copied } = useClipboard({ source: toRef(props, 'content'), copiedDuring: 700 })
</script>
