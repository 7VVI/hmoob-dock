<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { api } from '../api/client'
import BrandMark from './BrandMark.vue'
import { listAlerts } from '../api/alerts'
import { describeError } from '../api/types'
import { clearSession } from '../auth/session'

const route = useRoute()
const router = useRouter()

const busy = ref(false)
const error = ref('')

// The badge of the alerts: the events that are still open. The shell reads
// them at its own pace, so the number of the navigation follows the
// platform without a page of its own asking for it.
const openAlerts = ref(0)
let alertTimer: ReturnType<typeof setInterval> | undefined

async function loadAlertCount() {
  try {
    const events = await listAlerts(true)
    openAlerts.value = events.length
  } catch {
    // The badge is a convenience: a failed read hides it instead of
    // blocking the page of the administrator.
  }
}

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

// The icons of the navigation. One stroke set, everywhere the same.
const icons = {
  home: 'M4 10.4 12 4l8 6.4V20a1 1 0 0 1-1 1h-4.2v-6H9.2v6H5a1 1 0 0 1-1-1v-9.6Z',
  sites: 'M4 4h16v6H4V4Zm0 10h16v6H4v-6ZM7.5 7h.01M7.5 17h.01',
  nodes: 'M12 3 3 8v8l9 5 9-5V8L12 3ZM3 8l9 5 9-5M12 13v8',
  alerts: 'M18 8a6 6 0 0 0-12 0c0 6-3 8-3 8h18s-3-2-3-8M10 21h4',
  settings: 'M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4Zm7.4-3.2c0 .5-.05 1-.14 1.47l2.06 1.6-2 3.46-2.42-.98c-.75.62-1.6 1.12-2.54 1.45L14 22h-4l-.36-2.6c-.94-.33-1.79-.83-2.54-1.45l-2.42.98-2-3.46 2.06-1.6A8.6 8.6 0 0 1 4.6 12c0-.5.05-1 .14-1.47l-2.06-1.6 2-3.46 2.42.98c.75-.62 1.6-1.12 2.54-1.45L10 2h4l.36 2.6c.94.33 1.79.83 2.54 1.45l2.42-.98 2 3.46-2.06 1.6c.09.47.14.97.14 1.47Z',
  logout: 'M9 3H4v18h5m6-14 5 5-5 5M8 12h12',
}

// The menu follows the design of the platform: the overview first, then
// the sites, the nodes that run them, the alerts and the settings of the
// deployment. The services live on the page of their node.
const navigation = [
  { label: '概览', to: '/', icon: icons.home, section: '/' },
  { label: '站点管理', to: '/applications', icon: icons.sites, section: '/applications' },
  { label: '节点管理', to: '/nodes', icon: icons.nodes, section: '/nodes' },
  { label: '告警管理', to: '/alerts', icon: icons.alerts, section: '/alerts' },
  { label: '系统设置', to: '/settings', icon: icons.settings, section: '/settings' },
]

// The active entry follows the address: the detail pages of a section keep
// their section highlighted.
function isActive(section: string): boolean {
  if (section === '/') {
    return route.path === '/'
  }
  return route.path === section || route.path.startsWith(`${section}/`)
}

// The crumb names the section of the page the administrator reads.
const crumb = computed(() => {
  const path = route.path
  if (path === '/') return '概览'
  if (path.startsWith('/applications')) return '站点管理'
  if (path.startsWith('/nodes')) return '节点管理'
  if (path.startsWith('/alerts')) return '告警管理'
  if (path.startsWith('/settings')) return '系统设置'
  return '概览'
})

onMounted(() => {
  void loadAlertCount()
  alertTimer = setInterval(() => {
    void loadAlertCount()
  }, 30 * 1000)
})

