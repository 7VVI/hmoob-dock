<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { api } from '../api/client'
import { describeError, formatTimestamp, type AgentTokenPayload } from '../api/types'

// The panel that enrolls a node. The token lives in the memory of the page
// only and disappears after a minute or when the panel closes, so it does
// not stay on a screen that is left open.
const tokenDisplayTime = 60 * 1000
const enrollCommand = 'HMOOB_ENROLLMENT_FILE=/etc/hmoob/enrollment.txt hmoob-agent'

const emit = defineEmits<{ close: [] }>()

const token = ref('')
const expiresAt = ref('')
const busy = ref(false)
const error = ref('')
const copied = ref('')
let tokenTimer: ReturnType<typeof setTimeout> | undefined
let copyTimer: ReturnType<typeof setTimeout> | undefined

// generateToken asks for a one time enrollment token.
async function generateToken() {
  if (busy.value) {
    return
  }
  busy.value = true
  error.value = ''
  try {
    const payload = await api<AgentTokenPayload>('/api/v1/agent-tokens', { method: 'POST' })
    token.value = payload.token
    expiresAt.value = payload.expires_at
    if (tokenTimer) {
      clearTimeout(tokenTimer)
    }
    tokenTimer = setTimeout(hideToken, tokenDisplayTime)
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    busy.value = false
  }
}

// hideToken removes the token from the page.
function hideToken() {
  token.value = ''
  expiresAt.value = ''
  if (tokenTimer) {
    clearTimeout(tokenTimer)
    tokenTimer = undefined
  }
}

// copy writes one of the values to the clipboard and marks the button.
async function copy(value: string, key: string) {
  try {
    await navigator.clipboard.writeText(value)
    copied.value = key
    if (copyTimer) {
      clearTimeout(copyTimer)
    }
    copyTimer = setTimeout(() => {
      copied.value = ''
    }, 1800)
  } catch {
    error.value = '浏览器未允许复制，请手动选择文本。'
  }
}

onBeforeUnmount(() => {
  if (tokenTimer) {
    clearTimeout(tokenTimer)
  }
  if (copyTimer) {
    clearTimeout(copyTimer)
  }
})
</script>

<template>
  <div class="enroll-body">
    <p class="enroll-intro">生成一次性令牌，在目标主机上启动 Agent。令牌 15 分钟内有效。</p>
    <div class="enroll-body-grid">
      <div class="enroll-main">
        <template v-if="!token">
          <ol class="steps">
            <li>
              <span>1</span>
              <div>
                <strong>生成接入令牌</strong>
                <small>令牌只显示一次，生成后立即复制。</small>
              </div>
            </li>
            <li>
              <span>2</span>
              <div>
                <strong>在节点上写入令牌并启动 Agent</strong>
                <small>把令牌写入 0600 权限的文件，用环境变量指向它。</small>
              </div>
            </li>
            <li>
              <span>3</span>
              <div>
                <strong>回到本页查看节点</strong>
                <small>节点连接后会自动出现在列表里。</small>
              </div>
            </li>
          </ol>

          <button type="button" class="enroll-primary" :disabled="busy" @click="generateToken">
            {{ busy ? '生成中…' : '生成接入令牌' }}
          </button>
        </template>

        <template v-else>
          <div class="token-box">
            <div class="token-head">
              <span>接入令牌只显示这一次</span>
              <small>有效期至 {{ formatTimestamp(expiresAt) }}</small>
            </div>
            <div class="token-line">
              <code>{{ token }}</code>
              <button type="button" @click="copy(token, 'token')">
                {{ copied === 'token' ? '已复制' : '复制令牌' }}
              </button>
            </div>
          </div>

          <div class="command-box">
            <div class="command-head">
              <span>在目标节点上执行</span>
              <button type="button" @click="copy(enrollCommand, 'command')">
                {{ copied === 'command' ? '已复制' : '复制命令' }}
              </button>
            </div>
            <code>{{ enrollCommand }}</code>
          </div>

          <div class="enroll-actions">
            <span>令牌只使用一次，过期后重新生成即可。</span>
            <button type="button" class="quiet" @click="hideToken">隐藏令牌</button>
          </div>
        </template>

        <p v-if="error" class="enroll-error">{{ error }}</p>
      </div>

        <aside class="enroll-aside">
          <div class="aside-item">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 5 5.8v5.4c0 4.2 2.9 7.6 7 9.8 4.1-2.2 7-5.6 7-9.8V5.8L12 3Z" />
              <path d="m8.8 11.6 2.2 2.2 4.2-4.4" />
            </svg>
            <div>
              <strong>无需开放入站端口</strong>
              <small>Agent 主动向控制端发起连接，节点侧不用放通端口。</small>
            </div>
          </div>
          <div class="aside-item">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <circle cx="12" cy="12" r="8.5" />
              <path d="M12 7.5V12l3 1.8" />
            </svg>
            <div>
              <strong>令牌只显示一次</strong>
              <small>15 分钟内有效，页面关闭后需要重新生成。</small>
            </div>
          </div>
          <div class="aside-item">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M4 12a8 8 0 0 0 14 6" />
            </svg>
            <div>
              <strong>自动出现在列表</strong>
              <small>连接成功后本页每 15 秒同步一次，无需手动刷新。</small>
            </div>
          </div>
        </aside>
      </div>

      <p v-if="error" class="enroll-error">{{ error }}</p>
  </div>
