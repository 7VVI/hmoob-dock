<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api/client'
import { describeError } from '../api/types'
import { clearSession } from '../auth/session'

const router = useRouter()

const busy = ref(false)
const error = ref('')

// signOut ends the session of the administrator.
//
// The local session ends in any case: a request that fails does not keep the
// administrator on a page that needs a session. The reason of the failure is
// shown, so it does not pass unnoticed.
async function signOut() {
  if (busy.value) {
    return
  }
  busy.value = true
  error.value = ''
  try {
    await api<void>('/api/v1/auth/logout', { method: 'POST' })
    clearSession(false)
    await router.replace('/login')
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="shell">
    <header class="shell__header">
      <p class="shell__brand">轻量部署平台</p>
      <nav class="shell__nav" aria-label="主导航">
        <RouterLink class="shell__link" to="/">概览</RouterLink>
        <RouterLink class="shell__link" to="/applications">应用</RouterLink>
        <RouterLink class="shell__link" to="/alerts">告警</RouterLink>
        <RouterLink class="shell__link" to="/settings">设置</RouterLink>
      </nav>
      <button type="button" class="button button--quiet" :disabled="busy" @click="signOut">
        退出登录
      </button>
    </header>
    <p v-if="error" class="alert" role="alert">{{ error }}</p>
    <main class="shell__main">
      <slot />
    </main>
  </div>
</template>
