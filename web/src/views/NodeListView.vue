<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import AppShell from '../components/AppShell.vue'
import BrandMark from '../components/BrandMark.vue'
import NodeEnrollment from '../components/NodeEnrollment.vue'
import { listNodes, roleLabel } from '../api/nodes'
import { listApplications } from '../api/deployments'
import { listContainers, type ContainerSummary } from '../api/metrics'
import { api } from '../api/client'
import { describeError, formatTimestamp, type AppSummary, type NodeSummary } from '../api/types'

// The nodes page follows the environment list of Portainer: one row for
// every node, with the facts of the host, the containers it runs and the
// actions an administrator takes on it. The enrollment of a new node lives
// here too, so the page where a node appears is also the page that adds it.

const nodes = ref<NodeSummary[]>([])
const apps = ref<AppSummary[]>([])
const containersByNode = ref<Record<string, ContainerSummary[]>>({})
const loading = ref(true)
const initialLoaded = ref(false)
const refreshing = ref(false)
const error = ref('')
const enrolling = ref(false)
const revoking = ref('')
const revokeBusy = ref(false)
const notice = ref('')
const actionError = ref('')

// load reads the nodes, the sites and the inventory of every node. A
// container list that fails for one node does not sink the page; the counts
// of that node fall back to the number of the last inventory.
async function load(silent = false) {
  if (!initialLoaded.value) {
    loading.value = true
  } else if (!silent) {
    refreshing.value = true
  }
  error.value = ''
  try {
    const [nodeList, appList] = await Promise.all([
      listNodes(),
      listApplications().catch(() => [] as AppSummary[]),
    ])
    nodes.value = nodeList
    apps.value = appList

    const lists = await Promise.all(
      nodeList.map(async (node) => {
        try {
          return [node.id, await listContainers(node.id)] as const
        } catch {
          return [node.id, [] as ContainerSummary[]] as const
        }
      }),
    )
    const map: Record<string, ContainerSummary[]> = {}
    for (const [id, list] of lists) {
      map[id] = list
    }
    containersByNode.value = map
  } catch (failure) {
    if (!silent) {
      error.value = describeError(failure)
    }
  } finally {
    loading.value = false
    refreshing.value = false
    initialLoaded.value = true
  }
}

// containersOf lists the containers of one node.
function containersOf(node: NodeSummary): ContainerSummary[] {
  return containersByNode.value[node.id] ?? []
}

// totalOf and runningOf are the readings of the row. When the inventory was
// not read, the count of the last complete inventory stands in.
function totalOf(node: NodeSummary): number {
  const list = containersOf(node)
  return list.length > 0 ? list.length : node.container_count
}

function runningOf(node: NodeSummary): number {
  return containersOf(node).filter((item) => item.state === 'running').length
}

function stoppedOf(node: NodeSummary): number {
  return Math.max(totalOf(node) - runningOf(node), 0)
}

// appCountOf counts the sites that target the node.
function appCountOf(node: NodeSummary): number {
  return apps.value.filter((app) => app.target_id === node.id).length
}

// systemLabel writes the system of the node in one line.
function systemLabel(node: NodeSummary): string {
  const parts = [node.os_type, node.os_arch].filter((part) => part !== '')
  return parts.length > 0 ? parts.join(' / ') : '系统信息未上报'
}

// statusLabel and statusTone name the connection of the node.
function statusLabel(node: NodeSummary): string {
  if (node.revoked) return '已撤销'
  return node.connected ? '已连接' : '连接中断'
}

function statusTone(node: NodeSummary): 'ok' | 'warn' | 'off' {
  if (node.revoked) return 'off'
  return node.connected ? 'ok' : 'warn'
}

// theConnected counts the nodes whose agent sends heartbeats.
const theConnected = computed(() => nodes.value.filter((node) => node.connected).length)

// theDisconnected counts the nodes that do not send heartbeats and were not
// revoked. A revoked node never comes back, so it does not count here.
const theDisconnected = computed(
  () => nodes.value.filter((node) => !node.connected && !node.revoked).length,
)

// askForRevoke opens the confirmation of a revocation.
function askForRevoke(node: NodeSummary) {
  actionError.value = ''
  notice.value = ''
  revoking.value = node.id
}