</template>

<style scoped>
.enroll-intro {
  margin: 0 0 16px;
  color: #6b7d9c;
  font-size: 12.5px;
  line-height: 1.7;
}

.enroll-body-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.55fr) minmax(240px, 0.9fr);
  gap: 26px;
}

.enroll-main {
  min-width: 0;
}

.enroll-aside {
  display: grid;
  align-content: start;
  gap: 14px;
  padding: 16px 18px;
  border: 1px solid #eef3fb;
  border-radius: 14px;
  background: #f8fbff;
}

.aside-item {
  display: flex;
  gap: 10px;
}

.aside-item svg {
  width: 16px;
  height: 16px;
  flex: none;
  margin-top: 1px;
  fill: none;
  stroke: #2f6fe4;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.aside-item strong {
  display: block;
  color: #34446b;
  font-size: 12px;
  font-weight: 600;
}

.aside-item small {
  display: block;
  margin-top: 1px;
  color: #8595b2;
  font-size: 11px;
  line-height: 1.65;
}

.steps {
  display: grid;
  gap: 12px;
  margin: 0 0 20px;
  padding: 0;
  list-style: none;
}

.steps li {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}

.steps li > span {
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e8f1ff;
  color: #2f6fe4;
  font-size: 11px;
  font-weight: 700;
}

.steps strong {
  display: block;
  color: #34446b;
  font-size: 12.5px;
  font-weight: 600;
}

.steps small {
  display: block;
  margin-top: 1px;
  color: #8595b2;
  font-size: 11.5px;
  line-height: 1.65;
}

.enroll-primary {
  height: 36px;
  padding: 0 16px;
  border: 0;
  border-radius: 9px;
  background: #0b63f6;
  color: #fff;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}

.enroll-primary:hover:not(:disabled) {
  background: #0755d8;
}

.enroll-primary:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.token-box {
  margin-top: 0;
  padding: 14px 16px;
  border: 1px solid #f4e2c4;
  border-radius: 12px;
  background: #fffaeb;
}

.token-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.token-head span {
  color: #84510a;
  font-size: 12px;
  font-weight: 600;
}

.token-head small {
  color: #9a6a2a;
  font-size: 11px;
}

.token-line {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 8px;
}

.token-line code {
  min-width: 0;
  flex: 1;
  color: #6b4409;
  font-size: 13px;
  overflow-wrap: anywhere;
}

.token-line button,
.command-head button {
  height: 28px;
  padding: 0 10px;
  flex: none;
  border: 1px solid #f0d9ae;
  border-radius: 7px;
  background: #fff;
  color: #84510a;
  font-size: 11.5px;
  font-weight: 600;
  cursor: pointer;
}

.command-box {
  margin-top: 14px;
  border: 1px solid #e9eff9;
  border-radius: 12px;
  overflow: hidden;
}

.command-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  border-bottom: 1px solid #eef3fb;
  background: #f8faff;
}

.command-head span {
  color: #6b7d9c;
  font-size: 11.5px;
  font-weight: 600;
}

.command-head button {
  border-color: #dbe6f7;
  color: #2f6fe4;
}

.command-box > code {
  display: block;
  padding: 12px 14px;
  color: #34446b;
  font-size: 12px;
  overflow-wrap: anywhere;
}

.enroll-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 14px;
}

.enroll-actions span {
  color: #8595b2;
  font-size: 11.5px;
}

.quiet {
  border: 0;
  background: none;
  color: #2f6fe4;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}

.enroll-error {
  margin-top: 12px;
  padding: 9px 12px;
  border-radius: 9px;
  background: #fef0ef;
  color: #d92d20;
  font-size: 12px;
}

@media (max-width: 980px) {
  .enroll-body-grid {
    grid-template-columns: minmax(0, 1fr);
    gap: 18px;
  }

  .enroll-aside {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
}

@media (max-width: 640px) {
  .enroll-aside {
    grid-template-columns: minmax(0, 1fr);
  }

  .token-line {
    flex-direction: column;
    align-items: stretch;
  }

  .token-line button {
    align-self: flex-start;
  }
}
</style>
