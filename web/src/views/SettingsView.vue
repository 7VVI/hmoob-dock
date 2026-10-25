<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import NodeTable from '../components/NodeTable.vue'
import { api } from '../api/client'
import {
  ApiError,
  describeError,
  formatTimestamp,
  type AgentTokenPayload,
  type NodeListPayload,
  type NodeSummary,
} from '../api/types'
import { clearSession } from '../auth/session'

// The time the enrollment token stays on the page. The token is shown once
// and disappears again, so it does not lie around on a screen that is left
// open.
const tokenDisplayTime = 60 * 1000

const router = useRouter()

// The enrollment token lives in the memory of the page only. It never
// reaches the storage of the browser.
const token = ref('')
const tokenExpiresAt = ref('')
const tokenError = ref('')
const tokenBusy = ref(false)
let tokenTimer: ReturnType<typeof setTimeout> | undefined

const nodes = ref<NodeSummary[]>([])
const nodesError = ref('')
const nodesBusy = ref(false)
const revoking = ref('')
const revokeError = ref('')
const revokeNotice = ref('')

const currentPassword = ref('')
const newPassword = ref('')
const confirmation = ref('')
const passwordError = ref('')
const passwordFields = ref<Record<string, string>>({})
const passwordBusy = ref(false)

// generateToken asks for a one time enrollment token.
//
// The token is only part of the answer of the API, so it can be shown once
// and never again.
async function generateToken() {
  if (tokenBusy.value) {
    return
  }
  tokenBusy.value = true
  tokenError.value = ''
  try {
    const payload = await api<AgentTokenPayload>('/api/v1/agent-tokens', { method: 'POST' })
    token.value = payload.token
    tokenExpiresAt.value = payload.expires_at
    if (tokenTimer) {
      clearTimeout(tokenTimer)
    }
    tokenTimer = setTimeout(hideToken, tokenDisplayTime)
  } catch (failure) {
    tokenError.value = describeError(failure)
  } finally {
    tokenBusy.value = false
  }
}

// hideToken removes the token from the page.
function hideToken() {
  token.value = ''
  tokenExpiresAt.value = ''
  if (tokenTimer) {
    clearTimeout(tokenTimer)
    tokenTimer = undefined
  }
}

// loadNodes reads the nodes of the deployment.
async function loadNodes() {
  nodesBusy.value = true
  nodesError.value = ''
  try {
    const payload = await api<NodeListPayload>('/api/v1/nodes')
    nodes.value = payload.nodes
  } catch (failure) {
    nodesError.value = describeError(failure)
  } finally {
    nodesBusy.value = false
  }
}

// askForRevoke opens the confirmation of a revocation.
function askForRevoke(node: NodeSummary) {
  revokeError.value = ''
  revokeNotice.value = ''
  revoking.value = node.id
}

// cancelRevoke closes the confirmation without doing anything.
function cancelRevoke() {
  revoking.value = ''
}

// revoke withdraws the credential of a node.
//
// The confirmation stays open when the request fails, so the administrator
// can try again without starting over.
async function revoke(node: NodeSummary) {
  revokeError.value = ''
  try {
    await api<void>(`/api/v1/nodes/${encodeURIComponent(node.id)}/revoke`, { method: 'POST' })
    revoking.value = ''
    revokeNotice.value = '已撤销该节点的接入凭据，它不会再连接。'
    await loadNodes()
  } catch (failure) {
    revokeError.value = describeError(failure)
  }
}

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

onMounted(loadNodes)
onBeforeUnmount(() => {
  if (tokenTimer) {
    clearTimeout(tokenTimer)
  }
})
</script>

<template>
  <AppShell>
    <h1 class="page__title">设置</h1>

    <section class="panel">
      <h2 class="panel__title">接入 Agent</h2>
      <p class="muted">令牌 15 分钟内有效，只能使用一次。</p>

      <button
        v-if="!token"
        type="button"
        class="button button--primary"
        :disabled="tokenBusy"
        @click="generateToken"
      >
        {{ tokenBusy ? '生成中…' : '生成接入令牌' }}
      </button>

      <div v-else class="token">
        <p class="token__hint">令牌只显示这一次，请立即保存。</p>
        <p class="token__value"><code>{{ token }}</code></p>
        <p class="muted">有效期至 {{ formatTimestamp(tokenExpiresAt) }}</p>
        <ol class="steps">
          <li>把令牌写入节点上的文件，并把权限设为 0600。</li>
          <li>用环境变量 HMOOB_ENROLLMENT_FILE 指向该文件。</li>
          <li>启动 Agent，令牌不会出现在命令历史里。</li>
        </ol>
        <p class="token__command">
          <code>HMOOB_ENROLLMENT_FILE=/etc/hmoob/enrollment.txt hmoob-agent</code>
        </p>
        <button type="button" class="button button--quiet" @click="hideToken">隐藏令牌</button>
      </div>

      <p v-if="tokenError" class="alert" role="alert">{{ tokenError }}</p>
    </section>

    <section class="panel">
      <div class="panel__head">
        <h2 class="panel__title">已接入节点</h2>
        <button type="button" class="button button--quiet" :disabled="nodesBusy" @click="loadNodes">
          刷新
        </button>
      </div>

      <div v-if="nodesError" class="alert" role="alert">
        <p>{{ nodesError }}</p>
        <button type="button" class="button button--quiet" @click="loadNodes">重试</button>
      </div>

      <NodeTable v-else :nodes="nodes">
        <template #actions="{ node }">
          <button
            v-if="revoking !== node.id"
            type="button"
            class="button button--quiet"
            @click="askForRevoke(node)"
          >
            撤销
          </button>
          <template v-else>
            <span class="muted">确认撤销？</span>
            <button type="button" class="button button--danger" @click="revoke(node)">确认</button>
            <button type="button" class="button button--quiet" @click="cancelRevoke">取消</button>
          </template>
        </template>
      </NodeTable>

      <p v-if="revokeNotice" class="notice">{{ revokeNotice }}</p>
      <p v-if="revokeError" class="alert" role="alert">{{ revokeError }}</p>
    </section>

    <section class="panel">
      <h2 class="panel__title">管理员密码</h2>
      <form class="form" novalidate @submit.prevent="changePassword">
        <div class="field">
          <label class="field__label" for="current-password">当前密码</label>
          <input
            id="current-password"
            v-model="currentPassword"
            class="field__input"
            type="password"
            name="current-password"
            autocomplete="current-password"
            required
          />
          <p v-if="passwordFields.current_password" class="field__error">
            {{ passwordFields.current_password }}
          </p>
        </div>
        <div class="field">
          <label class="field__label" for="new-password">新密码</label>
          <input
            id="new-password"
            v-model="newPassword"
            class="field__input"
            type="password"
            name="new-password"
            autocomplete="new-password"
            required
          />
          <p class="field__hint">密码长度为 12 到 128 个字符。</p>
          <p v-if="passwordFields.new_password" class="field__error">
            {{ passwordFields.new_password }}
          </p>
        </div>
        <div class="field">
          <label class="field__label" for="new-password-confirmation">确认新密码</label>
          <input
            id="new-password-confirmation"
            v-model="confirmation"
            class="field__input"
            type="password"
            name="new-password-confirmation"
            autocomplete="new-password"
            required
          />
        </div>
        <p v-if="passwordError" class="alert" role="alert">{{ passwordError }}</p>
        <button class="button button--primary" type="submit" :disabled="passwordBusy">
          {{ passwordBusy ? '提交中…' : '修改密码' }}
        </button>
      </form>
    </section>
  </AppShell>
</template>
