<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import {
  getNode,
  listNodeNetworks,
  listNodeServices,
  roleLabel,
} from '../api/nodes'
import { listContainers, type ContainerSummary } from '../api/metrics'
import {
  describeError,
  formatTimestamp,
  type NodeNetwork,
  type NodeService,
  type NodeSummary,
  type PortMapping,
} from '../api/types'

// The page of a node follows the environment page of Portainer: a compact
// header, the readings of the node and one list for every kind of object
// the node holds. A container opens its own page, so the list stays a
// list.
type Tab = 'containers' | 'services' | 'networks'

const route = useRoute()
const router = useRouter()

const node = ref<NodeSummary | null>(null)
const containers = ref<ContainerSummary[]>([])
const services = ref<NodeService[]>([])
const networks = ref<NodeNetwork[]>([])
const loading = ref(true)
const initialLoaded = ref(false)
const refreshing = ref(false)
const error = ref('')
const containersError = ref('')
const servicesError = ref('')
const servicesFallback = ref(false)
const networksError = ref('')
const tab = ref<Tab>('containers')
const filter = ref('')

const nodeID = computed(() => String(route.params.id ?? ''))

// load reads the node and the three lists of its objects. A list that fails
// for one node does not sink the page: the page shows the lists it knows
// and the reason of the missing one.
async function load(silent = false) {
  if (!initialLoaded.value) {
    loading.value = true
  } else if (!silent) {
    refreshing.value = true
  }
  error.value = ''
  containersError.value = ''
  servicesError.value = ''
  networksError.value = ''

  try {
    node.value = await getNode(nodeID.value)
  } catch (failure) {
    node.value = null
    error.value = describeError(failure)
    loading.value = false
    refreshing.value = false
    initialLoaded.value = true
    return
  }

  const [containerResult, serviceResult, networkResult] = await Promise.all([
    listContainers(nodeID.value).then(
      (items) => ({ items, error: '' }),
      (failure) => ({ items: [] as ContainerSummary[], error: describeError(failure) }),
    ),
    listNodeServices(nodeID.value).then(
      (items) => ({ items, error: '' }),
      (failure) => ({ items: [] as NodeService[], error: describeError(failure) }),
    ),
    listNodeNetworks(nodeID.value).then(
      (items) => ({ items, error: '' }),
      (failure) => ({ items: [] as NodeNetwork[], error: describeError(failure) }),
    ),
  ])
  containers.value = containerResult.items
  networks.value = networkResult.items
  containersError.value = containerResult.error
  networksError.value = networkResult.error
  services.value = serviceResult.items
  servicesFallback.value = false
  servicesError.value = serviceResult.error
  // A control端 that does not serve the service list yet still shows the
  // services the Compose labels of the containers name.
  if (serviceResult.error !== '' && containerResult.items.length > 0) {
    services.value = deriveServices(containerResult.items)
    servicesFallback.value = true
    servicesError.value = ''
  }
  loading.value = false
  refreshing.value = false
  initialLoaded.value = true
}

// theRunning counts the running containers.
const theRunning = computed(
  () => containers.value.filter((item) => item.state === 'running').length,
)

// theRunningServices counts the services that reached their target.
const theRunningServices = computed(
  () => services.value.filter((item) => item.running >= item.desired && item.desired > 0).length,
)

// theFilteredContainers applies the search of the container list.
const theFilteredContainers = computed(() => {
  const needle = filter.value.trim().toLowerCase()
  if (needle === '') {
    return containers.value
  }
  return containers.value.filter((item) =>
    [item.name, item.image, item.namespace, item.service, item.ip ?? '']
      .join(' ')
      .toLowerCase()
      .includes(needle),
  )
})

// stateLabel and stateTone name the state of a container.
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

// serviceTone names the state of a service.
function serviceTone(service: NodeService): 'ok' | 'bad' | 'warn' | 'off' {
  if (service.update_state === 'updating' || service.update_state === 'paused') {
    return 'warn'
  }
  if (service.desired === 0) {
    return 'off'
  }
  if (service.source === 'compose' && service.running < service.desired) {
    return service.running === 0 ? 'bad' : 'warn'
  }
  return service.running >= service.desired ? 'ok' : 'bad'
}

