<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api/client'
import { ApiError, describeError, type InitializePayload } from '../api/types'
import { clearSession } from '../auth/session'

const router = useRouter()

const bootstrapToken = ref('')
const password = ref('')
const confirmation = ref('')
const error = ref('')
const fieldErrors = ref<Record<string, string>>({})
const busy = ref(false)

// submit creates the administrator of the deployment.
//
// The inputs stay in the form when the initialization fails, so a wrong
// credential does not have to be typed again.
async function submit() {
  if (busy.value) {
    return
  }
  error.value = ''
  fieldErrors.value = {}
  if (password.value !== confirmation.value) {
    error.value = '两次输入的密码不一致。'
    return
  }
  busy.value = true
  try {
    await api<InitializePayload>('/api/v1/auth/initialize', {
      method: 'POST',
      body: JSON.stringify({
        bootstrap_token: bootstrapToken.value,
        password: password.value,
      }),
    })
    // The administrator exists now, the page holds no session yet.
    clearSession(false)
    await router.push('/login')
  } catch (failure) {
    error.value = describeError(failure)
    if (failure instanceof ApiError) {
      fieldErrors.value = failure.fields
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="page page--narrow">
    <p class="shell__brand">轻量部署平台</p>
    <h1 class="page__title">初始化</h1>
    <p class="page__lead">用部署时生成的凭据创建管理员账号。</p>
    <form class="form" novalidate @submit.prevent="submit">
      <div class="field">
        <label class="field__label" for="bootstrap-token">初始化凭据</label>
        <input
          id="bootstrap-token"
          v-model="bootstrapToken"
          class="field__input"
          type="password"
          name="bootstrap-token"
          autocomplete="off"
          required
        />
        <p class="field__hint">凭据由部署过程生成，只在第一次初始化时有效。</p>
        <p v-if="fieldErrors.bootstrap_token" class="field__error">
          {{ fieldErrors.bootstrap_token }}
        </p>
      </div>
      <div class="field">
        <label class="field__label" for="password">管理员密码</label>
        <input
          id="password"
          v-model="password"
          class="field__input"
          type="password"
          name="password"
          autocomplete="new-password"
          required
        />
        <p class="field__hint">密码长度为 12 到 128 个字符。</p>
        <p v-if="fieldErrors.password" class="field__error">{{ fieldErrors.password }}</p>
      </div>
      <div class="field">
        <label class="field__label" for="confirmation">确认密码</label>
        <input
          id="confirmation"
          v-model="confirmation"
          class="field__input"
          type="password"
          name="confirmation"
          autocomplete="new-password"
          required
        />
      </div>
      <p v-if="error" class="alert" role="alert">{{ error }}</p>
      <button class="button button--primary" type="submit" :disabled="busy">
        {{ busy ? '初始化中…' : '创建管理员' }}
      </button>
    </form>
  </main>
</template>
