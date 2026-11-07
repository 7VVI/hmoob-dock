<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import DockerHeroArt from '../components/DockerHeroArt.vue'
import { listNodes } from '../api/nodes'
import { listApplications } from '../api/deployments'
import { getMetrics, listContainers, type ContainerSummary } from '../api/metrics'
import { listAlerts } from '../api/alerts'
import { describeError, type NodeSummary } from '../api/types'
import type { AlertEvent } from '../api/types'
import type { AppSummary } from '../api/types'

// The overview follows the approved design: a hero with the title of the
// platform, four readings, the sites with their services and containers,
// the state of the whole deployment with its trend, and the newest alerts.

const router = useRouter()

const nodes = ref<NodeSummary[]>([])
const apps = ref<AppSummary[]>([])
const containersByNode = ref<Record<string, ContainerSummary[]>>({})
const alerts = ref<AlertEvent[]>([])
const loading = ref(true)
const error = ref('')

// load reads everything the overview shows. A container list that fails
// for one node does not sink the page: the node counts as read-empty and
// the page shows what it knows.
async function load() {
  loading.value = true
  error.value = ''
  try {
    const [nodeList, appList, alertList] = await Promise.all([
      listNodes(),
      listApplications(),
      listAlerts(false),
    ])
    nodes.value = nodeList
    apps.value = appList
    alerts.value = alertList

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
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
  // The trend reads the history after the inventories, so the containers
  // are known. A trend that fails leaves the card without its chart: the
  // counts of the page still come from the inventories.
  trendLoading.value = true
  try {
    await loadTrend()
  } finally {
    trendLoading.value = false
  }
}

// containersOf lists the containers of a node, or nothing.
function containersOf(nodeID: string): ContainerSummary[] {
  return containersByNode.value[nodeID] ?? []
}

// allContainers is every container the last inventories reported.
const allContainers = computed(() =>
  nodes.value.flatMap((node) => containersOf(node.id)),
)

// serviceRows groups the containers into the services of the sites. A
// service is a name inside the namespace of a site. A service whose
// containers all run is healthy, one with a part of them running is
// degraded, and one without a running container is in maintenance.
type ServiceState = 'running' | 'degraded' | 'maintenance'

interface ServiceRow {
  key: string
  namespace: string
  name: string
  containers: ContainerSummary[]
  running: number
  state: ServiceState
}

const services = computed<ServiceRow[]>(() => {
  const records = new Map<string, ServiceRow>()
  for (const item of allContainers.value) {
    if (!item.namespace || !item.service) {
      continue
    }
    const key = `${item.namespace}/${item.service}`
    let record = records.get(key)
    if (!record) {
      record = {
        key,
        namespace: item.namespace,
        name: item.service,
        containers: [],
        running: 0,
        state: 'running',
      }
      records.set(key, record)
    }
    record.containers.push(item)
  }
  for (const record of records.values()) {
    const running = record.containers.filter((item) => item.state === 'running').length
    const unhealthy = record.containers.some(
      (item) => item.state === 'running' && item.health === 'unhealthy',
    )
    record.running = running
    if (running === 0) {
      record.state = 'maintenance'
    } else if (running < record.containers.length || unhealthy) {
      record.state = 'degraded'
    } else {
      record.state = 'running'
    }
  }
  return [...records.values()]
})

// theServiceCount counts the distinct services of the sites.
const theServiceCount = computed(() => services.value.length)

// theHealthyServices counts the services whose containers all run.
const theHealthyServices = computed(
  () => services.value.filter((item) => item.state === 'running').length,
)

// theHealthRate is the share of the services that run. Without a service
// the page shows no rate instead of inventing one.
const theHealthRate = computed(() => {
  const total = services.value.length
  return total === 0 ? null : Math.round((theHealthyServices.value / total) * 1000) / 10
})

// theHealthText writes the rate the way the page shows it.
const theHealthText = computed(() =>
  theHealthRate.value === null ? '—' : `${theHealthRate.value}%`,
)

// siteStates joins every site of the platform with the containers of its
// node. A site whose node is away cannot be measured, and the page says so
// instead of guessing a state.
type SiteState = {
  id: string
  name: string
  mode: string
  namespace: string
  target: string
  connected: boolean
  revoked: boolean
  serviceCount: number
  containerCount: number
  runningCount: number
  state: 'healthy' | 'partial' | 'undeployed' | 'offline'
}

const sites = computed<SiteState[]>(() =>
  apps.value.map((app) => {
    const node = nodes.value.find((item) => item.id === app.target_id)
    const list = containersOf(app.target_id).filter(
      (item) => item.namespace === app.namespace,
    )
    const running = list.filter((item) => item.state === 'running').length
    const unhealthy = list.some(
      (item) => item.state === 'running' && item.health === 'unhealthy',
    )
    const siteServices = services.value.filter(
      (item) => item.namespace === app.namespace,
    )
    let state: SiteState['state']
    if (!node || node.revoked || !node.connected) {
      state = 'offline'
    } else if (list.length === 0) {
      state = 'undeployed'
    } else if (running === list.length && !unhealthy) {
      state = 'healthy'
    } else {
      state = 'partial'
    }
    return {
      id: app.id,
      name: app.name,
      mode: app.mode,
      namespace: app.namespace,
      target: app.target_id,
      connected: node?.connected ?? false,
      revoked: node?.revoked ?? true,
      serviceCount: siteServices.length,
      containerCount: list.length,
      runningCount: running,
      state,
    }
  }),
)

// stateLabel names the state of a site the way the design names it.
function stateLabel(site: SiteState): string {
  switch (site.state) {
    case 'healthy':
      return '正常运行'
    case 'partial':
      return '部分异常'
    case 'offline':
      return '节点断线'
    default:
      return '未部署'
  }
}

// siteSegments draws the services of a site as the four soft segments of
// the design: a healthy service is green, a degraded one orange and a
// service without a running container keeps the rest tone. The slots that
// remain are pale.
type SegmentTone = 'ok' | 'warn' | 'maint' | 'empty'

function siteSegments(site: SiteState): SegmentTone[] {
  const tones: SegmentTone[] = services.value
    .filter((item) => item.namespace === site.namespace)
    .slice(0, 4)
    .map((item) => {
      if (item.state === 'running') return 'ok'
      if (item.state === 'degraded') return 'warn'
      return 'maint'
    })
  while (tones.length < 4) {
    tones.push('empty')
  }
  return tones
}

// theNewestAlerts are the events the page shows: the newest one, and the
// link of the card opens the whole history.
const theNewestAlerts = computed(() => {
  const sorted = [...alerts.value].sort((left, right) =>
    right.started_at.localeCompare(left.started_at),
  )
  return sorted.slice(0, 1)
})

// The parts of the donut: the services in each of the three states.
const donutParts = computed(() => [
  {
    key: 'running',
    label: '运行中',
    color: '#12b76a',
    count: services.value.filter((item) => item.state === 'running').length,
  },
  {
    key: 'degraded',
    label: '异常',
    color: '#f79009',
    count: services.value.filter((item) => item.state === 'degraded').length,
  },
  {
    key: 'maintenance',
    label: '维护中',
    color: '#c9d7ee',
    count: services.value.filter((item) => item.state === 'maintenance').length,
  },
])

// donutSegments turns the parts into arcs of one ring. The arcs start at
// the top and keep a small gap between them.
const donutSegments = computed(() => {
  const total = services.value.length
  if (total === 0) {
    return []
  }
  const radius = 63
  const circumference = 2 * Math.PI * radius
  const gap = total > 1 ? 5 : 0
  let start = 0
  return donutParts.value
    .filter((part) => part.count > 0)
    .map((part) => {
      const length = (part.count / total) * circumference
      const visible = Math.max(length - gap, 0.8)
      const segment = {
        color: part.color,
        dash: `${visible} ${circumference - visible}`,
        offset: -start,
      }
      start += length
      return segment
    })
})

// The trend of the deployment: for every minute of the last day, the number
// of the containers that reported a sample. A container that reports is a
// container that runs, so the series is the honest history of the running
// containers of the whole deployment. The buckets stay in the memory of the
// page, so a change of the window does not read them again.
const trendRange = ref<60 | 360 | 1440>(1440)
const trendMinutes = ref<{ at: number; running: number }[]>([])
const trendLoading = ref(false)
const trendTotal = computed(() => allContainers.value.length)

// trendRangeLabel names the window of the chart.
const trendRangeLabel = computed(() =>
  trendRange.value === 60 ? '1 小时' : trendRange.value === 360 ? '6 小时' : '24 小时',
)

// loadTrend reads the minute history of every container of the deployment
// and counts, for every minute, the containers that reported a sample.
async function loadTrend() {
  const pairs = nodes.value.flatMap((node) =>
    containersOf(node.id).map((container) => ({ node: node.id, container: container.id })),
  )
  if (pairs.length === 0) {
    trendMinutes.value = []
    return
  }
  const to = new Date()
  const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
  const perContainer = await Promise.all(
    pairs.map(async (pair) => {
      try {
        const payload = await getMetrics({
          node_id: pair.node,
          container_id: pair.container,
          from: from.toISOString(),
          to: to.toISOString(),
        })
        return payload.buckets
      } catch {
        return []
      }
    }),
  )

  const minute = 60 * 1000
  const start = Math.floor(from.getTime() / minute) * minute
  const end = Math.floor(to.getTime() / minute) * minute
  const counts = new Map<number, number>()
  for (const buckets of perContainer) {
    for (const bucket of buckets) {
      const at = Math.floor(Date.parse(bucket.start) / minute) * minute
      if (Number.isNaN(at) || at < start || at >= end) {
        continue
      }
      if (bucket.average.cpu_percent !== null) {
        counts.set(at, (counts.get(at) ?? 0) + 1)
      }
    }
  }
  const series: { at: number; running: number }[] = []
  for (let at = start; at < end; at += minute) {
    series.push({ at, running: counts.get(at) ?? 0 })
  }
  trendMinutes.value = series
}

// The series of the trend card: the window of the chosen range, downsampled
// to a point every few minutes, so the line stays light.
const trendSeries = computed(() => {
  const window = trendMinutes.value.slice(-trendRange.value)
  if (window.length === 0) {
    return []
  }
  const step = Math.max(1, Math.ceil(window.length / 120))
  const points: { at: number; running: number }[] = []
  for (let index = 0; index < window.length; index += step) {
    const slice = window.slice(index, index + step)
    // The mean of the window keeps a container that ran for a part of it
    // visible without turning a short peak into a flat ceiling.
    const mean = slice.reduce((total, item) => total + item.running, 0) / slice.length
    points.push({
      at: slice[0].at,
      running: Math.round(mean * 10) / 10,
    })
  }
  return points
})

// trendPaths draws the series as a soft curve of 600 by 108 units. The y of
// a point is the share of the containers that ran against the total.
const trendPaths = computed(() => {
  const points = trendSeries.value
  if (points.length < 2 || trendTotal.value === 0) {
    return null
  }
  const w = 600
  const h = 108
  const pad = 7
  // The ceiling keeps a little air above the line, so a deployment that
  // runs every container still reads as a curve and not as a full block.
  const ceiling = Math.max(trendTotal.value, 1) * 1.08
  const x = (index: number) =>
    pad + (index * (w - 2 * pad)) / (points.length - 1)
  const y = (value: number) =>
    h - pad - (value / ceiling) * (h - 2 * pad)
  let line = `M${x(0).toFixed(1)},${y(points[0].running).toFixed(1)}`
  for (let index = 1; index < points.length; index += 1) {
    const x0 = x(index - 1)
    const y0 = y(points[index - 1].running)
    const x1 = x(index)
    const y1 = y(points[index].running)
    const control = ((x0 + x1) / 2).toFixed(1)
    line += ` C${control},${y0.toFixed(1)} ${control},${y1.toFixed(1)} ${x1.toFixed(1)},${y1.toFixed(1)}`
  }
  const area = `${line} L${(w - pad).toFixed(1)},${(h - pad).toFixed(1)} L${pad.toFixed(1)},${(h - pad).toFixed(1)} Z`
  return { line, area, w, h }
})

// formatClock writes the hour of an alert the way the overview shows it.
function formatClock(value: string): string {
  const at = new Date(value)
  if (Number.isNaN(at.getTime())) {
    return '—'
  }
  return at.toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

onMounted(load)

// The overview follows the inventories: a short ticker reads the page again
// while it is open, at the pace of the agents.
let pollTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  pollTimer = setInterval(() => {
    void load()
  }, 20 * 1000)
})
onBeforeUnmount(() => {
  if (pollTimer) {
    clearInterval(pollTimer)
  }
})

