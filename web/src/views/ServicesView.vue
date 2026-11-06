<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import AppShell from '../components/AppShell.vue'
import { listApplications } from '../api/deployments'
import { listContainers, type ContainerSummary } from '../api/metrics'
import { listNodes } from '../api/nodes'
import { describeError, type AppSummary, type NodeSummary } from '../api/types'

// The services page lists the services of every site, with the state of
// their containers. A service is a name inside the namespace of a site.

const apps = ref<AppSummary[]>([])
const nodes = ref<NodeSummary[]>([])
const containersByNode = ref<Record<string, ContainerSummary[]>>({})
const loading = ref(true)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [appList, nodeList] = await Promise.all([listApplications(), listNodes()])
    apps.value = appList
    nodes.value = nodeList
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
}

function containersOf(nodeID: string): ContainerSummary[] {
  return containersByNode.value[nodeID] ?? []
}

type ServiceState = 'running' | 'degraded' | 'maintenance'

interface ServiceItem {
  key: string
  name: string
  namespace: string
  siteID: string
  siteName: string
  containers: number
  running: number
  state: ServiceState
}

const services = computed<ServiceItem[]>(() => {
  const appsByNamespace = new Map(apps.value.map((app) => [app.namespace, app]))
  const records = new Map<string, ServiceItem>()
  for (const node of nodes.value) {
    for (const item of containersOf(node.id)) {
      if (!item.namespace || !item.service) {
        continue
      }
      const app = appsByNamespace.get(item.namespace)
      const key = `${item.namespace}/${item.service}`
      let record = records.get(key)
      if (!record) {
        record = {
          key,
          name: item.service,
          namespace: item.namespace,
          siteID: app?.id ?? '',
          siteName: app?.name ?? '未归属站点',
          containers: 0,
          running: 0,
          state: 'running',
        }
        records.set(key, record)
      }
      record.containers += 1
      if (item.state === 'running') {
        record.running += 1
      }
    }
  }
  for (const record of records.values()) {
    if (record.running === 0) {
      record.state = 'maintenance'
    } else if (record.running < record.containers) {
      record.state = 'degraded'
    } else {
      record.state = 'running'
    }
  }
  // The order of the sites is the order of the application list; the
  // services without a site come last, and every group keeps its services
  // sorted by name.
  const order = new Map(apps.value.map((app, index) => [app.id, index]))
  return [...records.values()].sort((left, right) => {
    const leftIndex = left.siteID ? (order.get(left.siteID) ?? 999) : 999
    const rightIndex = right.siteID ? (order.get(right.siteID) ?? 999) : 999
    if (leftIndex !== rightIndex) {
      return leftIndex - rightIndex
    }
    if (!left.siteID && left.namespace !== right.namespace) {
      return left.namespace.localeCompare(right.namespace, 'zh-CN')
    }
    return left.name.localeCompare(right.name, 'zh-CN')
  })
})

// The rows are grouped by site, so an administrator reads the services of
// one site together. The services whose namespace belongs to no site stay
// in a group of their own.
const groups = computed(() => {
  const map = new Map<
    string,
    { key: string; siteID: string; siteName: string; items: ServiceItem[] }
  >()
  for (const item of services.value) {
    const key = item.siteID || '__unmanaged'
    let group = map.get(key)
    if (!group) {
      group = { key, siteID: item.siteID, siteName: item.siteName, items: [] }
      map.set(key, group)
    }
    group.items.push(item)
  }
  return [...map.values()]
})

const theRunning = computed(
  () => services.value.filter((item) => item.state === 'running').length,
)
const theDegraded = computed(
  () => services.value.filter((item) => item.state === 'degraded').length,
)
const theStopped = computed(
  () => services.value.filter((item) => item.state === 'maintenance').length,
)

function stateLabel(state: ServiceState): string {
  if (state === 'running') return '运行中'
  if (state === 'degraded') return '部分异常'
  return '维护中'
}

onMounted(load)
</script>