function serviceStateLabel(service: NodeService): string {
  switch (service.update_state) {
    case 'updating':
      return '更新中'
    case 'paused':
      return '更新暂停'
    case 'rollback_completed':
      return '已回滚'
    case 'rollback_started':
      return '回滚中'
    default:
      break
  }
  if (service.desired === 0) return '已停止'
  if (service.source === 'compose') {
    if (service.running === 0) return '未运行'
    if (service.running < service.desired) return '部分运行'
    return '运行中'
  }
  return service.running >= service.desired ? '运行中' : '副本不足'
}

// portText writes one port the way the list shows it.
function portText(port: PortMapping): string {
  const published = port.public_port ? `${port.public_port}:` : ''
  return `${published}${port.private_port}/${port.type || 'tcp'}`
}

function portsText(ports: PortMapping[] | undefined): string {
  if (!ports || ports.length === 0) {
    return '—'
  }
  return ports.map(portText).join(', ')
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
    hour12: false,
  })
}

// deriveServices builds the services of the Compose containers when the
// control端 does not serve the service list yet.
function deriveServices(items: ContainerSummary[]): NodeService[] {
  const groups = new Map<string, ContainerSummary[]>()
  for (const item of items) {
    if (item.service === '' || item.namespace === '') {
      continue
    }
    const key = `${item.namespace}/${item.service}`
    const group = groups.get(key) ?? []
    group.push(item)
    groups.set(key, group)
  }
  const services: NodeService[] = []
  for (const [key, group] of groups) {
    const first = group[0]
    const ports = new Map<string, NodeService['ports'][number]>()
    let running = 0
    for (const item of group) {
      if (item.state === 'running') {
        running += 1
      }
      for (const port of item.ports ?? []) {
        ports.set(`${port.private_port}-${port.public_port ?? 0}-${port.type}`, port)
      }
    }
    services.push({
      id: key,
      name: first.service,
      stack: first.namespace,
      image: first.image,
      mode: 'compose',
      desired: group.length,
      running,
      update_state: '',
      containers: group.length,
      ports: [...ports.values()],
      updated_at: '',
      source: 'compose',
    })
  }
  return services
}

// openContainer opens the page of one container.
function openContainer(containerID: string) {
  void router.push(`/nodes/${encodeURIComponent(nodeID.value)}/containers/${encodeURIComponent(containerID)}`)
}

// showContainersOf opens the container list filtered by a service.
function showContainersOf(service: NodeService) {
  filter.value = service.stack
  tab.value = 'containers'
}

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

// A change of the node is another page: read it from the beginning.
watch(nodeID, async (current, previous) => {
  if (current === previous) {
    return
  }
  initialLoaded.value = false
  filter.value = ''
  tab.value = 'containers'
  await load()
})
</script>

