<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { NButton, NInput } from 'naive-ui'
import AuthLayout from '../components/AuthLayout.vue'
import { api } from '../api/client'
import { ApiError, describeError, type InitializePayload } from '../api/types'
import { clearSession } from '../auth/session'

const router = useRouter()

const bootstrapToken = ref('')
const password = ref('123456')
const confirmation = ref('123456')
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
  <AuthLayout>
    <header class="gate-head">
      <p class="gate-eyebrow">FIRST RUN</p>
      <h1>初始化</h1>
      <p>用部署时生成的凭据创建管理员账号。</p>
    </header>

    <form class="gate-form" novalidate @submit.prevent="submit">
      <div class="gate-field">
        <label for="bootstrap-token">初始化凭据</label>
        <NInput
          v-model:value="bootstrapToken"
          type="password"
          size="large"
          show-password-on="click"
          placeholder="请输入部署时生成的凭据"
          autocomplete="off"
          :input-props="{ id: 'bootstrap-token', name: 'bootstrap-token' }"
        />
        <p class="gate-field-hint">凭据由部署过程生成，只在第一次初始化时有效。</p>
        <p v-if="fieldErrors.bootstrap_token" class="gate-field-error">{{ fieldErrors.bootstrap_token }}</p>
      </div>

      <div class="gate-field">
        <label for="initialize-password">管理员密码</label>
        <NInput
          v-model:value="password"
          type="password"
          size="large"
          show-password-on="click"
          placeholder="请输入管理员密码"
          autocomplete="new-password"
          :input-props="{ id: 'initialize-password', name: 'password' }"
        />
        <p class="gate-field-hint">密码长度为 6 到 128 个字符，默认密码为 123456。</p>
        <p v-if="fieldErrors.password" class="gate-field-error">{{ fieldErrors.password }}</p>
      </div>

      <div class="gate-field">
        <label for="initialize-confirmation">确认密码</label>
        <NInput
          v-model:value="confirmation"
          type="password"
          size="large"
          placeholder="请再次输入管理员密码"
          autocomplete="new-password"
          :input-props="{ id: 'initialize-confirmation', name: 'confirmation' }"
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
        {{ busy ? '初始化中…' : '创建管理员' }}
      </NButton>
    </form>

    <p class="gate-hint">
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <path d="M12 3 5 5.8v5.4c0 4.2 2.9 7.6 7 9.8 4.1-2.2 7-5.6 7-9.8V5.8L12 3Z" />
        <path d="m8.8 11.6 2.2 2.2 4.2-4.4" />
      </svg>
      管理员只能创建一次，请妥善保存密码
    </p>
  </AuthLayout>
</template>
