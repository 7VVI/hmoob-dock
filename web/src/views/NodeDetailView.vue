<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NAlert, NButton, NCard } from 'naive-ui'
import AppShell from '../components/AppShell.vue'
import MetricChart from '../components/MetricChart.vue'
import ContainerRows from '../components/ContainerRows.vue'
import { containerOp, describeOpError, getNode, roleLabel } from '../api/nodes'
import {
  getMetrics,
  isStale,
  metricNames,
  metricPoints,
  metricScale,
  metricUnit,
  newestPoint,
  subscribeContainerEvents,
  type ContainerSummary,
  type MetricName,
} from '../api/metrics'
import {
  createLogSubscription,
  deleteLogSubscription,
  logErrorMessage,
  openLogStream,
  type LogStream,
} from '../api/logs'
import { api } from '../api/client'
import {
  ApiError,
  describeError,
  formatTimestamp,
  type NodeSummary,
} from '../api/types'

// The bounds of the logs of a container. They are the bounds of the page of
// an application as well, so a page never holds more than the platform
// promises.
const maxLogLines = 5000
const logStreamTimeout = 5 * 1000

const route = useRoute()
const router = useRouter()

const node = ref<NodeSummary | null>(null)
const loading = ref(true)
const error = ref('')

const containers = ref<ContainerSummary[]>([])
const containersBusy = ref(false)
const containersError = ref('')
const collectedAt = ref('')

// The operation that runs at the moment, and the container whose row holds
// the confirmation of a removal. Both are identifiers, so a stale answer
// never lands in the wrong row.
const operating = ref('')
const opError = ref('')
const confirmingRemove = ref('')
const opNotice = ref('')

// The container whose logs are open, and the state of the subscription.
const logContainer = ref('')
const logLines = ref<string[]>([])
const logTruncated = ref(false)
const logError = ref('')
const logBusy = ref(false)
let logStream: LogStream | null = null
let logSubscriptionID = ''

// The window of the curve.
const ranges = [
  { minutes: 60, label: '最近 1 小时' },
  { minutes: 360, label: '最近 6 小时' },
  { minutes: 1440, label: '最近 24 小时' },
]
const curveContainerID = ref('')
const metric = ref<MetricName>('cpu_percent')
const range = ref(ranges[0].minutes)
const points = ref<ReturnType<typeof metricPoints>>([])
const curveLoading = ref(false)
const curveError = ref('')
const curveLoaded = ref(false)

const nodeID = computed(() => String(route.params.id ?? ''))

// theManaged groups the containers that carry the namespace of an
// application of the platform.
const theManaged = computed(() => {
  const groups = new Map<string, ContainerSummary[]>()
  for (const item of containers.value) {
    if (item.namespace === '') {
      continue
    }
    const group = groups.get(item.namespace) ?? []
    group.push(item)
    groups.set(item.namespace, group)
  }
  return [...groups.entries()].map(([namespace, items]) => ({ namespace, items }))
})

// theUnmanaged are the containers that no application of the platform
// manages. They may be operated on, but the platform does not deploy or
// clean them.
const theUnmanaged = computed(() => containers.value.filter((item) => item.namespace === ''))

// theNodeIsConnected reports whether the agent of the node sends
// heartbeats. An operation needs the connection, so the rows of an away
// node carry no working buttons.
const theNodeIsConnected = computed(() => node.value?.connected === true)

// theNodeIsRevoked reports whether the credential of the node was
// withdrawn. A revoked node never comes back.
const theNodeIsRevoked = computed(() => node.value?.revoked === true)

// theMetricName names the metric of the curve.
const theMetricName = computed(
  () => metricNames.find((item) => item.name === metric.value)?.label ?? metric.value,
)
const theUnit = computed(() => metricUnit(metric.value))
const theScale = computed(() => metricScale(metric.value))
const theDataIsStale = computed(() => curveLoaded.value && isStale(points.value))
const theNewestAt = computed(() => newestPoint(points.value)?.at ?? '')

// loadNode reads the record and the live state of the node.
async function loadNode() {
  error.value = ''
  try {
    node.value = await getNode(nodeID.value)
  } catch (failure) {
    node.value = null
    error.value = describeError(failure)
  }
}