// revoke withdraws the credential of a node.
async function revoke(node: NodeSummary) {
  if (revokeBusy.value) {
    return
  }
  revokeBusy.value = true
  actionError.value = ''
  try {
    await api<void>(`/api/v1/nodes/${encodeURIComponent(node.id)}/revoke`, { method: 'POST' })
    revoking.value = ''
    notice.value = '已撤销该节点的接入凭据，它不会再连接。'
    await load(true)
  } catch (failure) {
    actionError.value = describeError(failure)
  } finally {
    revokeBusy.value = false
  }
}

// The page follows the platform while it is open, so a node that finishes
// its enrollment appears without a refresh.
let pollTimer: ReturnType<typeof setInterval> | undefined

onMounted(() => {
  void load()
  pollTimer = setInterval(() => {
    void load(true)
  }, 15 * 1000)
})

onBeforeUnmount(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
  }
})
</script>

<template>
  <AppShell>
    <div class="page-head">
      <div>
        <h1 class="page-title">节点管理</h1>
        <p class="page-subtitle">
          每个节点运行一个 Agent，主动连接控制端并上报本机 Docker 清单。点一行查看节点详情。
        </p>
      </div>
      <div class="head-actions">
        <button type="button" class="ghost-button" :disabled="refreshing" @click="load()">
          {{ refreshing ? '刷新中…' : '刷新' }}
        </button>
        <button type="button" class="primary-button" @click="enrolling = !enrolling">
          {{ enrolling ? '收起接入' : '接入节点' }}
        </button>
      </div>
    </div>

    <NodeEnrollment v-if="enrolling" class="enroll-block" @close="enrolling = false" />

    <p v-if="notice" class="banner success">
      {{ notice }}
      <button type="button" aria-label="关闭提示" @click="notice = ''">×</button>
    </p>
    <p v-if="actionError" class="banner error" role="alert">
      {{ actionError }}
      <button type="button" aria-label="关闭提示" @click="actionError = ''">×</button>
    </p>

    <section class="nodes-panel">
      <header class="list-head">
        <div>
          <h2>节点列表</h2>
          <p>
            {{ nodes.length }} 个节点 · {{ theConnected }} 个已连接
            <template v-if="theDisconnected > 0"> · {{ theDisconnected }} 个连接中断</template>
          </p>
        </div>
        <span v-if="refreshing" class="list-busy">同步中…</span>
      </header>

      <p v-if="loading" class="state">正在读取节点…</p>

      <div v-else-if="error" class="state error">
        <p>{{ error }}</p>
        <button type="button" class="text-button" @click="load()">重试</button>
      </div>

      <div v-else-if="nodes.length === 0" class="empty">
        <span class="empty-mark" aria-hidden="true">
          <BrandMark class="empty-whale" />
        </span>
        <h3>还没有接入节点</h3>
        <p>生成一次性令牌，在目标主机上启动 Agent，节点连接后会出现在这里。</p>
        <button type="button" class="primary-button" @click="enrolling = true">接入第一个节点</button>
      </div>

      <div v-else class="node-list">
        <article
          v-for="node in nodes"
          :key="node.id"
          class="node-row"
          :class="{ revoked: node.revoked }"
        >
          <RouterLink class="node-main" :to="`/nodes/${encodeURIComponent(node.id)}`">
            <span class="node-mark" aria-hidden="true">
              <BrandMark class="node-whale" />
              <i class="node-state" :class="statusTone(node)"></i>
            </span>

            <div class="node-info">
              <div class="node-title">
                <strong>{{ node.hostname }}</strong>
                <span class="status" :class="statusTone(node)">
                  <i aria-hidden="true"></i>{{ statusLabel(node) }}
                </span>
                <span class="chip">{{ roleLabel(node.role) }}</span>
                <span v-if="node.agent_version" class="chip">Agent {{ node.agent_version }}</span>
              </div>

              <p class="node-facts">
                <span>{{ node.docker_version ? `Docker ${node.docker_version}` : 'Docker 版本未上报' }}</span>
                <span>{{ systemLabel(node) }}</span>
                <span>最近上报 {{ formatTimestamp(node.last_seen) }}</span>
              </p>

              <div class="node-stats">
                <span>
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M4 4h6v6H4V4Zm10 0h6v6h-6V4ZM4 14h6v6H4v-6Zm10 0h6v6h-6v-6Z" />
                  </svg>
                  {{ appCountOf(node) }} 个站点
                </span>
                <span>
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M12 3 3 8v8l9 5 9-5V8L12 3ZM3 8l9 5 9-5M12 13v8" />
                  </svg>
                  {{ totalOf(node) }} 个容器
                </span>
                <span class="ok">
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M3 12h4l3-8 4 16 3-8h4" />
                  </svg>
                  {{ runningOf(node) }} 运行中
                </span>
                <span v-if="stoppedOf(node) > 0" class="muted">
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M9 6v12M15 6v12" />
                  </svg>
                  {{ stoppedOf(node) }} 已停止
                </span>
              </div>
            </div>

            <span class="node-chevron" aria-hidden="true">
              <svg viewBox="0 0 24 24"><path d="m9 5 7 7-7 7" /></svg>
            </span>
          </RouterLink>

          <div class="node-actions">
            <template v-if="revoking === node.id">
              <span class="confirm">确认撤销接入？</span>
              <button type="button" class="danger" :disabled="revokeBusy" @click="revoke(node)">
                {{ revokeBusy ? '撤销中…' : '确认' }}
              </button>
              <button type="button" class="quiet" @click="revoking = ''">取消</button>
            </template>
            <button
              v-else-if="!node.revoked"
              type="button"
              class="danger-quiet"
              @click="askForRevoke(node)"
            >
              撤销接入
            </button>
          </div>
        </article>
      </div>
    </section>
  </AppShell>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 18px;
  flex-wrap: wrap;
}

