<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NAlert, NButton, NInput } from 'naive-ui'
import AuthLayout from '../components/AuthLayout.vue'
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
  <AuthLayout>
    <header class="gate-head">
      <p class="gate-eyebrow">ADMIN CONSOLE</p>
      <h1>登录</h1>
      <p>使用管理员密码进入 Docker 集群监控平台。</p>
    </header>

    <NAlert v-if="notice" class="mb-4" type="info" :show-icon="false">{{ notice }}</NAlert>

    <form class="gate-form" novalidate @submit.prevent="submit">
      <div class="gate-field">
        <label for="admin-password">管理员密码</label>
        <NInput
          v-model:value="password"
          type="password"
          size="large"
          show-password-on="click"
          placeholder="请输入管理员密码"
          autocomplete="current-password"
          :input-props="{ id: 'admin-password', name: 'password' }"
          @keyup.enter="submit"
        />
      </div>

      <p v-if="error" role="alert" class="gate-error">{{ error }}</p>

      <NButton
        class="gate-submit"
        type="primary"
        size="large"
        attr-type="submit"
        :disabled="busy"
        :loading="busy"
        @click="submit"
      >
        {{ busy ? '登录中…' : '登录' }}
      </NButton>
    </form>

    <p class="gate-hint">
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <rect x="5" y="10" width="14" height="10" rx="2.4" />
        <path d="M8 10V7.6a4 4 0 0 1 8 0V10" />
      </svg>
      单管理员账户 · 密码由部署时设置
    </p>
  </AuthLayout>
</template>