// loadContainers reads the containers of the last inventory.
//
// The curve follows the list: a container that is gone cannot be watched,
// and a running container is the one whose metrics tell something.
async function loadContainers() {
  containersBusy.value = true
  containersError.value = ''
  try {
    const payload = await api<{ containers: ContainerSummary[]; collected_at?: string }>(
      `/api/v1/nodes/${encodeURIComponent(nodeID.value)}/containers`,
    )
    containers.value = payload.containers ?? []
    collectedAt.value = payload.collected_at ?? ''
    if (!containers.value.some((item) => item.id === curveContainerID.value)) {
      curveContainerID.value =
        containers.value.find((item) => item.state === 'running')?.id ?? containers.value[0]?.id ?? ''
    }
  } catch (failure) {
    containersError.value = describeError(failure)
  } finally {
    containersBusy.value = false
  }
}

// operate performs one operation on one container.
//
// The answer of the node is the truth of the page: the list is read again
// after every operation, so the row shows what Docker reported and not
// what the page guessed.
async function operate(target: ContainerSummary, action: 'start' | 'stop' | 'restart' | 'remove') {
  if (operating.value !== '') {
    return
  }
  operating.value = `${action}:${target.id}`
  opError.value = ''
  opNotice.value = ''
  try {
    const result = await containerOp(nodeID.value, target.id, action)
    confirmingRemove.value = ''
    if (action === 'remove') {
      opNotice.value = `容器 ${target.name} 已删除。平台不清理它的数据卷与镜像。`
    } else if (target.namespace !== '') {
      opNotice.value = `对 ${target.name} 的操作已完成。平台外的变更会在下次部署预检中提示，健康状态以节点上报为准。`
    } else {
      opNotice.value = `对 ${target.name} 的操作已完成，健康状态以节点上报为准。`
    }
    void result
  } catch (failure) {
    opError.value = describeOpError(failure)
  } finally {
    operating.value = ''
    await loadContainers()
  }
}

// askForRemove opens the confirmation of a removal in the row.
function askForRemove(target: ContainerSummary) {
  opError.value = ''
  confirmingRemove.value = target.id
}

// cancelRemove closes the confirmation without doing anything.
function cancelRemove() {
  confirmingRemove.value = ''
}

// watchContainer opens the logs of a container.
//
// The stream of the events has to be open before the subscription is asked
// for, because the pieces of the logs travel in it. The page waits for the
// stream and reports it when it does not open.
async function watchContainer(target: ContainerSummary) {
  if (logContainer.value === target.id) {
    closeLogs()
    return
  }
  closeLogs()
  logContainer.value = target.id
  logBusy.value = true
  logError.value = ''
  try {
    logStream = openLogStream({
      onLog: (event) => {
        if (logSubscriptionID === '' || event.object_id !== logSubscriptionID) {
          return
        }
        if (event.error_code) {
          logError.value = logErrorMessage(event.error_code)
          closeLogs(true)
          return
        }
        if (event.truncated) {
          logTruncated.value = true
        }
        if (event.text) {
          appendText(event.text)
        }
      },
    })
    await Promise.race([
      logStream.ready,
      new Promise((_, reject) =>
        setTimeout(() => reject(new Error('the stream of the logs did not open')), logStreamTimeout),
      ),
    ])
    const payload = await createLogSubscription({
      node_id: nodeID.value,
      container_id: target.id,
    })
    logSubscriptionID = payload.id
  } catch (failure) {
    logError.value =
      failure instanceof ApiError ? describeError(failure) : '实时连接没有建立，请稍后重试。'
    closeLogs()
  } finally {
    logBusy.value = false
  }
}

// appendText adds a piece of the logs to the page. The lines beyond the
// limit are dropped, and the page says so.
function appendText(text: string) {
  const incoming = logLines.value.length === 0 ? text.split('\n') : text.split('\n').slice(1)
  logLines.value.push(...incoming)
  if (logLines.value.length > maxLogLines) {
    logLines.value.splice(0, logLines.value.length - maxLogLines)
    logTruncated.value = true
  }
}

// closeLogs ends the subscription and the stream of the logs. A stream that
// is left open would deliver the pieces of a page that no longer shows
// them.
function closeLogs(keepContainer = false) {
  if (logSubscriptionID !== '') {
    const id = logSubscriptionID
    logSubscriptionID = ''
    void deleteLogSubscription(id).catch(() => {})
  }
  logStream?.close()
  logStream = null
  logLines.value = []
  logTruncated.value = false
  if (!keepContainer) {
    logContainer.value = ''
  }
}