onBeforeUnmount(() => {
  if (alertTimer) {
    clearInterval(alertTimer)
  }
})
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <RouterLink to="/" class="brand" aria-label="Docker 监控平台">
        <BrandMark class="brand-mark" />
        <span class="brand-copy">
          <strong>Docker</strong>
          <small>监控平台</small>
        </span>
      </RouterLink>

      <nav class="nav" aria-label="主导航">
        <RouterLink
          v-for="item in navigation"
          :key="item.to"
          :to="item.to"
          class="nav-link"
          :class="{ active: isActive(item.section) }"
          :aria-current="isActive(item.section) ? 'page' : undefined"
        >
          <svg class="nav-icon" viewBox="0 0 24 24" aria-hidden="true">
            <path :d="item.icon" />
          </svg>
          <span class="nav-label">{{ item.label }}</span>
          <span v-if="item.section === '/alerts' && openAlerts > 0" class="nav-badge">{{ openAlerts }}</span>
        </RouterLink>
      </nav>

      <div class="sidebar-art" aria-hidden="true">
        <svg viewBox="0 0 232 330" preserveAspectRatio="xMidYMax slice">
          <defs>
            <linearGradient id="wave-line" x1="0" y1="1" x2=".9" y2="0">
              <stop offset="0" stop-color="#2f7ef0" stop-opacity=".08" />
              <stop offset=".45" stop-color="#4d97ff" stop-opacity=".55" />
              <stop offset="1" stop-color="#8fd0ff" stop-opacity=".8" />
            </linearGradient>
            <radialGradient id="wave-glow" cx="50%" cy="78%" r="62%">
              <stop offset="0" stop-color="#3f8dff" stop-opacity=".52" />
              <stop offset="1" stop-color="#3f8dff" stop-opacity="0" />
            </radialGradient>
          </defs>
          <rect x="0" y="120" width="232" height="210" fill="url(#wave-glow)" />
          <g fill="none" stroke="url(#wave-line)" stroke-width="1.1" opacity=".62">
            <path d="M-30 300C40 210 58 250 96 214S158 130 210 150 250 120 262 132" />
            <path d="M-30 312C42 222 62 258 100 222S162 138 212 158 252 128 265 140" />
            <path d="M-30 324C44 234 66 266 104 230S166 146 214 166 254 136 268 148" />
            <path d="M-30 336C46 246 70 274 108 238S170 154 216 174 256 144 271 156" />
            <path d="M-30 288C38 198 54 242 92 206S154 122 208 142 248 112 259 124" />
            <path d="M-30 276C36 186 50 234 88 198S150 114 206 134 246 104 256 116" />
            <path d="M-30 264C34 174 46 226 84 190S146 106 204 126 244 96 253 108" />
            <path d="M-30 252C32 162 42 218 80 182S142 98 202 118 242 88 250 100" />
            <path d="M-30 240C30 150 38 210 76 174S138 90 200 110 240 80 247 92" />
          </g>
          <g fill="none" stroke="#6fb4ff" stroke-width=".5" opacity=".16">
            <path d="M20 330C46 250 80 210 118 190M60 330C82 262 112 224 146 204M100 330C118 274 144 240 174 222M140 330C154 286 176 254 202 238M180 330C190 296 208 268 230 254" />
          </g>
          <g fill="#9fd4ff" opacity=".5">
            <circle cx="96" cy="214" r="1.6" />
            <circle cx="210" cy="150" r="1.8" />
            <circle cx="206" cy="134" r="1.2" />
            <circle cx="80" cy="182" r="1.2" />
          </g>
        </svg>
      </div>
    </aside>

    <div class="workspace">
      <header class="topbar">
        <p class="crumb">
          <svg class="crumb-home" viewBox="0 0 24 24" aria-hidden="true">
            <path d="M4 10.4 12 4l8 6.4V20a1 1 0 0 1-1 1h-4.2v-6H9.2v6H5a1 1 0 0 1-1-1v-9.6Z" />
          </svg>
          <span class="crumb-root">首页</span>
          <span class="crumb-sep">›</span>
          <span class="crumb-current">{{ crumb }}</span>
        </p>

        <div class="account">
          <RouterLink class="account-bell" to="/alerts" aria-label="查看告警">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M18 8a6 6 0 0 0-12 0c0 6-3 8-3 8h18s-3-2-3-8M10 21h4" />
            </svg>
            <i v-if="openAlerts > 0" aria-hidden="true"></i>
          </RouterLink>
          <span class="account-avatar">管</span>
          <span class="account-name">管理员</span>
          <button type="button" class="account-out" :disabled="busy" aria-label="退出登录" title="退出登录" @click="signOut">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path :d="icons.logout" />
            </svg>
          </button>
        </div>
      </header>

      <main class="page">
        <p v-if="error" class="shell-error">{{ error }}</p>
        <slot />
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: 232px minmax(0, 1fr);
  min-height: 100dvh;
}

.sidebar {
  position: sticky;
  top: 0;
  z-index: 2;
  display: flex;
  flex-direction: column;
  height: 100dvh;
  overflow: hidden;
  background: #112a58;
  padding: 50px 20px 0;
}

.brand {
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 0 6px;
  color: #fff;
  text-decoration: none;
}

.brand:hover {
  text-decoration: none;
}

.brand-mark {
  width: 46px;
  height: 44px;
  flex: none;
  filter: drop-shadow(0 4px 10px rgb(20 140 255 / 45%));
}

.brand-copy {
  display: grid;
  gap: 1px;
  line-height: 1.16;
}

.brand-copy strong {
  font-size: 19px;
  font-weight: 700;
  letter-spacing: -0.02em;
}

.brand-copy small {
  font-size: 15px;
  color: #cfdcf2;
  letter-spacing: 0.02em;
}

.nav {
  display: grid;
  gap: 10px;
  margin-top: 50px;
}