.page-head .page-subtitle {
  margin-bottom: 20px;
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: none;
}

.ghost-button {
  height: 36px;
  padding: 0 15px;
  border: 1px solid #dbe6f7;
  border-radius: 9px;
  background: #fff;
  color: #2f6fe4;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}

.ghost-button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.primary-button {
  height: 36px;
  padding: 0 16px;
  border: 0;
  border-radius: 9px;
  background: #0b63f6;
  color: #fff;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  transition: background 150ms ease;
}

.primary-button:hover {
  background: #0755d8;
}

.enroll-block {
  margin-bottom: 18px;
}

.banner {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 14px;
  padding: 10px 14px;
  border-radius: 10px;
  font-size: 12.5px;
}

.banner.success {
  background: #e7f9f1;
  color: #0a8f63;
}

.banner.error {
  background: #fef0ef;
  color: #d92d20;
}

.banner button {
  margin-left: auto;
  border: 0;
  background: none;
  color: inherit;
  font-size: 15px;
  line-height: 1;
  cursor: pointer;
  opacity: 0.65;
}

.nodes-panel {
  overflow: hidden;
  border: 1px solid #e9eff9;
  border-radius: 18px;
  background: #fff;
  box-shadow: 0 18px 38px -36px rgb(20 52 110 / 50%);
}

.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  padding: 17px 22px;
  border-bottom: 1px solid #eef3fb;
}

.list-head h2 {
  display: flex;
  align-items: center;
  gap: 9px;
  color: #16264a;
  font-size: 16px;
  font-weight: 700;
}

.list-head h2::before {
  content: "";
  width: 4px;
  height: 16px;
  border-radius: 2px;
  background: #2f6fe4;
}

.list-head p {
  margin-top: 3px;
  color: #8595b2;
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.list-busy {
  color: #94a3bd;
  font-size: 11.5px;
}

.state {
  display: grid;
  place-items: center;
  gap: 8px;
  min-height: 180px;
  color: #7b8aa8;
  font-size: 13px;
}

.state.error {
  color: #d92d20;
}

.text-button {
  border: 0;
  background: none;
  color: #0b63f6;
  font-weight: 600;
  cursor: pointer;
}

.empty {
  display: grid;
  justify-items: center;
  padding: 56px 24px;
  text-align: center;
}

.empty-mark {
  width: 76px;
  height: 76px;
  display: grid;
  place-items: center;
  margin-bottom: 16px;
  border-radius: 50%;
  background: #e9f2ff;
  box-shadow: inset 0 0 0 1px rgb(47 111 228 / 8%);
}

.empty-whale {
  width: 40px;
  height: 37px;
}

.empty h3 {
  color: #16264a;
  font-size: 16px;
  font-weight: 700;
}

.empty p {
  max-width: 26rem;
  margin: 6px 0 18px;
  color: #7b8aa8;
  font-size: 12.5px;
  line-height: 1.8;
}

.node-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 16px 22px;
  transition: background 150ms ease;
}