// loadCurve reads the history of the container of the curve.
async function loadCurve(): Promise<void> {
  if (curveContainerID.value === '') {
    points.value = []
    curveLoaded.value = false
    curveError.value = ''
    return
  }
  curveLoading.value = true
  curveError.value = ''
  try {
    const to = new Date()
    const from = new Date(to.getTime() - range.value * 60 * 1000)
    const payload = await getMetrics({
      node_id: nodeID.value,
      container_id: curveContainerID.value,
      from: from.toISOString(),
      to: to.toISOString(),
    })
    points.value = metricPoints(payload.buckets, metric.value, payload.from, payload.to)
    curveLoaded.value = true
  } catch (failure) {
    points.value = []
    curveLoaded.value = false
    curveError.value = describeError(failure)
  } finally {
    curveLoading.value = false
  }
}

// The confirmation of a revocation stays in the page, like the settings
// page keeps it.
const revoking = ref(false)
const revokeError = ref('')

async function revoke() {
  revokeError.value = ''
  try {
    await api<void>(`/api/v1/nodes/${encodeURIComponent(nodeID.value)}/revoke`, { method: 'POST' })
    revoking.value = false
    await loadNode()
  } catch (failure) {
    revokeError.value = describeError(failure)
  }
}

// subscription holds the events of the containers of the deployment. The
// events of this node reload its list; every open of the stream reloads it
// as well, so a reconnection never leaves an old list behind. The ticker is
// the second line of defense: the agent reports its inventory every ten
// seconds, so a page that reads at the same pace never shows a state that
// is minutes old, even when the stream of the events is held back by a
// proxy in between.
let subscription: ReturnType<typeof subscribeContainerEvents> | null = null
let pollTimer: ReturnType<typeof setInterval> | undefined

onMounted(async () => {
  await Promise.all([loadNode(), loadContainers()])
  loading.value = false
  void loadCurve()

  subscription = subscribeContainerEvents({
    onEvent: (changed) => {
      if (changed === nodeID.value) {
        void loadContainers()
        void loadNode()
      }
    },
    onOpen: () => {
      void loadContainers()
    },
  })
  pollTimer = setInterval(() => {
    void loadContainers()
  }, 15 * 1000)
})

onBeforeUnmount(() => {
  subscription?.close()
  if (pollTimer) {
    clearInterval(pollTimer)
  }
  closeLogs()
})

// A change of the route parameter is another node: the page reads it from
// the beginning.
watch(nodeID, async (current, previous) => {
  if (current === previous) {
    return
  }
  closeLogs()
  curveContainerID.value = ''
  curveLoaded.value = false
  points.value = []
  loading.value = true
  await Promise.all([loadNode(), loadContainers()])
  loading.value = false
  void loadCurve()
})

// The curve follows its selectors without a button: a change of the
// container, of the metric or of the window reads the history again.
watch([curveContainerID, metric, range], () => {
  void loadCurve()
})

// A node that went away cannot serve the logs that are open. The page
// closes them and says why.
watch(theNodeIsConnected, (connected) => {
  if (!connected && logContainer.value !== '') {
    closeLogs(true)
    logError.value = '连接中断，日志已经停止。'
  }
})
</script>