// The readings of the hero: each one is a card with an icon, a number and,
// when a page of the platform explains it, the way to that page. A reading
// without a way is a number that only reads, like the services that live
// on the pages of their nodes.
type ReadingIcon = 'sites' | 'services' | 'containers' | 'health'

const readings = computed<{
  label: string
  value: string
  icon: ReadingIcon
  to?: string
}[]>(() => [
  { label: '站点总数', value: String(apps.value.length), icon: 'sites', to: '/applications' },
  { label: '服务总数', value: String(theServiceCount.value), icon: 'services' },
  { label: '容器总数', value: String(allContainers.value.length), icon: 'containers', to: '/nodes' },
  { label: '服务健康率', value: theHealthText.value, icon: 'health', to: '/alerts' },
])

// goTo pushes the browser to an address of the application.
function goTo(path: string): void {
  void router.push(path)
}
</script>

<template>
  <AppShell>
    <div v-if="loading" class="overview-loading">
      <span class="spinner" aria-hidden="true"></span>正在读取概览…
    </div>

    <div v-else-if="error" class="overview-error">
      <p>{{ error }}</p>
      <button type="button" class="text-button" @click="load">重试</button>
    </div>

    <div v-else class="overview">
      <!-- The hero of the platform -->
      <section class="hero">
        <div class="hero-copy">
          <h1>Docker 集群监控平台</h1>
          <p>集中管理多站点、多服务的 Docker 运行状态，保障业务稳定运行</p>
        </div>
        <div class="hero-visual">
          <DockerHeroArt />
        </div>
      </section>

      <!-- The four readings -->
      <section class="stats" aria-label="平台统计">
        <component
          :is="reading.to ? RouterLink : 'div'"
          v-for="reading in readings"
          :key="reading.label"
          :to="reading.to"
          class="stat-card"
          :class="{ plain: !reading.to }"
        >
          <span class="stat-icon" :class="reading.icon">
            <svg v-if="reading.icon === 'sites'" viewBox="0 0 24 24" aria-hidden="true">
              <rect x="4" y="4" width="16" height="7" rx="2.2" fill="currentColor" />
              <rect x="4" y="13" width="16" height="7" rx="2.2" fill="currentColor" opacity=".82" />
              <circle cx="7.6" cy="7.5" r=".9" fill="#fff" />
              <circle cx="16.4" cy="7.5" r=".9" fill="#fff" />
              <circle cx="7.6" cy="16.5" r=".9" fill="#fff" />
              <circle cx="16.4" cy="16.5" r=".9" fill="#fff" />
            </svg>
            <svg v-else-if="reading.icon === 'services'" viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
              <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
              <path d="M12 12.2v8.8" stroke="#fff" stroke-opacity=".35" stroke-width="1.2" />
            </svg>
            <svg v-else-if="reading.icon === 'containers'" viewBox="0 0 24 24" aria-hidden="true">
              <rect x="4" y="4" width="16" height="5" rx="1.7" fill="currentColor" />
              <rect x="4" y="9.5" width="16" height="5" rx="1.7" fill="currentColor" opacity=".84" />
              <rect x="4" y="15" width="16" height="5" rx="1.7" fill="currentColor" opacity=".68" />
              <circle cx="7.4" cy="6.5" r=".9" fill="#fff" />
              <circle cx="16.6" cy="6.5" r=".9" fill="#fff" />
              <circle cx="7.4" cy="12" r=".9" fill="#fff" />
              <circle cx="16.6" cy="12" r=".9" fill="#fff" />
              <circle cx="7.4" cy="17.5" r=".9" fill="#fff" />
              <circle cx="16.6" cy="17.5" r=".9" fill="#fff" />
            </svg>
            <svg v-else viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 5 5.8v5.4c0 4.2 2.9 7.6 7 9.8 4.1-2.2 7-5.6 7-9.8V5.8L12 3Z" fill="currentColor" />
              <path
                d="m8.8 11.8 2.2 2.2 4.2-4.4"
                fill="none"
                stroke="#fff"
                stroke-width="1.9"
                stroke-linecap="round"
                stroke-linejoin="round"
              />
            </svg>
          </span>
          <span class="stat-copy">
            <strong>{{ reading.value }}</strong>
            <small>{{ reading.label }}</small>
          </span>
          <span v-if="reading.to" class="stat-chevron" aria-hidden="true">
            <svg viewBox="0 0 24 24"><path d="m9 5 7 7-7 7" /></svg>
          </span>
        </component>
      </section>

      <div class="main-grid">
        <!-- The sites and the services they carry -->
        <section class="panel">
          <header class="panel-head">
            <h2 class="panel-title">站点服务总览</h2>
            <RouterLink class="panel-link" to="/applications">查看全部 <span aria-hidden="true">→</span></RouterLink>
          </header>

          <div v-if="sites.length === 0" class="empty-state">
            <span class="empty-icon" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <path d="M12 5v14M5 12h14" />
              </svg>
            </span>
            <div>
              <strong>还没有站点</strong>
              <p>在「站点管理」新建站点，编写 YAML 并部署后，这里会显示每个站点的服务与容器运行状态。</p>
            </div>
            <button type="button" class="primary-button" @click="goTo('/applications')">新建站点</button>
          </div>

          <div v-else class="site-list">
            <RouterLink
              v-for="site in sites"
              :key="site.id"
              :to="`/applications/${encodeURIComponent(site.id)}`"
              class="site-row"
            >
              <span class="site-icon" aria-hidden="true">
                <svg viewBox="0 0 24 24">
                  <rect x="6" y="3" width="9" height="18" rx="1.6" fill="currentColor" />
                  <rect x="14.6" y="9" width="4.4" height="12" rx="1.2" fill="currentColor" opacity=".78" />
                  <g fill="#fff" opacity=".88">
                    <rect x="8.3" y="6" width="1.7" height="1.7" rx=".4" />
                    <rect x="11" y="6" width="1.7" height="1.7" rx=".4" />
                    <rect x="8.3" y="9.4" width="1.7" height="1.7" rx=".4" />
                    <rect x="11" y="9.4" width="1.7" height="1.7" rx=".4" />
                    <rect x="8.3" y="12.8" width="1.7" height="1.7" rx=".4" />
                    <rect x="11" y="12.8" width="1.7" height="1.7" rx=".4" />
                  </g>
                </svg>
              </span>

              <div class="site-main">
                <div class="site-title">
                  <strong>{{ site.name }}</strong>
                  <span class="pill" :class="site.state">
                    <i aria-hidden="true"></i>{{ stateLabel(site) }}
                  </span>
                </div>
                <p class="site-meta">
                  <template v-if="site.containerCount > 0">
                    {{ site.serviceCount }} 个服务 · {{ site.containerCount }} 个容器
                  </template>
                  <template v-else>尚未上报容器</template>
                </p>
              </div>

              <span class="site-track" aria-hidden="true">
                <i v-for="(tone, index) in siteSegments(site)" :key="index" :class="tone"></i>
              </span>

              <span class="site-chevron" aria-hidden="true">
                <svg viewBox="0 0 24 24"><path d="m9 5 7 7-7 7" /></svg>
              </span>
            </RouterLink>
          </div>
        </section>

        <!-- The state of the whole deployment -->
        <section class="panel overall">
          <h2 class="overall-title">整体运行状态</h2>

          <div class="donut-row">
            <div class="donut">
              <svg viewBox="0 0 160 160" role="img" aria-label="服务的运行状态">
                <circle cx="80" cy="80" r="63" fill="none" stroke="#eef3fb" stroke-width="15" />
                <circle
                  v-for="(segment, index) in donutSegments"
                  :key="index"
                  cx="80"
                  cy="80"
                  r="63"
                  fill="none"
                  :stroke="segment.color"
                  stroke-width="15"
                  :stroke-dasharray="segment.dash"
                  :stroke-dashoffset="segment.offset"
                  transform="rotate(-90 80 80)"
                />
              </svg>
              <span class="donut-center">
                <strong>{{ theHealthText }}</strong>
                <small>健康率</small>
              </span>
            </div>

            <ul class="legend">
              <li v-for="part in donutParts" :key="part.key">
                <i :style="{ background: part.color }" aria-hidden="true"></i>
                <span>{{ part.label }}</span>
                <b>{{ part.count }}</b>
              </li>
            </ul>
          </div>

          <div class="trend">
            <header class="trend-head">
              <h3>近 {{ trendRangeLabel }}运行趋势</h3>
              <div class="ranges" role="group" aria-label="趋势的时间范围">
                <button
                  v-for="range in [60, 360, 1440]"
                  :key="range"
                  type="button"
                  :class="{ active: trendRange === range }"
                  :aria-pressed="trendRange === range"
                  @click="trendRange = range as 60 | 360 | 1440"
                >
                  {{ range === 60 ? '1h' : range === 360 ? '6h' : '24h' }}
                </button>
              </div>
            </header>

            <p v-if="trendLoading" class="trend-empty">正在读取趋势…</p>
            <p v-else-if="!trendPaths" class="trend-empty">
              这个窗口里没有采样数据。容器运行起来后，这里的曲线按分钟绘制。
            </p>
            <svg
              v-else
              class="trend-chart"
              :viewBox="`0 0 ${trendPaths.w} ${trendPaths.h}`"
              preserveAspectRatio="none"
              role="img"
              aria-label="运行中容器数随时间的趋势"
            >
              <defs>
                <linearGradient id="trend-fill" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0" stop-color="#4a95ff" stop-opacity=".28" />
                  <stop offset="1" stop-color="#4a95ff" stop-opacity=".02" />
                </linearGradient>
              </defs>
              <path :d="trendPaths.area" fill="url(#trend-fill)" />
              <path
                :d="trendPaths.line"
                fill="none"
                stroke="#2f7ef0"
                stroke-width="2.4"
                vector-effect="non-scaling-stroke"
                stroke-linejoin="round"
                stroke-linecap="round"
              />
            </svg>
          </div>
        </section>
      </div>

      <!-- The newest alerts -->
      <section class="panel alerts-panel">
        <header class="panel-head alerts-head">
          <h2 class="panel-title bell">
            <svg class="bell-icon" viewBox="0 0 24 24" aria-hidden="true">
              <path d="M18 8a6 6 0 0 0-12 0c0 6-3 8-3 8h18s-3-2-3-8M10 21h4" />
            </svg>
            最近告警
          </h2>
          <RouterLink class="panel-link" to="/alerts">查看全部 <span aria-hidden="true">→</span></RouterLink>
        </header>

        <p v-if="theNewestAlerts.length === 0" class="alerts-empty">当前没有告警事件。</p>

        <RouterLink
          v-for="event in theNewestAlerts"
          :key="event.id"
          to="/alerts"
          class="alert-row"
        >
          <i class="alert-dot" :class="{ resolved: event.state === 'resolved' }" aria-hidden="true"></i>
          <span class="alert-text">{{ event.summary }}</span>
          <time class="alert-time" :datetime="event.started_at">{{ formatClock(event.started_at) }}</time>
        </RouterLink>
      </section>
    </div>
  </AppShell>