<template>
  <AppShell>
    <div class="services">
      <header class="page-head">
        <div>
          <h1>服务管理</h1>
          <p>每个站点的服务及其容器运行状态。服务来自容器的命名空间与服务标签。</p>
        </div>
        <button type="button" class="ghost-button" :disabled="loading" @click="load">
          {{ loading ? '读取中…' : '刷新' }}
        </button>
      </header>

      <div v-if="loading" class="state">正在读取服务…</div>

      <div v-else-if="error" class="state error">
        <p>{{ error }}</p>
        <button type="button" class="text-button" @click="load">重试</button>
      </div>

      <template v-else>
        <section class="summary" aria-label="服务统计">
          <div>
            <span>服务总数</span>
            <strong>{{ services.length }}</strong>
          </div>
          <div>
            <span>运行中</span>
            <strong class="ok">{{ theRunning }}</strong>
          </div>
          <div>
            <span>部分异常</span>
            <strong class="warn">{{ theDegraded }}</strong>
          </div>
          <div>
            <span>维护中</span>
            <strong>{{ theStopped }}</strong>
          </div>
        </section>

        <section v-if="groups.length === 0" class="panel empty-state">
          <strong>还没有服务</strong>
          <p>站点部署后，容器上报的命名空间与服务会让它们出现在这里。</p>
        </section>

        <section v-for="group in groups" :key="group.key" class="panel">
          <header class="panel-head">
            <h2>{{ group.siteName }}</h2>
            <span v-if="!group.siteID" class="unmanaged">未归属站点</span>
            <span class="panel-count">{{ group.items.length }} 个服务</span>
            <RouterLink
              v-if="group.siteID"
              class="panel-link"
              :to="`/applications/${encodeURIComponent(group.siteID)}`"
            >
              查看站点 <span aria-hidden="true">→</span>
            </RouterLink>
          </header>

          <div class="service-list">
            <div v-for="item in group.items" :key="item.key" class="service-row">
              <span class="service-icon" aria-hidden="true">
                <svg viewBox="0 0 24 24">
                  <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
                  <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
                </svg>
              </span>
              <div class="service-main">
                <strong>{{ item.name }}</strong>
                <small>{{ item.namespace }} · {{ item.running }} / {{ item.containers }} 个容器运行中</small>
              </div>
              <span class="pill" :class="item.state">
                <i aria-hidden="true"></i>{{ stateLabel(item.state) }}
              </span>
            </div>
          </div>
        </section>
      </template>
    </div>
  </AppShell>
</template>

<style scoped>
.services {
  display: grid;
  gap: 20px;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 18px;
}

.page-head h1 {
  color: #132a56;
  font-size: 28px;
  font-weight: 700;
  letter-spacing: -0.02em;
}

.page-head p {
  margin-top: 6px;
  color: #7b8aa8;
  font-size: 13px;
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

.summary {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 18px;
}

.summary > div {
  padding: 16px 20px;
  border: 1px solid #e9eff9;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 14px 30px -32px rgb(20 52 110 / 50%);
}

.summary span {
  color: #7b8aa8;
  font-size: 12.5px;
}

.summary strong {
  display: block;
  margin-top: 2px;
  color: #132a56;
  font-size: 28px;
  font-weight: 700;
  line-height: 1.2;
  font-variant-numeric: tabular-nums;
}

.summary strong.ok {
  color: #0a9b6a;
}

.summary strong.warn {
  color: #e07c00;
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
  gap: 10px;
  padding: 17px 22px;
  border-bottom: 1px solid #eef3fb;
}

.panel-head h2 {
  display: flex;
  align-items: center;
  gap: 9px;
  color: #16264a;
  font-size: 16px;
  font-weight: 700;
}

.panel-head h2::before {
  content: "";
  width: 4px;
  height: 16px;
  border-radius: 2px;
  background: #2f6fe4;
}

.panel-count {
  color: #8595b2;
  font-size: 12px;
}

.unmanaged {
  padding: 1px 8px;
  border-radius: 999px;
  background: #f0f3f9;
  color: #7b8aa8;
  font-size: 11px;
  font-weight: 600;
}

.panel-link {
  margin-left: auto;
  color: #2f6fe4;
  font-size: 13px;
  font-weight: 600;
  text-decoration: none;
}

.service-list {
  display: grid;
  gap: 10px;
  padding: 14px 18px 18px;
}

.service-row {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 12px 15px;
  border: 1px solid #edf2fb;
  border-radius: 13px;
  color: inherit;
  text-decoration: none;
  transition: border-color 150ms ease, background 150ms ease;
}

.service-row:hover {
  border-color: #cfe0fa;
  background: #fbfdff;
  text-decoration: none;
}

.service-icon {
  width: 40px;
  height: 40px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.service-icon svg {
  width: 21px;
  height: 21px;
}

.service-main {
  display: grid;
  gap: 1px;
  min-width: 0;
}

.service-main strong {
  color: #16264a;
  font-size: 14.5px;
  font-weight: 600;
}

.service-main small {
  color: #7b8aa8;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-left: auto;
  padding: 2px 9px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
  white-space: nowrap;
}

.pill i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentcolor;
}

.pill.running {
  background: #e7f9f1;
  color: #0a9b6a;
}

.pill.degraded {
  background: #fff4e6;
  color: #e07c00;
}

.pill.maintenance {
  background: #f0f3f9;
  color: #7b8aa8;
}

.empty-state {
  display: grid;
  gap: 4px;
  justify-items: center;
  padding: 52px 20px;
  text-align: center;
}

.empty-state strong {
  color: #16264a;
  font-size: 15px;
}

.empty-state p {
  color: #7b8aa8;
  font-size: 12.5px;
}

@media (max-width: 900px) {
  .summary {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 640px) {
  .page-head h1 {
    font-size: 24px;
  }

  .summary {
    grid-template-columns: minmax(0, 1fr);
  }

  .pill {
    display: none;
  }
}
</style>
