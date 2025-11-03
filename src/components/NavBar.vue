<template>
  <nav class="flex items-center bg-white p-4 px-8 shadow-md dark:bg-slate-900">
    <div class="flex items-center">
      <a class="flex items-center" href="/">
        <Icon :size="24" class="mr-3 h-6 w-8">
          <Database />
        </Icon>
        <span class="text-xl font-semibold dark:text-white">MirrorDB</span>
      </a>
    </div>
    <div class="flex items-center gap-5 ml-6">
      <RouterLink v-for="menu in menus" :key="menu.key" :to="menu.to" v-slot="{ navigate }" custom>
        <NButton @click="navigate" text>
          {{ menu.label }}
        </NButton>
      </RouterLink>
    </div>
    <div class="flex flex-1"></div>
  </nav>
</template>

<script setup lang="ts">
import { Database } from '@vicons/tabler'
import { Icon } from '@vicons/utils'
import type { Component } from 'vue'
import { h } from 'vue'
import { RouterLink } from 'vue-router'

import { NButton } from 'naive-ui'

const menuIcon = (icon: Component) => () => h(Icon, null, { default: () => h(icon) })

const menus = [
  {
    label: 'Endpoints',
    key: "endpoints",
    to: "/endpoints",
  },
  {
    label: 'Servers',
    key: "servers",
    to: "/servers",
  },
]
</script>
