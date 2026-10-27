<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api/client'
import { describeError, type LoginPayload } from '../api/types'
import { beginSession } from '../auth/session'

const router = useRouter()
const route = useRoute()

const password = ref('')
const error = ref('')
const busy = ref(false)

// The notice of the page tells the administrator what happened before.
const notice = computed(() =>
  route.query.notice === 'password-changed' ? '密码已修改，请使用新密码登录。' : '',
)

// submit logs the administrator in.
//
// The password stays in the form when the login fails, so a typo does not
// have to be typed again.
async function submit() {
  if (busy.value) {
    return
  }
  busy.value = true
  error.value = ''
  try {
    const payload = await api<LoginPayload>('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify({ password: password.value }),
    })
    beginSession(payload)
    await router.push('/')
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="page page--narrow">
    <p class="shell__brand">轻量部署平台</p>
    <h1 class="page__title">登录</h1>
    <p v-if="notice" class="notice">{{ notice }}</p>
    <form class="form" novalidate @submit.prevent="submit">
      <div class="field">
        <label class="field__label" for="password">管理员密码</label>
        <input
          id="password"
          v-model="password"
          class="field__input"
          type="password"
          name="password"
          autocomplete="current-password"
          required
        />
      </div>
      <p v-if="error" class="alert" role="alert">{{ error }}</p>
      <button class="button button--primary" type="submit" :disabled="busy">
        {{ busy ? '登录中…' : '登录' }}
      </button>
    </form>
  </main>
</template>
