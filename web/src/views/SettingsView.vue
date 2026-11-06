<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { NButton, NInput } from 'naive-ui'
import AppShell from '../components/AppShell.vue'
import { api } from '../api/client'
import { ApiError, describeError } from '../api/types'
import { clearSession } from '../auth/session'

// The settings page holds the one thing an administrator changes on the
// deployment itself: the password of the administrator. The enrollment of
// the nodes lives on the nodes page, next to the list of the nodes.
const router = useRouter()

const currentPassword = ref('')
const newPassword = ref('')
const confirmation = ref('')
const passwordError = ref('')
const passwordFields = ref<Record<string, string>>({})
const passwordBusy = ref(false)

// changePassword replaces the password of the administrator.
//
// The password change revokes every session, so the page holds no session
// afterwards and the administrator has to log in again.
async function changePassword() {
  if (passwordBusy.value) {
    return
  }
  passwordError.value = ''
  passwordFields.value = {}
  if (newPassword.value !== confirmation.value) {
    passwordError.value = '两次输入的密码不一致。'
    return
  }
  passwordBusy.value = true
  try {
    await api<void>('/api/v1/auth/password', {
      method: 'POST',
      body: JSON.stringify({
        current_password: currentPassword.value,
        new_password: newPassword.value,
      }),
    })
    clearSession(false)
    currentPassword.value = ''
    newPassword.value = ''
    confirmation.value = ''
    await router.replace({ path: '/login', query: { notice: 'password-changed' } })
  } catch (failure) {
    passwordError.value = describeError(failure)
    if (failure instanceof ApiError) {
      passwordFields.value = failure.fields
    }
  } finally {
    passwordBusy.value = false
  }
}
</script>

<template>
  <AppShell>
    <h1 class="page-title">系统设置</h1>
    <p class="page-subtitle">管理管理员的登录凭据。</p>

    <section class="settings-panel">
      <header class="settings-head">
        <span class="settings-icon" aria-hidden="true">
          <svg viewBox="0 0 24 24">
            <rect x="5" y="10" width="14" height="10" rx="2.4" />
            <path d="M8 10V7.6a4 4 0 0 1 8 0V10" />
          </svg>
        </span>
        <div class="settings-copy">
          <h2>管理员密码</h2>
          <p>修改后所有会话失效，需要使用新密码重新登录。</p>
        </div>
        <span class="account-chip">单管理员账户</span>
      </header>

      <form class="password-form" novalidate @submit.prevent="changePassword">
        <div class="field">
          <label for="current-password">当前密码</label>
          <NInput
            v-model:value="currentPassword"
            type="password"
            size="large"
            placeholder="请输入当前密码"
            autocomplete="current-password"
            :input-props="{ id: 'current-password', name: 'current-password' }"
          />
          <p v-if="passwordFields.current_password" class="field-error">
            {{ passwordFields.current_password }}
          </p>
        </div>

        <div class="field">
          <label for="new-password">新密码</label>
          <NInput
            v-model:value="newPassword"
            type="password"
            size="large"
            show-password-on="click"
            placeholder="请输入新密码"
            autocomplete="new-password"
            :input-props="{ id: 'new-password', name: 'new-password' }"
          />
          <small class="field-hint">密码长度为 6 到 128 个字符。</small>
          <p v-if="passwordFields.new_password" class="field-error">
            {{ passwordFields.new_password }}
          </p>
        </div>

        <div class="field">
          <label for="confirm-password">确认新密码</label>
          <NInput
            v-model:value="confirmation"
            type="password"
            size="large"
            placeholder="请再次输入新密码"
            autocomplete="new-password"
            :input-props="{ id: 'confirm-password', name: 'confirmation' }"
          />
        </div>

        <p v-if="passwordError" role="alert" class="form-error">{{ passwordError }}</p>

        <div class="form-actions">
          <NButton type="primary" size="large" attr-type="submit" :loading="passwordBusy">
            修改密码
          </NButton>
        </div>
      </form>
    </section>
  </AppShell>
</template>

<style scoped>
.settings-panel {
  max-width: 640px;
  overflow: hidden;
  border: 1px solid #e9eff9;
  border-radius: 18px;
  background: #fff;
  box-shadow: 0 18px 38px -36px rgb(20 52 110 / 50%);
}

.settings-head {
  display: flex;
  align-items: flex-start;
  gap: 13px;
  padding: 22px 24px;
  border-bottom: 1px solid #eef3fb;
}

.settings-icon {
  width: 40px;
  height: 40px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e8f1ff;
  color: #2f6fe4;
}

.settings-icon svg {
  width: 19px;
  height: 19px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.settings-copy {
  min-width: 0;
  flex: 1;
}

.settings-copy h2 {
  color: #16264a;
  font-size: 16px;
  font-weight: 700;
}

.settings-copy p {
  margin-top: 2px;
  color: #6b7d9c;
  font-size: 12.5px;
  line-height: 1.7;
}

.account-chip {
  margin-left: auto;
  padding: 2px 9px;
  flex: none;
  border-radius: 999px;
  background: #f2f6fd;
  color: #7b8aa8;
  font-size: 10.5px;
  font-weight: 600;
  white-space: nowrap;
}

.password-form {
  display: grid;
  gap: 18px;
  padding: 24px;
}

.field {
  display: grid;
  gap: 8px;
}

.field label {
  color: #34446b;
  font-size: 13px;
  font-weight: 600;
}

.field-hint {
  color: #94a3bd;
  font-size: 11.5px;
  line-height: 1.7;
}

.field-error {
  color: #d92d20;
  font-size: 11.5px;
  line-height: 1.7;
}

.form-error {
  padding: 10px 13px;
  border-radius: 10px;
  background: #fef0ef;
  color: #d92d20;
  font-size: 12.5px;
  line-height: 1.7;
}

.form-actions {
  display: flex;
  justify-content: flex-end;
  padding-top: 2px;
}

@media (max-width: 640px) {
  .settings-head {
    flex-wrap: wrap;
    padding: 18px;
  }

  .account-chip {
    display: none;
  }

  .password-form {
    padding: 18px;
  }

  .form-actions {
    justify-content: stretch;
  }

  .form-actions :deep(.n-button) {
    width: 100%;
  }
}
</style>
