<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import LogViewer from '../components/LogViewer.vue'
import MetricChart from '../components/MetricChart.vue'
import {
  containerOp,
  describeOpError,
  getNode,
  getNodeContainer,
  type ContainerDetail,
} from '../api/nodes'
import {
  getMetrics,
  isStale,
  listContainers,
  metricNames,
  metricPoints,
  metricScale,
  metricUnit,
  newestPoint,
  subscribeContainerEvents,
  type MetricName,
} from '../api/metrics'
import {
  createLogSubscription,
  deleteLogSubscription,
  logErrorMessage,
  openLogStream,
  type LogStream,
} from '../api/logs'
import {
  ApiError,
  describeError,
  formatTimestamp,
  type NodeSummary,
  type PortMapping,
} from '../api/types'

// The page of one container: the facts of the container, the fixed
// operations of the protocol, the logs and the curve. It is the page a
// container of a node opens into, so the list of a node stays a list.
const maxLogLines = 5000
const logStreamTimeout = 5 * 1000

const route = useRoute()
const router = useRouter()

const node = ref<NodeSummary | null>(null)
const container = ref<ContainerDetail | null>(null)
const loading = ref(true)
const error = ref('')
const refreshing = ref(false)
const partial = ref(false)

const operating = ref('')
const opError = ref('')
const opNotice = ref('')
const confirmingRemove = ref(false)

const tab = ref<'logs' | 'metrics'>('logs')

// The logs of the container.
const logLines = ref<string[]>([])
const logTruncated = ref(false)
const logError = ref('')
const logBusy = ref(false)
const logOpen = ref(false)
let logStream: LogStream | null = null
let logSubscriptionID = ''

// The window of the curve.
const ranges = [
  { minutes: 60, label: '最近 1 小时' },
  { minutes: 360, label: '最近 6 小时' },
  { minutes: 1440, label: '最近 24 小时' },
]
const metric = ref<MetricName>('cpu_percent')
const range = ref(ranges[0].minutes)
const points = ref<ReturnType<typeof metricPoints>>([])
const curveLoading = ref(false)
const curveError = ref('')
const curveLoaded = ref(false)

const nodeID = computed(() => String(route.params.id ?? ''))
const containerID = computed(() => String(route.params.cid ?? ''))
const theNodeIsConnected = computed(() => node.value?.connected === true)

// canRemove reports whether the container may be removed. A container that
// a site of the platform manages belongs to its YAML, so only the
// containers outside of the platform may be removed here.
const canRemove = computed(
  () => theNodeIsConnected.value && container.value !== null && container.value.namespace === '',
)
const canStart = computed(
  () => theNodeIsConnected.value && container.value !== null && container.value.state !== 'running',
)
const canStop = computed(
  () => theNodeIsConnected.value && container.value !== null && container.value.state === 'running',
)

// theMetricName names the metric of the curve.
const theMetricName = computed(
  () => metricNames.find((item) => item.name === metric.value)?.label ?? metric.value,
)
const theUnit = computed(() => metricUnit(metric.value))
const theScale = computed(() => metricScale(metric.value))
const theDataIsStale = computed(() => curveLoaded.value && isStale(points.value))
const theNewestAt = computed(() => newestPoint(points.value)?.at ?? '')

// readContainer reads the details of the container. A control端 that does
// not serve the detail endpoint yet answers with the list of the node: the
// page keeps working with the facts of the list and says which facts are
// missing.
async function readContainer(): Promise<ContainerDetail> {
  try {
    partial.value = false
    return await getNodeContainer(nodeID.value, containerID.value)
  } catch (failure) {
    const list = await listContainers(nodeID.value)
    const found = list.find((item) => item.id === containerID.value)
    if (found === undefined) {
      throw failure
    }
    partial.value = true
    return { ...found, mounts: [], labels: {} }
  }
}