<template>
  <AppShell>
    <p v-if="loading" class="state">正在读取节点…</p>

    <div v-else-if="error || !node" class="state error">
      <p>{{ error || '未找到该节点。' }}</p>
      <RouterLink class="text-link" to="/nodes">返回节点列表</RouterLink>
    </div>

    <div v-else class="node-page">
      <nav class="crumb" aria-label="面包屑">
        <RouterLink to="/nodes">节点管理</RouterLink>
        <span aria-hidden="true">/</span>
        <span>{{ node.hostname }}</span>
      </nav>

      <header class="node-head">
        <div class="node-title">
          <span class="node-mark" aria-hidden="true">
            <svg viewBox="0 0 24 24">
              <path d="M4 4h16v6H4V4Zm0 10h16v6H4v-6ZM7.5 7h.01M7.5 17h.01" />
            </svg>
          </span>
          <div>
            <div class="title-line">
              <h1>{{ node.hostname }}</h1>
              <span class="status" :class="node.revoked ? 'off' : node.connected ? 'ok' : 'warn'">
                <i aria-hidden="true"></i>
                {{ node.revoked ? '已撤销' : node.connected ? '已连接' : '连接中断' }}
              </span>
            </div>
            <p class="node-meta">
              <span>{{ roleLabel(node.role) }}</span>
              <span>{{ node.docker_version ? `Docker ${node.docker_version}` : 'Docker 版本未上报' }}</span>
              <span>{{ node.os_type || '?' }} / {{ node.os_arch || '?' }}</span>
              <span v-if="node.agent_version">Agent {{ node.agent_version }}</span>
              <span>最近上报 {{ formatTimestamp(node.last_seen) }}</span>
            </p>
          </div>
        </div>
        <button type="button" class="ghost-button" :disabled="refreshing" @click="load()">
          {{ refreshing ? '刷新中…' : '刷新' }}
        </button>
      </header>

      <p v-if="node.revoked" class="banner error">
        该节点的接入凭据已撤销。节点记录与历史数据保留，但节点不再连接、清单不再更新。
      </p>
      <p v-else-if="!node.connected" class="banner warn">
        连接中断。下面显示的是 {{ formatTimestamp(node.last_seen) }} 的最后一次上报，不代表机器宕机。
      </p>

      <section class="readings" aria-label="节点概况">
        <div class="reading">
          <span>容器</span>
          <strong>{{ containers.length }}</strong>
          <small>{{ theRunning }} 个运行中</small>
        </div>
        <div class="reading">
          <span>服务</span>
          <strong>{{ services.length }}</strong>
          <small>{{ theRunningServices }} 个达到目标</small>
        </div>
        <div class="reading">
          <span>网络</span>
          <strong>{{ networks.length }}</strong>
          <small>Docker 网络</small>
        </div>
        <div class="reading">
          <span>最近上报</span>
          <strong class="reading-time">{{ formatDate(node.last_seen) }}</strong>
          <small>{{ node.connected ? '每 15 秒同步' : '历史清单' }}</small>
        </div>
      </section>

      <section class="panel">
        <nav class="tabs" aria-label="节点对象">
          <button
            type="button"
            :class="{ active: tab === 'containers' }"
            :aria-selected="tab === 'containers'"
            @click="tab = 'containers'"
          >
            容器 <span>{{ containers.length }}</span>
          </button>
          <button
            type="button"
            :class="{ active: tab === 'services' }"
            :aria-selected="tab === 'services'"
            @click="tab = 'services'"
          >
            服务 <span>{{ services.length }}</span>
          </button>
          <button
            type="button"
            :class="{ active: tab === 'networks' }"
            :aria-selected="tab === 'networks'"
            @click="tab = 'networks'"
          >
            网络 <span>{{ networks.length }}</span>
          </button>
        </nav>

        <!-- Containers -->
        <div v-if="tab === 'containers'" class="tab-body">
          <div class="list-toolbar">
            <label class="search">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <circle cx="11" cy="11" r="7" />
                <path d="m16 16 4 4" />
              </svg>
              <input v-model="filter" type="search" placeholder="按名称、镜像、堆栈或 IP 搜索容器" />
            </label>
            <span class="toolbar-note">{{ theFilteredContainers.length }} / {{ containers.length }} 个容器</span>
          </div>

          <p v-if="containersError" class="banner error">{{ containersError }}</p>

          <div v-else-if="containers.length === 0" class="empty">
            <p>这个节点还没有上报任何容器。</p>
          </div>

          <div v-else class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">名称</th>
                  <th scope="col">状态</th>
                  <th scope="col">堆栈 / 命名空间</th>
                  <th scope="col">镜像</th>
                  <th scope="col">创建</th>
                  <th scope="col">IP</th>
                  <th scope="col">端口</th>
                  <th scope="col" class="actions-col"><span class="sr-only">操作</span></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="item in theFilteredContainers"
                  :key="item.id"
                  class="container-row"
                  @click="openContainer(item.id)"
                >
                  <td class="name-cell">
                    <RouterLink
                      class="container-link"
                      :to="`/nodes/${encodeURIComponent(node.id)}/containers/${encodeURIComponent(item.id)}`"
                      @click.stop
                    >
                      {{ item.name }}
                    </RouterLink>
                    <small>{{ item.service || '未归属服务' }}</small>
                  </td>
                  <td>
                    <span class="pill" :class="stateTone(item.state)">
                      <i aria-hidden="true"></i>{{ stateLabel(item.state) }}
                    </span>
                    <small v-if="item.health" class="health">{{ item.health === 'healthy' ? '健康' : item.health }}</small>
                  </td>
                  <td>
                    <code v-if="item.namespace" class="stack">{{ item.namespace }}</code>
                    <span v-else class="muted">未接管</span>
                  </td>
                  <td class="image-cell" :title="item.image">{{ item.image }}</td>
                  <td class="mono">{{ formatDate(item.created) }}</td>
                  <td class="mono">{{ item.ip || '—' }}</td>
                  <td class="ports-cell">{{ portsText(item.ports) }}</td>
                  <td class="actions-col">
                    <RouterLink
                      class="detail-link"
                      :to="`/nodes/${encodeURIComponent(node.id)}/containers/${encodeURIComponent(item.id)}`"
                      @click.stop
                    >
                      详情
                    </RouterLink>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <!-- Services -->
        <div v-else-if="tab === 'services'" class="tab-body">
          <p v-if="servicesFallback" class="banner warn">
            控制端或 Agent 还是旧版本，以下服务由容器标签推导；重启后可获得完整的 Swarm 服务清单。
          </p>
          <p v-if="servicesError" class="banner error">{{ servicesError }}</p>

          <div v-else-if="services.length === 0" class="empty">
            <p>这个节点没有可展示的服务。Swarm 服务需要节点加入集群；Compose 服务需要容器带有服务标签。</p>
          </div>

          <div v-else class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">名称</th>
                  <th scope="col">堆栈</th>
                  <th scope="col">镜像</th>
                  <th scope="col">模式</th>
                  <th scope="col">副本 / 容器</th>
                  <th scope="col">端口</th>
                  <th scope="col">状态</th>
                  <th scope="col">更新时间</th>
                  <th scope="col" class="actions-col"><span class="sr-only">操作</span></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="service in services" :key="service.id">
                  <td class="name-cell">
                    <strong>{{ service.name }}</strong>
                    <small>{{ service.source === 'swarm' ? 'Swarm 服务' : 'Compose 服务' }}</small>
                  </td>
                  <td><code class="stack">{{ service.stack || '—' }}</code></td>
                  <td class="image-cell" :title="service.image">{{ service.image || '—' }}</td>
                  <td>{{ service.mode || '—' }}</td>
                  <td class="mono">{{ service.running }} / {{ service.desired }}</td>
                  <td class="ports-cell">{{ portsText(service.ports) }}</td>
                  <td>
                    <span class="pill" :class="serviceTone(service)">
                      <i aria-hidden="true"></i>{{ serviceStateLabel(service) }}
                    </span>
                  </td>
                  <td class="mono">{{ formatDate(service.updated_at) }}</td>
                  <td class="actions-col">
                    <button type="button" class="detail-link" @click="showContainersOf(service)">
                      查看容器
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <!-- Networks -->
        <div v-else class="tab-body">
          <p v-if="networksError" class="banner error">{{ networksError }}</p>

          <div v-else-if="networks.length === 0" class="empty">
            <p>这个节点还没有上报网络。</p>
          </div>

          <div v-else class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">名称</th>
                  <th scope="col">驱动</th>
                  <th scope="col">范围</th>
                  <th scope="col">内部</th>
                  <th scope="col">容器</th>
                  <th scope="col">创建时间</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="network in networks" :key="network.id">
                  <td class="name-cell">
                    <strong>{{ network.name }}</strong>
                    <small class="mono">{{ network.id.slice(0, 12) }}</small>
                  </td>
                  <td>{{ network.driver || '—' }}</td>
                  <td>{{ network.scope || '—' }}</td>
                  <td>{{ network.internal ? '是' : '否' }}</td>
                  <td class="mono">{{ network.containers }}</td>
                  <td class="mono">{{ formatDate(network.created) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
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

.node-page {
  display: grid;
  gap: 16px;
}

.crumb {
  display: flex;
  align-items: center;
  gap: 8px;
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

.node-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.node-title {
  display: flex;
  align-items: center;
  gap: 13px;
  min-width: 0;
}

.node-mark {
  width: 48px;
  height: 48px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.node-mark svg {
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
  font-size: 26px;
  font-weight: 800;
  letter-spacing: -0.02em;
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

.status.warn {
  background: #fff4e6;
  color: #e07c00;
}

.status.off {
  background: #f0f3f9;
  color: #8595b2;
}

.node-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px 12px;
  margin-top: 4px;
  color: #7b8aa8;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.node-meta span + span::before {
  content: "·";
  margin-right: 12px;
  color: #c1ccdf;
}

.ghost-button {
  height: 34px;
  padding: 0 14px;
  border: 1px solid #dbe6f7;
  border-radius: 9px;
  background: #fff;
  color: #2f6fe4;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
}

.ghost-button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
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

.readings {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 14px;
}

.reading {
  padding: 14px 18px;
  border: 1px solid #e9eff9;
  border-radius: 14px;
  background: #fff;
  box-shadow: 0 14px 30px -34px rgb(20 52 110 / 50%);
}

.reading span {
  color: #7b8aa8;
  font-size: 11.5px;
}

.reading strong {
  display: block;
  margin-top: 1px;
  color: #132a56;
  font-size: 24px;
  font-weight: 800;
  line-height: 1.2;
  font-variant-numeric: tabular-nums;
}

.reading strong.reading-time {
  font-size: 14px;
  font-weight: 600;
  line-height: 1.7;
}

.reading small {
  color: #94a3bd;
  font-size: 11px;
}

.panel {
  overflow: hidden;
  border: 1px solid #e9eff9;
  border-radius: 18px;
  background: #fff;
  box-shadow: 0 18px 38px -36px rgb(20 52 110 / 50%);
}

.tabs {
  display: flex;
  gap: 24px;
  padding: 0 22px;
  border-bottom: 1px solid #eef3fb;
  overflow-x: auto;
}

.tabs button {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 52px;
  padding: 0 1px;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: #7b8aa8;
  font-size: 13.5px;
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
}

.tabs button span {
  padding: 0 6px;
  border-radius: 999px;
  background: #f2f6fd;
  color: #8595b2;
  font-size: 10.5px;
  line-height: 17px;
}

.tabs button.active {
  border-color: #0b63f6;
  color: #16264a;
  font-weight: 700;
}

.tabs button.active span {
  background: #e8f1ff;
  color: #2f6fe4;
}

.tab-body {
  padding: 18px 22px 22px;
}

.list-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}

.search {
  display: flex;
  align-items: center;
  gap: 8px;
  width: min(100%, 330px);
  height: 34px;
  padding: 0 11px;
  border: 1px solid #dbe6f7;
  border-radius: 9px;
  background: #fff;
  color: #8595b2;
}

.search svg {
  width: 15px;
  height: 15px;
  flex: none;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
}

.search input {
  min-width: 0;
  flex: 1;
  border: 0;
  outline: none;
  background: transparent;
  color: #34446b;
  font-size: 12.5px;
}

.toolbar-note {
  color: #94a3bd;
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.table-scroll {
  overflow-x: auto;
}

table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
  text-align: left;
}

th {
  padding: 9px 12px;
  border-bottom: 1px solid #eef3fb;
  background: #f8faff;
  color: #7b8aa8;
  font-size: 11px;
  font-weight: 600;
  white-space: nowrap;
}

td {
  padding: 12px;
  border-bottom: 1px solid #f1f5fc;
  color: #34446b;
  vertical-align: middle;
  white-space: nowrap;
}

tbody tr:last-child td {
  border-bottom: 0;
}

tbody tr:hover {
  background: #fbfdff;
}

.container-row {
  cursor: pointer;
}

.name-cell {
  display: grid;
  gap: 1px;
  min-width: 160px;
}

.name-cell strong,
.container-link {
  color: #16264a;
  font-size: 13px;
  font-weight: 600;
  text-decoration: none;
}

.container-link:hover {
  color: #0b63f6;
  text-decoration: none;
}

.name-cell small {
  color: #94a3bd;
  font-size: 10.5px;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: 10.5px;
  font-weight: 600;
}

.pill i {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: currentcolor;
}

.pill.ok {
  background: #e7f9f1;
  color: #0a9b6a;
}

.pill.bad {
  background: #fef0ef;
  color: #d92d20;
}

.pill.warn {
  background: #fff4e6;
  color: #e07c00;
}

.pill.off {
  background: #f0f3f9;
  color: #8595b2;
}

.health {
  display: block;
  margin-top: 2px;
  color: #94a3bd;
  font-size: 9.5px;
}

.stack {
  color: #2f6fe4;
  font-size: 11.5px;
}

.muted {
  color: #94a3bd;
}

.image-cell {
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--font-mono);
  font-size: 11.5px;
}

.mono {
  font-family: var(--font-mono);
  font-size: 11.5px;
  font-variant-numeric: tabular-nums;
}

.ports-cell {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--font-mono);
  font-size: 11px;
}

.actions-col {
  width: 1%;
  text-align: right;
}

.detail-link {
  border: 0;
  background: none;
  color: #2f6fe4;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
  text-decoration: none;
  white-space: nowrap;
}

.detail-link:hover {
  color: #0755d8;
}

.empty {
  display: grid;
  place-items: center;
  min-height: 150px;
  color: #94a3bd;
  font-size: 12.5px;
  text-align: center;
}

@media (max-width: 1000px) {
  .readings {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 640px) {
  .title-line h1 {
    font-size: 22px;
  }

  .readings {
    grid-template-columns: minmax(0, 1fr);
  }

  .tab-body {
    padding: 14px 14px 18px;
  }

  .tabs {
    padding: 0 14px;
  }
}
</style>