</template>

<style scoped>
.overview {
  display: grid;
  gap: 22px;
}

.overview-loading,
.overview-error {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  min-height: 220px;
  color: #7b8aa8;
  font-size: 14px;
}

.overview-error {
  flex-direction: column;
  color: #d92d20;
}

.spinner {
  width: 16px;
  height: 16px;
  border: 2px solid #e2ebf9;
  border-top-color: #0b63f6;
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

.text-button {
  border: 0;
  background: none;
  color: #0b63f6;
  font-weight: 600;
  cursor: pointer;
}

/* The hero ------------------------------------------------------------- */
.hero {
  position: relative;
  display: flex;
  align-items: center;
  min-height: 218px;
  padding: 36px 44px;
  overflow: hidden;
  border: 1px solid #e3edfd;
  border-radius: 20px;
  background:
    linear-gradient(112deg, rgb(255 255 255 / 20%) 0%, rgb(255 255 255 / 5%) 36%, rgb(255 255 255 / 0%) 62%),
    linear-gradient(104deg, #dfecff 0%, #cfe2fd 44%, #bad3fa 100%);
  box-shadow: 0 18px 40px -34px rgb(23 66 140 / 45%);
}

/* The diagonal band of light of the design. */
.hero::before {
  content: "";
  position: absolute;
  top: -40%;
  right: 12%;
  width: 46%;
  height: 190%;
  background: linear-gradient(90deg, rgb(255 255 255 / 0%), rgb(255 255 255 / 38%), rgb(255 255 255 / 0%));
  transform: rotate(24deg);
  pointer-events: none;
}

.hero-copy {
  position: relative;
  z-index: 1;
  max-width: 46%;
}

.hero-copy h1 {
  color: #132a56;
  font-size: 40px;
  font-weight: 800;
  letter-spacing: -0.02em;
  line-height: 1.18;
}

.hero-copy p {
  margin-top: 14px;
  color: #5a7398;
  font-size: 16px;
  letter-spacing: 0.01em;
}

.hero-visual {
  position: absolute;
  top: 0;
  right: -1%;
  bottom: 0;
  width: 56%;
  min-width: 700px;
  pointer-events: none;
  mask-image: linear-gradient(90deg, transparent 0%, #000 22%);
  -webkit-mask-image: linear-gradient(90deg, transparent 0%, #000 22%);
}

/* The readings --------------------------------------------------------- */
.stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 20px;
}

.stat-card {
  display: flex;
  align-items: center;
  gap: 17px;
  min-width: 0;
  padding: 24px 22px;
  border: 1px solid #e9eff9;
  border-radius: 16px;
  background: #fff;
  color: inherit;
  text-decoration: none;
  box-shadow: 0 14px 30px -32px rgb(20 52 110 / 50%);
  transition: transform 160ms ease, box-shadow 160ms ease, border-color 160ms ease;
}

.stat-card:hover {
  transform: translateY(-1px);
  border-color: #d8e6fb;
  box-shadow: 0 18px 34px -28px rgb(20 52 110 / 45%);
  text-decoration: none;
}

/* A reading without a page of its own only reads: it does not lift and it
   does not offer a hand the page cannot follow. */
.stat-card.plain {
  cursor: default;
}

.stat-card.plain:hover {
  transform: none;
  border-color: #e9eff9;
  box-shadow: 0 14px 30px -32px rgb(20 52 110 / 50%);
}

.stat-icon {
  width: 64px;
  height: 64px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.stat-icon svg {
  width: 30px;
  height: 30px;
}

.stat-icon.health {
  background: #e5f9ef;
  color: #0bbf7a;
}

.stat-copy {
  display: grid;
  gap: 1px;
  min-width: 0;
}

.stat-copy strong {
  color: #132a56;
  font-size: 32px;
  font-weight: 800;
  line-height: 1.16;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
}

.stat-copy small {
  color: #6e80a0;
  font-size: 14px;
  white-space: nowrap;
}

.stat-chevron {
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  flex: none;
  margin-left: auto;
  border-radius: 50%;
  background: #eff5ff;
  color: #2f6fe4;
}

.stat-chevron svg {
  width: 14px;
  height: 14px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

/* The two panels ------------------------------------------------------- */
.main-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.62fr) minmax(0, 1fr);
  gap: 22px;
  align-items: stretch;
}

.panel {
  min-width: 0;
  border: 1px solid #e9eff9;
  border-radius: 18px;
  background: #fff;
  box-shadow: 0 18px 38px -36px rgb(20 52 110 / 50%);
}

.panel-head {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 22px 24px;
  border-bottom: 1px solid #eef3fb;
}

.panel-title {
  display: flex;
  align-items: center;
  gap: 10px;
  color: #16264a;
  font-size: 19px;
  font-weight: 700;
  letter-spacing: -0.01em;
}

.panel-title::before {
  content: "";
  width: 4px;
  height: 18px;
  border-radius: 2px;
  background: #2f6fe4;
}

.panel-title.bell::before {
  display: none;
}

.bell-icon {
  width: 21px;
  height: 21px;
  fill: #2f6fe4;
  stroke: none;
}

.panel-link {
  margin-left: auto;
  color: #2f6fe4;
  font-size: 14px;
  font-weight: 600;
  text-decoration: none;
  white-space: nowrap;
}

.panel-link:hover {
  text-decoration: none;
  color: #0b55d4;
}

/* The sites ------------------------------------------------------------ */
.site-list {
  display: grid;
  gap: 12px;
  padding: 18px 20px 22px;
}

.site-row {
  display: flex;
  align-items: center;
  gap: 15px;
  min-height: 100px;
  padding: 18px;
  border: 1px solid #edf2fb;
  border-radius: 14px;
  background: #fff;
  color: inherit;
  text-decoration: none;
  transition: border-color 150ms ease, background 150ms ease, box-shadow 150ms ease;
}

.site-row:hover {
  border-color: #cfe0fa;
  background: #fbfdff;
  box-shadow: 0 14px 28px -30px rgb(20 52 110 / 55%);
  text-decoration: none;
}

.site-icon {
  width: 54px;
  height: 54px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.site-icon svg {
  width: 27px;
  height: 27px;
}

.site-main {
  min-width: 0;
  flex: 1;
}

.site-title {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.site-title strong {
  color: #16264a;
  font-size: 16px;
  font-weight: 700;
}

.site-meta {
  margin-top: 3px;
  color: #6b7d9c;
  font-size: 13px;
  font-weight: 500;
  font-variant-numeric: tabular-nums;
}

.site-track {
  display: flex;
  gap: 6px;
  flex: none;
}

.site-track i {
  display: block;
  width: 36px;
  height: 9px;
  border-radius: 999px;
  background: #e6edf9;
}

.site-track i.ok {
  background: #12b76a;
}

.site-track i.warn {
  background: #f79009;
}

.site-track i.maint {
  background: #c9d7ee;
}

.site-chevron {
  display: grid;
  place-items: center;
  flex: none;
  margin-left: 2px;
  color: #7b8aa8;
}

.site-chevron svg {
  width: 18px;
  height: 18px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 11px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 600;
  line-height: 1.5;
  white-space: nowrap;
}

.pill i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentcolor;
}

.pill.healthy {
  background: #e7f9f1;
  color: #0a9b6a;
}

.pill.partial {
  background: #fff4e6;
  color: #e07c00;
}

.pill.offline {
  background: #fff4e6;
  color: #c46a00;
}

.pill.undeployed {
  background: #f0f3f9;
  color: #7b8aa8;
}

.empty-state {
  display: flex;
  align-items: center;
  gap: 16px;
  margin: 20px;
  padding: 20px;
  border: 1px dashed #cfe0fa;
  border-radius: 14px;
  background: #f8fbff;
}

.empty-icon {
  width: 44px;
  height: 44px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e8f1ff;
  color: #2f6fe4;
}

.empty-icon svg {
  width: 20px;
  height: 20px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.8;
  stroke-linecap: round;
}

.empty-state strong {
  color: #16264a;
  font-size: 14px;
}

.empty-state p {
  margin-top: 3px;
  color: #7b8aa8;
  font-size: 12px;
  line-height: 1.7;
}

.primary-button {
  margin-left: auto;
  height: 36px;
  padding: 0 16px;
  flex: none;
  border: 0;
  border-radius: 9px;
  background: #0b63f6;
  color: #fff;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}

/* The state of the deployment ------------------------------------------ */
.overall {
  display: flex;
  flex-direction: column;
  padding: 26px 24px 26px;
}

.overall-title {
  color: #16264a;
  font-size: 19px;
  font-weight: 700;
  letter-spacing: -0.01em;
}

.donut-row {
  display: flex;
  align-items: center;
  gap: 22px;
  padding: 24px 2px 22px;
}

.donut {
  position: relative;
  width: 196px;
  height: 196px;
  flex: none;
}

.donut svg {
  display: block;
  width: 100%;
  height: 100%;
}

.donut-center {
  position: absolute;
  inset: 0;
  display: grid;
  place-content: center;
  gap: 2px;
  text-align: center;
}

.donut-center strong {
  color: #16264a;
  font-size: 28px;
  font-weight: 700;
  line-height: 1.1;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
}

.donut-center small {
  color: #8595b2;
  font-size: 13px;
}

.legend {
  display: grid;
  gap: 21px;
  flex: 1;
  min-width: 0;
  margin: 0;
  padding: 0;
  list-style: none;
}

.legend li {
  display: flex;
  align-items: center;
  gap: 10px;
  color: #16264a;
  font-size: 15px;
  font-weight: 600;
}

.legend i {
  width: 10px;
  height: 10px;
  flex: none;
  border-radius: 50%;
}

.legend b {
  margin-left: auto;
  color: #16264a;
  font-size: 14px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
}

.trend {
  margin-top: auto;
  padding: 18px 18px 15px;
  border: 1px solid #eef3fb;
  border-radius: 14px;
  background: #fff;
}

.trend-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.trend-head h3 {
  color: #16264a;
  font-size: 14px;
  font-weight: 700;
  white-space: nowrap;
}

.ranges {
  display: flex;
  gap: 5px;
}

.ranges button {
  height: 26px;
  padding: 0 10px;
  border: 0;
  border-radius: 999px;
  background: #f2f6fd;
  color: #7b8aa8;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: background 140ms ease, color 140ms ease;
}

.ranges button:hover {
  background: #eaf2ff;
  color: #2f6fe4;
}

.ranges button.active {
  background: #e2edff;
  color: #2f6fe4;
  font-weight: 700;
}

.trend-chart {
  display: block;
  width: 100%;
  height: 96px;
  margin-top: 10px;
}

.trend-empty {
  display: grid;
  place-items: center;
  min-height: 100px;
  margin-top: 8px;
  color: #94a3bd;
  font-size: 11.5px;
  line-height: 1.7;
  text-align: center;
}

/* The alerts ----------------------------------------------------------- */
.alerts-panel {
  overflow: hidden;
}

.alerts-head {
  padding: 19px 24px;
}

.alerts-empty {
  padding: 26px 24px;
  color: #7b8aa8;
  font-size: 13px;
  text-align: center;
}

.alert-row {
  display: flex;
  align-items: center;
  gap: 12px;
  min-height: 58px;
  padding: 16px 24px;
  border-top: 1px solid #f1f5fc;
  color: inherit;
  text-decoration: none;
  transition: background 140ms ease;
}

.alert-row:hover {
  background: #fbfdff;
  text-decoration: none;
}

.alert-dot {
  width: 8px;
  height: 8px;
  flex: none;
  border-radius: 50%;
  background: #f04438;
}

.alert-dot.resolved {
  background: #c9d7ee;
}

.alert-text {
  color: #34446b;
  font-size: 14px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.alert-time {
  margin-left: auto;
  color: #8595b2;
  font-size: 12.5px;
  font-variant-numeric: tabular-nums;
}

/* Responsive ----------------------------------------------------------- */
@media (max-width: 1440px) {
  .hero-copy h1 {
    font-size: 34px;
  }

  .hero {
    min-height: 188px;
    padding: 30px 36px;
  }

  .hero-visual {
    width: 60%;
  }

  .site-track i {
    width: 26px;
  }
}

@media (max-width: 1240px) {
  .hero-copy {
    max-width: 56%;
  }

  .hero-visual {
    width: 58%;
    opacity: 0.9;
  }

  .stat-copy strong {
    font-size: 26px;
  }

  .stat-icon {
    width: 50px;
    height: 50px;
  }

  .main-grid {
    grid-template-columns: minmax(0, 1fr);
  }

  .donut-row {
    justify-content: flex-start;
  }

  .legend {
    max-width: 260px;
  }
}

@media (max-width: 980px) {
  .stats {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .hero-copy {
    max-width: 100%;
  }

  .hero-copy h1 {
    font-size: 30px;
  }

  .hero-visual {
    width: 72%;
    opacity: 0.42;
    mask-image: linear-gradient(90deg, transparent 0%, #000 55%);
    -webkit-mask-image: linear-gradient(90deg, transparent 0%, #000 55%);
  }
}

@media (max-width: 640px) {
  .hero {
    padding: 24px 22px;
    border-radius: 16px;
  }

  .hero-copy h1 {
    font-size: 26px;
  }

  .hero-copy p {
    font-size: 13px;
  }

  .stats {
    grid-template-columns: minmax(0, 1fr);
  }

  .stat-card {
    padding: 14px 16px;
  }

  .site-track {
    display: none;
  }

  .panel-head,
  .overall {
    padding: 16px;
  }

  .site-list {
    padding: 12px;
  }

  .donut-row {
    flex-direction: column;
    align-items: flex-start;
  }

  .legend {
    width: 100%;
    max-width: none;
  }

  .empty-state {
    flex-wrap: wrap;
  }

  .empty-state .primary-button {
    width: 100%;
    margin-left: 0;
  }
}
</style>
