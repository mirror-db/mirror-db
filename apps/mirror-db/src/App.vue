<script setup lang="ts">
import { RouterView } from 'vue-router'
import NavBar from './components/NavBar.vue'
import { NConfigProvider, NGlobalStyle, darkTheme } from "naive-ui";

</script>

<template>
  <NConfigProvider :theme="darkTheme">
    <NGlobalStyle />
    <NavBar />
    <main class="container mx-auto p-8">
      <RouterView />
    </main>
  </NConfigProvider>
</template>
