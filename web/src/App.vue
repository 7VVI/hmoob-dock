<script setup lang="ts">
import { onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { onSessionEnded } from './auth/session'

const router = useRouter()

// A session that ends on its own sends the administrator back to the login.
const stopListening = onSessionEnded(() => {
  if (router.currentRoute.value.path !== '/login') {
    void router.replace('/login')
  }
})

onBeforeUnmount(stopListening)
</script>

<template>
  <RouterView />
</template>