.node-row + .node-row {
  border-top: 1px solid #f1f5fc;
}

.node-row:hover {
  background: #fbfdff;
}

.node-row.revoked {
  background: #fcfdff;
}

.node-row.revoked .node-whale {
  filter: grayscale(0.55);
  opacity: 0.7;
}

.node-main {
  display: flex;
  align-items: center;
  gap: 15px;
  flex: 1;
  min-width: 0;
  color: inherit;
  text-decoration: none;
}

.node-main:hover {
  text-decoration: none;
}

.node-mark {
  position: relative;
  width: 54px;
  height: 54px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
}

.node-whale {
  width: 30px;
  height: 28px;
}

.node-state {
  position: absolute;
  right: 2px;
  bottom: 2px;
  width: 12px;
  height: 12px;
  border: 2px solid #fff;
  border-radius: 50%;
}

.node-state.ok {
  background: #12b76a;
}

.node-state.warn {
  background: #f79009;
}

.node-state.off {
  background: #b9c4d6;
}

.node-info {
  min-width: 0;
  flex: 1;
}

.node-title {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.node-title strong {
  color: #16264a;
  font-size: 15.5px;
  font-weight: 700;
}

.status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
}

.status i {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: currentcolor;
}

.status.ok {
  background: #e7f9f1;
  color: #0a9b6a;
}

.status.warn {
  background: #fff4e6;
  color: #e07c00;
}

.status.off {
  background: #f0f3f9;
  color: #8595b2;
}

.chip {
  padding: 1px 7px;
  border-radius: 6px;
  background: #f2f6fd;
  color: #7b8aa8;
  font-size: 10.5px;
  font-weight: 500;
  white-space: nowrap;
}

.node-facts {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  margin-top: 3px;
  color: #7b8aa8;
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.node-stats {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 18px;
  margin-top: 7px;
  color: #5b6d8e;
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.node-stats span {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}

.node-stats svg {
  width: 13px;
  height: 13px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.node-stats .ok {
  color: #0a9b6a;
}

.node-stats .muted {
  color: #94a3bd;
}

.node-chevron {
  display: grid;
  place-items: center;
  flex: none;
  color: #a6b3c9;
}

.node-chevron svg {
  width: 17px;
  height: 17px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.node-actions {
  display: flex;
  align-items: center;
  gap: 7px;
  flex: none;
}

.danger-quiet {
  height: 30px;
  padding: 0 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #d92d20;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
  transition: background 150ms ease;
}

.danger-quiet:hover {
  background: #fef0ef;
}

.confirm {
  color: #7b8aa8;
  font-size: 11.5px;
  white-space: nowrap;
}

.danger {
  height: 30px;
  padding: 0 12px;
  border: 0;
  border-radius: 8px;
  background: #d92d20;
  color: #fff;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}

.danger:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.quiet {
  height: 30px;
  padding: 0 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #5b6d8e;
  font-size: 12px;
  cursor: pointer;
}

.quiet:hover {
  background: #f2f6fd;
}

@media (max-width: 900px) {
  .node-title strong {
    font-size: 15px;
  }

  .node-facts,
  .node-stats {
    gap: 4px 12px;
  }
}

@media (max-width: 720px) {
  .page-head {
    align-items: flex-start;
  }

  .head-actions {
    width: 100%;
  }

  .node-row {
    flex-wrap: wrap;
    align-items: flex-start;
    padding: 14px 16px;
  }

  .node-main {
    align-items: flex-start;
    gap: 12px;
  }

  .node-mark {
    width: 46px;
    height: 46px;
  }

  .node-whale {
    width: 26px;
    height: 24px;
  }

  .node-chevron {
    display: none;
  }

  .node-actions {
    width: 100%;
    justify-content: flex-end;
    padding-left: 58px;
  }

  .list-head {
    padding: 15px 16px;
  }
}
</style>