.nav-link {
  display: flex;
  align-items: center;
  gap: 13px;
  height: 50px;
  padding: 0 16px;
  border-radius: 12px;
  color: #cfdcf2;
  font-size: 15.5px;
  font-weight: 500;
  text-decoration: none;
  transition: background 150ms ease, color 150ms ease;
}

.nav-link:hover {
  background: rgb(255 255 255 / 7%);
  color: #fff;
  text-decoration: none;
}

.nav-link.active {
  background: #0b5ce8;
  color: #fff;
  font-weight: 600;
  box-shadow: 0 10px 22px -12px rgb(11 92 232 / 80%);
}

.nav-icon {
  width: 20px;
  height: 20px;
  flex: none;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.nav-badge {
  margin-left: auto;
  min-width: 20px;
  height: 20px;
  padding: 0 6px;
  display: grid;
  place-items: center;
  border-radius: 999px;
  background: #f04438;
  color: #fff;
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
}

.sidebar-art {
  position: relative;
  flex: 1;
  min-height: 220px;
  margin: 24px -20px 0;
  pointer-events: none;
}

.sidebar-art svg {
  position: absolute;
  inset: auto 0 0;
  width: 100%;
  height: 100%;
}

.workspace {
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 22px 38px 0;
}

.crumb {
  display: flex;
  align-items: center;
  gap: 8px;
  color: #8595b2;
  font-size: 13px;
}

.crumb-home {
  width: 15px;
  height: 15px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.crumb-root {
  color: #7b8aa8;
}

.crumb-sep {
  color: #b3c0d6;
}

.crumb-current {
  color: #34446b;
  font-weight: 600;
}

.account {
  display: flex;
  align-items: center;
  gap: 9px;
  color: #34446b;
  font-size: 12.5px;
}

.account-bell {
  position: relative;
  width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  border-radius: 9px;
  color: #5b6d8e;
  transition: background 150ms ease, color 150ms ease;
}

.account-bell:hover {
  background: #e8f1ff;
  color: #0b63f6;
}

.account-bell svg {
  width: 17px;
  height: 17px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.account-bell i {
  position: absolute;
  top: 6px;
  right: 6px;
  width: 7px;
  height: 7px;
  border: 1.5px solid #f4f8ff;
  border-radius: 50%;
  background: #f04438;
}

.account-avatar {
  width: 26px;
  height: 26px;
  display: grid;
  place-items: center;
  border-radius: 50%;
  background: #0b63f6;
  color: #fff;
  font-size: 11px;
  font-weight: 600;
}

.account-name {
  color: #5b6d8e;
}

.account-out {
  width: 28px;
  height: 28px;
  margin-left: 2px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #8595b2;
  cursor: pointer;
  transition: background 150ms ease, color 150ms ease;
}

.account-out:hover:not(:disabled) {
  background: #e8f1ff;
  color: #0b63f6;
}

.account-out:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.account-out svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.page {
  width: 100%;
  padding: 16px 38px 52px;
}

.shell-error {
  margin-bottom: 16px;
  padding: 11px 15px;
  border-radius: 10px;
  background: #fef0ef;
  color: #d92d20;
  font-size: 12px;
}

@media (max-width: 1180px) {
  .shell {
    grid-template-columns: 210px minmax(0, 1fr);
  }

  .sidebar {
    padding: 26px 16px 0;
  }

  .sidebar-art {
    margin-inline: -16px;
  }

  .topbar {
    padding: 20px 24px 0;
  }

  .page {
    padding: 14px 24px 44px;
  }
}

@media (max-width: 860px) {
  .shell {
    display: block;
  }

  .sidebar {
    position: sticky;
    height: auto;
    flex-direction: row;
    align-items: center;
    gap: 14px;
    padding: 10px 14px;
    border-bottom: 1px solid rgb(255 255 255 / 10%);
  }

  .brand-mark {
    width: 34px;
    height: 32px;
  }

  .brand-copy strong {
    font-size: 15px;
  }

  .brand-copy small {
    display: none;
  }

  .nav {
    display: flex;
    flex: 1;
    justify-content: flex-end;
    gap: 4px;
    margin-top: 0;
    overflow-x: auto;
  }

  .nav-link {
    height: 38px;
    padding: 0 11px;
    font-size: 13px;
    white-space: nowrap;
  }

  .nav-label {
    display: none;
  }

  .nav-link.active {
    box-shadow: none;
  }

  .nav-badge {
    position: absolute;
    transform: translate(12px, -12px);
    min-width: 16px;
    height: 16px;
    font-size: 9px;
  }

  .nav-link {
    position: relative;
  }

  .sidebar-art {
    display: none;
  }

  .topbar {
    padding: 14px 16px 0;
  }

  .page {
    padding: 12px 16px 38px;
  }

  .account-name {
    display: none;
  }
}
</style>