// load reads the node and the container.
async function load(silent = false) {
  if (!silent) {
    loading.value = true
  } else {
    refreshing.value = true
  }
  error.value = ''
  try {
    const [nodeRecord, detail] = await Promise.all([getNode(nodeID.value), readContainer()])
    node.value = nodeRecord
    container.value = detail
  } catch (failure) {
    container.value = null
    error.value = describeError(failure)
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

// loadContainer reads the container again after an operation.
async function loadContainer() {
  try {
    container.value = await readContainer()
    error.value = ''
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// operate performs one operation of the protocol on the container.
async function operate(action: 'start' | 'stop' | 'restart' | 'remove') {
  if (operating.value !== '' || container.value === null) {
    return
  }
  operating.value = action
  opError.value = ''
  opNotice.value = ''
  try {
    await containerOp(nodeID.value, container.value.id, action)
    confirmingRemove.value = false
    if (action === 'remove') {
      await router.push(`/nodes/${encodeURIComponent(nodeID.value)}`)
      return
    }
    opNotice.value = '操作已完成。健康状态以节点下一次上报为准。'
    await loadContainer()
  } catch (failure) {
    opError.value = describeOpError(failure)
  } finally {
    operating.value = ''
  }
}

// stateLabel and stateTone name the state of the container.
function stateLabel(state: string): string {
  switch (state) {
    case 'running':
      return '运行中'
    case 'exited':
      return '已停止'
    case 'created':
      return '已创建'
    case 'paused':
      return '已暂停'
    case 'restarting':
      return '重启中'
    case 'dead':
      return '已死亡'
    case 'removing':
      return '删除中'
    default:
      return state === '' ? '未知' : state
  }
}

function stateTone(state: string): 'ok' | 'bad' | 'warn' | 'off' {
  if (state === 'running') return 'ok'
  if (state === 'exited' || state === 'dead') return 'bad'
  if (state === 'restarting' || state === 'paused' || state === 'created') return 'warn'
  return 'off'
}

function healthLabel(health: string): string {
  switch (health) {
    case 'healthy':
      return '健康'
    case 'unhealthy':
      return '不健康'
    case 'starting':
      return '检查中'
    default:
      return '未配置健康检查'
  }
}

// portText writes one port the way the page shows it.
function portText(port: PortMapping): string {
  const published = port.public_port ? `${port.public_port}:` : ''
  return `${published}${port.private_port}/${port.type || 'tcp'}`
}

// formatDate writes a date of the engine in the local time.
function formatDate(value: string | null | undefined): string {
  if (!value) {
    return '—'
  }
  const at = new Date(value)
  if (Number.isNaN(at.getTime())) {
    return '—'
  }
  return at.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
}

// openLogs starts the subscription of the logs.
async function openLogs() {
  if (logOpen.value || logBusy.value || container.value === null || !theNodeIsConnected.value) {
    return
  }
  closeLogs()
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
          closeLogs()
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
      container_id: containerID.value,
    })
    logSubscriptionID = payload.id
    logOpen.value = true
  } catch (failure) {
    logError.value =
      failure instanceof ApiError ? describeError(failure) : '实时连接没有建立，请稍后重试。'
    closeLogs()
  } finally {
    logBusy.value = false
  }
}

// appendText adds a piece of the logs to the page.
function appendText(text: string) {
  const incoming = logLines.value.length === 0 ? text.split('\n') : text.split('\n').slice(1)
  logLines.value.push(...incoming)
  if (logLines.value.length > maxLogLines) {
    logLines.value.splice(0, logLines.value.length - maxLogLines)
    logTruncated.value = true
  }
}

// closeLogs ends the subscription and the stream.
function closeLogs() {
  if (logSubscriptionID !== '') {
    const id = logSubscriptionID
    logSubscriptionID = ''
    void deleteLogSubscription(id).catch(() => {})
  }
  logStream?.close()
  logStream = null
  logOpen.value = false
}

// loadCurve reads the history of the container.
async function loadCurve(): Promise<void> {
  if (container.value === null) {
    return
  }
  curveLoading.value = true
  curveError.value = ''
  try {
    const to = new Date()
    const from = new Date(to.getTime() - range.value * 60 * 1000)
    const payload = await getMetrics({
      node_id: nodeID.value,
      container_id: containerID.value,
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

// subscription reloads the container when its node reports a change.
let subscription: ReturnType<typeof subscribeContainerEvents> | null = null
let pollTimer: ReturnType<typeof setInterval> | undefined

onMounted(async () => {
  await load()
  subscription = subscribeContainerEvents({
    onEvent: (changed) => {
      if (changed === nodeID.value) {
        void load()
      }
    },
    onOpen: () => {
      void load()
    },
  })
  pollTimer = setInterval(() => {
    void load(true)
  }, 15 * 1000)
})

onBeforeUnmount(() => {
  subscription?.close()
  if (pollTimer) {
    clearInterval(pollTimer)
  }
  closeLogs()
})

// A container that is gone cannot keep its page.
watch(container, (current) => {
  if (current !== null && tab.value === 'metrics') {
    void loadCurve()
  }
})

// A change of the tab reads the curve on demand; the logs wait for the
// administrator to ask for them, so a visit does not open a stream.
watch(tab, (current) => {
  if (current !== 'logs') {
    closeLogs()
  }
  if (current === 'metrics') {
    void loadCurve()
  }
})

// The curve follows its selectors without a button.
watch([metric, range], () => {
  if (tab.value === 'metrics') {
    void loadCurve()
  }
})

// A node that went away cannot serve the logs that are open.
watch(theNodeIsConnected, (connected) => {
  if (!connected && logOpen.value) {
    closeLogs()
    logError.value = '连接中断，日志已经停止。'
  }
})
</script>

<template>
  <AppShell>
    <p v-if="loading" class="state">正在读取容器…</p>

    <div v-else-if="error || !container || !node" class="state error">
      <p>{{ error || '未找到该容器。' }}</p>
      <RouterLink class="text-link" :to="`/nodes/${encodeURIComponent(nodeID)}`">返回节点</RouterLink>
    </div>

    <div v-else class="container-page">
      <nav class="crumb" aria-label="面包屑">
        <RouterLink to="/nodes">节点管理</RouterLink>
        <span aria-hidden="true">/</span>
        <RouterLink :to="`/nodes/${encodeURIComponent(node.id)}`">{{ node.hostname }}</RouterLink>
        <span aria-hidden="true">/</span>
        <span>{{ container.name }}</span>
      </nav>

      <header class="container-head">
        <div class="head-main">
          <span class="container-mark" aria-hidden="true">
            <svg viewBox="0 0 24 24">
              <path d="M12 3 3 8v8l9 5 9-5V8L12 3ZM3 8l9 5 9-5M12 13v8" />
            </svg>
          </span>
          <div class="head-copy">
            <div class="title-line">
              <h1>{{ container.name }}</h1>
              <span class="status" :class="stateTone(container.state)">
                <i aria-hidden="true"></i>{{ stateLabel(container.state) }}
              </span>
              <span v-if="container.health" class="health-chip">{{ healthLabel(container.health) }}</span>
            </div>
            <p class="image-line"><code>{{ container.image }}</code></p>
          </div>
        </div>

        <div class="head-actions">
          <template v-if="confirmingRemove">
            <span class="confirm">确认删除？不可恢复，数据卷保留。</span>
            <button type="button" class="danger" :disabled="operating !== ''" @click="operate('remove')">
              {{ operating === 'remove' ? '删除中…' : '确认删除' }}
            </button>
            <button type="button" class="quiet" @click="confirmingRemove = false">取消</button>
          </template>
          <template v-else>
            <button type="button" class="action" :disabled="!canStart || operating !== ''" @click="operate('start')">
              {{ operating === 'start' ? '启动中…' : '启动' }}
            </button>
            <button type="button" class="action" :disabled="!canStop || operating !== ''" @click="operate('stop')">
              {{ operating === 'stop' ? '停止中…' : '停止' }}
            </button>
            <button type="button" class="action" :disabled="!canStop || operating !== ''" @click="operate('restart')">
              {{ operating === 'restart' ? '重启中…' : '重启' }}
            </button>
            <button
              v-if="canRemove"
              type="button"
              class="danger-quiet"
              :disabled="operating !== ''"
              @click="confirmingRemove = true"
            >
              删除
            </button>
          </template>
        </div>
      </header>

      <p v-if="!theNodeIsConnected" class="banner warn">
        节点的连接已中断，容器操作与实时日志暂不可用。下面显示的是节点最后一次上报。
      </p>
      <p v-if="partial" class="banner warn">
        控制端或 Agent 还是旧版本，容器的挂载与标签暂不可用；重启控制端与 Agent 后可看到完整信息。
      </p>
      <p v-if="opNotice" class="banner success">{{ opNotice }}</p>
      <p v-if="opError" class="banner error" role="alert">{{ opError }}</p>

      <section class="facts panel">
        <header class="panel-head">
          <h2>容器信息</h2>
          <button type="button" class="refresh" :disabled="refreshing" @click="load(true)">
            {{ refreshing ? '刷新中…' : '刷新' }}
          </button>
        </header>
        <dl class="facts-grid">
          <div><dt>容器 ID</dt><dd class="mono">{{ container.id }}</dd></div>
          <div><dt>镜像</dt><dd class="mono">{{ container.image }}</dd></div>
          <div><dt>状态</dt><dd>{{ stateLabel(container.state) }} · {{ healthLabel(container.health) }}</dd></div>
          <div><dt>创建时间</dt><dd>{{ formatDate(container.created) }}</dd></div>
          <div><dt>启动时间</dt><dd>{{ formatDate(container.started_at) }}</dd></div>
          <div><dt>IP 地址</dt><dd class="mono">{{ container.ip || '—' }}</dd></div>
          <div><dt>堆栈 / 命名空间</dt><dd><code v-if="container.namespace">{{ container.namespace }}</code><span v-else class="muted">未接管</span></dd></div>
          <div><dt>服务</dt><dd>{{ container.service || '—' }}</dd></div>
          <div><dt>重启次数</dt><dd>{{ container.restart_count ?? 0 }}</dd></div>
          <div><dt>退出码</dt><dd>{{ container.exit_code ?? '—' }}</dd></div>
          <div class="wide"><dt>端口</dt><dd class="chips"><span v-for="port in container.ports" :key="`${port.private_port}-${port.public_port}-${port.type}`" class="chip mono">{{ portText(port) }}</span><span v-if="!container.ports || container.ports.length === 0" class="muted">—</span></dd></div>
          <div class="wide"><dt>挂载</dt><dd class="mounts"><template v-if="container.mounts && container.mounts.length > 0"><span v-for="mount in container.mounts" :key="mount.destination" class="mount"><code>{{ mount.source || mount.type }}</code> → <code>{{ mount.destination }}</code><small>{{ mount.mode || '' }}{{ mount.read_write ? ' · 可写' : ' · 只读' }}</small></span></template><span v-else class="muted">—</span></dd></div>
        </dl>

        <details v-if="container.labels && Object.keys(container.labels).length > 0" class="labels">
          <summary>标签（{{ Object.keys(container.labels).length }}）</summary>
          <dl>
            <div v-for="(value, key) in container.labels" :key="key">
              <dt class="mono">{{ key }}</dt>
              <dd class="mono">{{ value }}</dd>
            </div>
          </dl>
        </details>
      </section>

      <section class="panel">
        <nav class="tabs" aria-label="容器的数据">
          <button type="button" :class="{ active: tab === 'logs' }" :aria-selected="tab === 'logs'" @click="tab = 'logs'">
            日志
          </button>
          <button type="button" :class="{ active: tab === 'metrics' }" :aria-selected="tab === 'metrics'" @click="tab = 'metrics'">
            资源曲线
          </button>
        </nav>

        <div v-if="tab === 'logs'" class="tab-body">
          <div class="toolbar">
            <span class="toolbar-note">实时日志最多保留 {{ maxLogLines }} 行。</span>
            <button v-if="logOpen" type="button" class="quiet" @click="closeLogs">停止日志</button>
            <button v-else type="button" class="small-primary" :disabled="logBusy || !theNodeIsConnected" @click="openLogs">
              {{ logBusy ? '连接中…' : '开始日志' }}
            </button>
          </div>
          <p v-if="logError" class="banner warn">{{ logError }}</p>
          <LogViewer v-if="logOpen || logLines.length > 0" :lines="logLines" :truncated="logTruncated" />
          <p v-else-if="!logBusy && logError === ''" class="empty-note">
            {{ theNodeIsConnected ? '点「开始日志」读取容器的实时日志。' : '节点连接中断，无法读取实时日志。' }}
          </p>
        </div>

        <div v-else class="tab-body">
          <form class="curve-toolbar" @submit.prevent>
            <div class="curve-field">
              <label for="metric">指标</label>
              <select id="metric" v-model="metric">
                <option v-for="item in metricNames" :key="item.name" :value="item.name">{{ item.label }}</option>
              </select>
            </div>
            <div class="curve-field">
              <label for="range">时间范围</label>
              <select id="range" v-model.number="range">
                <option v-for="item in ranges" :key="item.minutes" :value="item.minutes">{{ item.label }}</option>
              </select>
            </div>
            <span class="toolbar-note">每 10 秒采样，按分钟聚合。</span>
          </form>

          <p v-if="curveError" class="banner error">{{ curveError }}</p>
          <p v-else-if="curveLoading" class="empty-note">正在读取曲线…</p>
          <p v-else-if="!curveLoaded" class="empty-note">这个容器还没有可以画曲线的采样。</p>
          <template v-else>
            <p v-if="theDataIsStale" class="banner warn">
              数据已过期。最后一次数据来自 {{ theNewestAt === '' ? '更早' : formatTimestamp(theNewestAt) }}。
            </p>
            <MetricChart :points="points" :unit="theUnit" :title="theMetricName" :format="theScale" />
          </template>
        </div>
      </section>
    </div>
  </AppShell>
</template>

<style scoped>
.state {
  display: grid;
  place-items: center;
  gap: 10px;
  min-height: 220px;
  color: #7b8aa8;
  font-size: 13px;
}

.state.error {
  color: #d92d20;
}

.text-link {
  color: #0b63f6;
  font-size: 13px;
  font-weight: 600;
  text-decoration: none;
}

.container-page {
  display: grid;
  gap: 16px;
}

.crumb {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  color: #8595b2;
  font-size: 12.5px;
}

.crumb a {
  color: #7b8aa8;
  text-decoration: none;
}

.crumb a:hover {
  color: #0b63f6;
}

.container-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.head-main {
  display: flex;
  align-items: center;
  gap: 13px;
  min-width: 0;
}

.container-mark {
  width: 48px;
  height: 48px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.container-mark svg {
  width: 24px;
  height: 24px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.title-line {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.title-line h1 {
  color: #132a56;
  font-size: 25px;
  font-weight: 800;
  letter-spacing: -0.02em;
  overflow-wrap: anywhere;
}

.status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 9px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
}

.status i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentcolor;
}

.status.ok {
  background: #e7f9f1;
  color: #0a9b6a;
}

.status.bad {
  background: #fef0ef;
  color: #d92d20;
}

.status.warn {
  background: #fff4e6;
  color: #e07c00;
}

.status.off {
  background: #f0f3f9;
  color: #8595b2;
}

.health-chip {
  padding: 2px 8px;
  border-radius: 999px;
  background: #f2f6fd;
  color: #7b8aa8;
  font-size: 10.5px;
  font-weight: 600;
}

.image-line {
  margin-top: 3px;
  color: #7b8aa8;
}

.image-line code {
  color: #5b6d8e;
  font-size: 11.5px;
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.action,
.quiet,
.small-primary,
.danger,
.danger-quiet {
  height: 32px;
  padding: 0 13px;
  border-radius: 8px;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
  white-space: nowrap;
}

.action {
  border: 1px solid #dbe6f7;
  background: #fff;
  color: #2f6fe4;
}

.action:hover:not(:disabled) {
  background: #f2f7ff;
}

.action:disabled,
.small-primary:disabled,
.danger:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.quiet {
  border: 0;
  background: transparent;
  color: #5b6d8e;
}

.quiet:hover {
  background: #f2f6fd;
}

.small-primary {
  border: 0;
  background: #0b63f6;
  color: #fff;
}

.danger {
  border: 0;
  background: #d92d20;
  color: #fff;
}

.danger-quiet {
  border: 0;
  background: transparent;
  color: #d92d20;
}

.danger-quiet:hover {
  background: #fef0ef;
}

.confirm {
  color: #7b8aa8;
  font-size: 11.5px;
}

.banner {
  padding: 10px 14px;
  border-radius: 10px;
  font-size: 12.5px;
  line-height: 1.7;
}

.banner.error {
  background: #fef0ef;
  color: #d92d20;
}

.banner.warn {
  background: #fff8eb;
  color: #9a6a2a;
}

.banner.success {
  background: #e7f9f1;
  color: #0a8f63;
}

.panel {
  overflow: hidden;
  border: 1px solid #e9eff9;
  border-radius: 18px;
  background: #fff;
  box-shadow: 0 18px 38px -36px rgb(20 52 110 / 50%);
}

.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 16px 22px;
  border-bottom: 1px solid #eef3fb;
}

.panel-head h2 {
  color: #16264a;
  font-size: 15.5px;
  font-weight: 700;
}

.refresh {
  border: 0;
  background: none;
  color: #2f6fe4;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}

.refresh:disabled {
  opacity: 0.55;
}

.facts-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 16px 26px;
  margin: 0;
  padding: 20px 22px;
}

.facts-grid > div {
  min-width: 0;
}

.facts-grid .wide {
  grid-column: 1 / -1;
}

.facts-grid dt {
  margin-bottom: 3px;
  color: #94a3bd;
  font-size: 11px;
}

.facts-grid dd {
  color: #34446b;
  font-size: 12.5px;
  overflow-wrap: anywhere;
}

.mono {
  font-family: var(--font-mono);
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.muted {
  color: #94a3bd;
}

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.chip {
  padding: 2px 8px;
  border-radius: 6px;
  background: #f2f6fd;
  color: #5b6d8e;
  font-size: 11px;
}

.mounts {
  display: grid;
  gap: 7px;
}

.mount {
  display: flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
  color: #5b6d8e;
  font-size: 11.5px;
}

.mount code {
  color: #34446b;
  font-size: 11px;
}

.mount small {
  color: #94a3bd;
  font-size: 10.5px;
}

.labels {
  border-top: 1px solid #eef3fb;
  padding: 14px 22px 18px;
}

.labels summary {
  color: #2f6fe4;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}

.labels dl {
  display: grid;
  gap: 7px;
  margin: 12px 0 0;
}

.labels dl > div {
  display: grid;
  grid-template-columns: minmax(180px, 0.8fr) minmax(0, 1.4fr);
  gap: 16px;
}

.labels dt {
  color: #8595b2;
  font-size: 10.5px;
  overflow-wrap: anywhere;
}

.labels dd {
  color: #5b6d8e;
  font-size: 10.5px;
  overflow-wrap: anywhere;
}

.tabs {
  display: flex;
  gap: 24px;
  padding: 0 22px;
  border-bottom: 1px solid #eef3fb;
}

.tabs button {
  height: 50px;
  padding: 0 1px;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: #7b8aa8;
  font-size: 13.5px;
  font-weight: 500;
  cursor: pointer;
}

.tabs button.active {
  border-color: #0b63f6;
  color: #16264a;
  font-weight: 700;
}

.tab-body {
  padding: 18px 22px 22px;
}

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.toolbar-note {
  color: #94a3bd;
  font-size: 11.5px;
}

.curve-toolbar {
  display: flex;
  align-items: flex-end;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}

.curve-field {
  display: grid;
  gap: 5px;
}

.curve-field label {
  color: #7b8aa8;
  font-size: 11px;
  font-weight: 600;
}

.curve-field select {
  height: 34px;
  min-width: 150px;
  padding: 0 10px;
  border: 1px solid #dbe6f7;
  border-radius: 9px;
  background: #fff;
  color: #34446b;
  font-size: 12.5px;
}

.empty-note {
  display: grid;
  place-items: center;
  min-height: 120px;
  color: #94a3bd;
  font-size: 12px;
  text-align: center;
}

@media (max-width: 1000px) {
  .facts-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 640px) {
  .title-line h1 {
    font-size: 21px;
  }

  .facts-grid {
    grid-template-columns: minmax(0, 1fr);
    padding: 16px;
  }

  .panel-head,
  .tab-body {
    padding: 14px 16px;
  }

  .labels {
    padding: 14px 16px;
  }

  .labels dl > div {
    grid-template-columns: minmax(0, 1fr);
    gap: 2px;
  }
}
</style>