<template>
  <AppShell>
    <p v-if="loading" class="py-6 text-[#667085]">正在读取节点…</p>

    <NAlert v-else-if="error || !node" type="error" :show-icon="false" class="mb-4">
      <p>{{ error || '未找到该节点。' }}</p>
      <NButton quaternary type="primary" class="mt-2" @click="router.push('/nodes')">返回节点列表</NButton>
    </NAlert>

    <template v-else>
      <div class="mb-1 flex items-center justify-between gap-4">
        <h1 class="page-title">{{ node.hostname }}</h1>
        <NButton quaternary type="primary" :loading="containersBusy" @click="loadContainers">
          刷新清单
        </NButton>
      </div>
      <p class="page-subtitle">
        {{ roleLabel(node.role) }}
        <template v-if="node.docker_version"> · Docker {{ node.docker_version }}</template>
        · {{ containers.filter((item) => item.state === 'running').length }} /
        {{ containers.length }} 个容器运行中
      </p>

      <NAlert v-if="theNodeIsRevoked" type="error" :show-icon="false" class="mb-6">
        该节点的接入凭据已撤销。节点记录与历史数据保留，但节点不再连接、清单不再更新。
      </NAlert>
      <NAlert v-else-if="!theNodeIsConnected" type="warning" :show-icon="false" class="mb-6">
        连接中断。下面显示的是 {{ formatTimestamp(node.last_seen) }} 的最后一次上报，不代表机器已经宕机；容器操作暂不可用。
      </NAlert>

      <NCard title="节点信息" class="mb-6">
        <template #header-extra>
          <template v-if="!theNodeIsRevoked">
            <NButton v-if="!revoking" quaternary type="error" @click="revoking = true">撤销接入</NButton>
            <template v-else>
              <span class="mr-2 text-xs text-[#667085]">确认撤销接入？连接将断开，容器不受影响。</span>
              <NButton size="small" type="error" class="mr-2" @click="revoke">确认撤销</NButton>
              <NButton size="small" quaternary @click="revoking = false">取消</NButton>
            </template>
          </template>
        </template>
        <NAlert v-if="revokeError" type="error" :show-icon="false" class="mb-4">{{ revokeError }}</NAlert>

        <dl class="grid grid-cols-1 gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-4">
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">连接状态</dt>
            <dd class="text-[13px]">
              <span v-if="node.revoked" class="text-[#e02e2e]">已撤销</span>
              <span v-else :class="node.connected ? 'text-[#025ffb]' : 'text-[#b54708]'">
                {{ node.connected ? '已连接' : '连接中断' }}
              </span>
            </dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">节点角色</dt>
            <dd class="text-[13px]">{{ roleLabel(node.role) }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">节点 ID</dt>
            <dd class="font-mono text-[13px]">{{ node.id }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">Docker 环境</dt>
            <dd class="text-[13px]">{{ node.docker_version || '尚未上报' }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">操作系统 / 架构</dt>
            <dd class="text-[13px]">{{ node.os_type || '?' }} · {{ node.os_arch || '?' }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">Agent 版本</dt>
            <dd class="text-[13px]">{{ node.agent_version || '尚未上报' }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">接入时间</dt>
            <dd class="text-[13px]">{{ formatTimestamp(node.created_at) }}</dd>
          </div>
          <div class="min-w-0">
            <dt class="mb-1 text-xs text-[#667085]">最近上报</dt>
            <dd class="text-[13px]">{{ formatTimestamp(node.last_seen) }}</dd>
          </div>
        </dl>
        <p class="mt-4 text-xs text-[#667085]">
          节点信息来自该节点最近一次上报的身份与环境信息，不是实时探测结果。
        </p>
      </NCard>

      <NCard class="mb-6">
        <template #header>
          <div class="flex items-center justify-between gap-3">
            <h2 class="text-base font-semibold">容器清单</h2>
            <span class="text-xs text-[#667085]">
              {{ containers.filter((item) => item.state === 'running').length }} /
              {{ containers.length }} 个运行中
              <template v-if="collectedAt"> · 上报于 {{ formatTimestamp(collectedAt) }}</template>
            </span>
          </div>
        </template>

        <div v-if="containersError" class="mb-4 rounded-lg bg-[#fdeaea] px-4 py-3 text-xs text-[#e02e2e]">
          <p>{{ containersError }}</p>
          <NButton quaternary type="primary" class="mt-2" @click="loadContainers">重试</NButton>
        </div>

        <p v-else-if="containers.length === 0" class="py-6 text-center text-[#667085]">
          这个节点还没有上报任何容器。节点连接后会随清单一起上报。
        </p>

        <template v-else>
          <p v-if="opNotice" class="mb-4 rounded-lg bg-[#e8f1fe] px-4 py-3 text-xs leading-relaxed text-[#344054]">
            {{ opNotice }}
          </p>
          <p v-if="opError" class="mb-4 rounded-lg bg-[#fdeaea] px-4 py-3 text-xs leading-relaxed text-[#e02e2e]">
            {{ opError }}
          </p>

          <section v-for="group in theManaged" :key="group.namespace" class="mb-5">
            <h3 class="mb-2 flex flex-wrap items-baseline gap-3 text-sm font-semibold">
              <code class="font-mono">{{ group.namespace }}</code>
              <span class="text-xs font-normal text-[#667085]">平台站点容器 · 操作是运维行为，不会修改站点配置</span>
            </h3>
            <ContainerRows
              :items="group.items"
              :operating="operating"
              :confirming-remove="confirmingRemove"
              :connected="theNodeIsConnected"
              :log-container="logContainer"
              :log-lines="logLines"
              :log-truncated="logTruncated"
              :log-error="logError"
              :log-busy="logBusy"
              @operate="operate"
              @ask-remove="askForRemove"
              @cancel-remove="cancelRemove"
              @watch="watchContainer"
            />
          </section>

          <section v-if="theUnmanaged.length > 0">
            <h3 class="mb-2 flex flex-wrap items-baseline gap-3 text-sm font-semibold">
              <span>未接管的容器</span>
              <span class="text-xs font-normal text-[#667085]">不由平台部署，可查看和启停，不会纳入部署管理</span>
            </h3>
            <ContainerRows
              :items="theUnmanaged"
              :operating="operating"
              :confirming-remove="confirmingRemove"
              :connected="theNodeIsConnected"
              :log-container="logContainer"
              :log-lines="logLines"
              :log-truncated="logTruncated"
              :log-error="logError"
              :log-busy="logBusy"
              @operate="operate"
              @ask-remove="askForRemove"
              @cancel-remove="cancelRemove"
              @watch="watchContainer"
            />
          </section>
        </template>
      </NCard>

      <NCard title="资源曲线">
        <p class="mb-4 text-xs text-[#667085]">曲线是容器的，不是整台机器的。每 10 秒采样，按分钟聚合，历史保留最近 24 小时。</p>

        <form class="mb-4 flex flex-wrap items-end gap-3" novalidate @submit.prevent>
          <div class="grid min-w-[11rem] flex-1 gap-1">
            <label class="text-xs font-medium text-[#344054]" for="curve-container">容器</label>
            <select
              id="curve-container"
              v-model="curveContainerID"
              class="h-[38px] rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]"
              :disabled="containers.length === 0"
            >
              <option v-if="containers.length === 0" value="">这个节点还没有上报容器</option>
              <option v-for="item in containers" :key="item.id" :value="item.id">
                {{ item.name }} · {{ item.state }}
              </option>
            </select>
          </div>

          <div class="grid min-w-[11rem] flex-1 gap-1">
            <label class="text-xs font-medium text-[#344054]" for="curve-metric">指标</label>
            <select
              id="curve-metric"
              v-model="metric"
              class="h-[38px] rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]"
            >
              <option v-for="item in metricNames" :key="item.name" :value="item.name">
                {{ item.label }}
              </option>
            </select>
          </div>

          <div class="grid min-w-[11rem] flex-1 gap-1">
            <label class="text-xs font-medium text-[#344054]" for="curve-range">时间范围</label>
            <select
              id="curve-range"
              v-model.number="range"
              class="h-[38px] rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]"
            >
              <option v-for="item in ranges" :key="item.minutes" :value="item.minutes">
                {{ item.label }}
              </option>
            </select>
          </div>
        </form>

        <p v-if="curveError" class="mb-4 rounded-lg bg-[#fdeaea] px-4 py-3 text-xs text-[#e02e2e]">
          <span>{{ curveError }}</span>
          <NButton quaternary type="primary" class="ml-2" @click="loadCurve">重试</NButton>
        </p>

        <NAlert v-if="!theNodeIsConnected" type="warning" :show-icon="false" class="mb-4">
          连接中断。曲线停在最后一次收到数据的时候。
        </NAlert>
        <NAlert v-else-if="theDataIsStale" type="warning" :show-icon="false" class="mb-4">
          数据已过期。最后一次数据来自
          {{ theNewestAt === '' ? '更早' : formatTimestamp(theNewestAt) }}。
        </NAlert>

        <p v-if="!curveLoaded && !curveLoading && curveError === ''" class="py-6 text-center text-[#667085]">
          这个节点还没有可以画曲线的容器。
        </p>

        <MetricChart
          v-if="curveLoaded"
          :points="points"
          :unit="theUnit"
          :title="theMetricName"
          :format="theScale"
        />
      </NCard>
    </template>
  </AppShell>
</template>
