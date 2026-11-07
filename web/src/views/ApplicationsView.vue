<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import AppModal from '../components/AppModal.vue'
import AppShell from '../components/AppShell.vue'
import YamlEditor from '../components/YamlEditor.vue'
import { api } from '../api/client'
import { createApplication, listApplications } from '../api/deployments'
import { listContainers, type ContainerSummary } from '../api/metrics'
import { containerOp } from '../api/nodes'
import { describeError, type AppMode, type AppSummary, type NodeSummary } from '../api/types'

const router = useRouter()

const applications = ref<AppSummary[]>([])
const nodes = ref<NodeSummary[]>([])
// containersByNode keeps the containers of every node that hosts at least
// one application, so the page can count the services and the states of a
// site without asking the node once per application.
const containersByNode = ref(new Map<string, ContainerSummary[]>())

const loading = ref(true)
const error = ref('')
const notice = ref('')

// The search and the two filters of the toolbar.
const search = ref('')
const statusFilter = ref<'all' | 'running' | 'partial' | 'stopped'>('all')
const modeFilter = ref<'all' | AppMode>('all')
const page = ref(1)
const pageSize = ref(10)

// hostnameOf names the target by the name the administrator knows.
function hostnameOf(targetID: string): string {
  return nodes.value.find((node) => node.id === targetID)?.hostname ?? targetID
}

// containersOf gathers the containers of one application from the
// containers of its node.
function containersOf(app: AppSummary): ContainerSummary[] {
  const all = containersByNode.value.get(app.target_id) ?? []
  return all.filter((item) => item.namespace === app.namespace)
}

// A row of the list: the application with the facts its containers tell.
interface AppRow {
  app: AppSummary
  containers: ContainerSummary[]
  serviceCount: number
  running: number
  status: 'running' | 'partial' | 'stopped'
}

// appRows joins the applications with the state of their containers.
const appRows = computed<AppRow[]>(() =>
  applications.value.map((app) => {
    const items = containersOf(app)
    const services = new Set(items.map((item) => item.service || item.name))
    const running = items.filter((item) => item.state === 'running').length
    let status: AppRow['status'] = 'stopped'
    if (items.length > 0 && running === items.length) {
      status = 'running'
    } else if (running > 0) {
      status = 'partial'
    }
    return { app, containers: items, serviceCount: services.size, running, status }
  }),
)

// statusLabels and statusTones name a state of a site the way the list
// reads it.
const statusLabels: Record<AppRow['status'], string> = {
  running: '运行中',
  partial: '部分异常',
  stopped: '已停止',
}

const statusTones: Record<AppRow['status'], string> = {
  running: 'ok',
  partial: 'warn',
  stopped: 'off',
}

// theStats are the four readings of the page.
const theStats = computed(() => {
  const rows = appRows.value
  const running = rows.filter((row) => row.status === 'running').length
  const stopped = rows.filter((row) => row.status === 'stopped').length
  const services = rows.reduce((sum, row) => sum + row.serviceCount, 0)
  return {
    total: rows.length,
    running,
    stopped,
    services,
  }
})

// theFilteredRows applies the search and the two filters of the toolbar.
const theFilteredRows = computed(() =>
  appRows.value.filter((row) => {
    const needle = search.value.trim().toLowerCase()
    if (
      needle !== '' &&
      !`${row.app.name} ${row.app.description} ${hostnameOf(row.app.target_id)} ${row.containers
        .map((item) => item.service || item.name)
        .join(' ')}`
        .toLowerCase()
        .includes(needle)
    ) {
      return false
    }
    if (statusFilter.value !== 'all' && row.status !== statusFilter.value) {
      return false
    }
    if (modeFilter.value !== 'all' && row.app.mode !== modeFilter.value) {
      return false
    }
    return true
  }),
)

// thePageRows are the rows of the page the administrator reads.
const thePageRows = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return theFilteredRows.value.slice(start, start + pageSize.value)
})

const pageCount = computed(() => Math.max(1, Math.ceil(theFilteredRows.value.length / pageSize.value)))

// pageOf writes the page numbers of a pager.
function pageOf(current: number, count: number): number[] {
  if (count <= 7) {
    return Array.from({ length: count }, (_, index) => index + 1)
  }
  const start = Math.max(1, Math.min(current - 2, count - 4))
  return Array.from({ length: 5 }, (_, index) => start + index)
}

// A filter that changes resets the page.
watch([search, statusFilter, modeFilter, pageSize], () => {
  page.value = 1
})

// load reads the applications, the nodes and the containers of every node
// that hosts an application.
async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    applications.value = await listApplications()
    const payload = await api<{ nodes: NodeSummary[] }>('/api/v1/nodes')
    nodes.value = payload.nodes
    const nodeIDs = [...new Set(applications.value.map((app) => app.target_id))]
    const results = await Promise.all(
      nodeIDs.map((id) =>
        listContainers(id).then(
          (items) => ({ id, items }),
          () => ({ id, items: [] as ContainerSummary[] }),
        ),
      ),
    )
    const map = new Map<string, ContainerSummary[]>()
    for (const result of results) {
      map.set(result.id, result.items)
    }
    containersByNode.value = map
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

// powerApps starts or stops every container of a site, one container at a
// time. A container that refuses keeps going: the page reports the site as
// a whole, not every container of it.
const operating = ref('')
const actionError = ref('')

async function powerApps(row: AppRow, action: 'start' | 'stop'): Promise<void> {
  if (operating.value !== '') {
    return
  }
  operating.value = row.app.id
  actionError.value = ''
  const failures: string[] = []
  for (const item of row.containers) {
    try {
      await containerOp(row.app.target_id, item.id, action)
    } catch (failure) {
      failures.push(`${item.name}: ${describeError(failure)}`)
    }
  }
  operating.value = ''
  if (failures.length > 0) {
    actionError.value = failures.join('；')
  }
  await load()
}

/* ------------------------------------------------------------------ */
/* The wizard that creates a site in three steps.                      */
/* ------------------------------------------------------------------ */

const creating = ref(false)
const wizardStep = ref(1)
const wName = ref('')
const wDescription = ref('')
const wMode = ref<AppMode>('compose')
const wTarget = ref('')
const wNamespace = ref('')
const wAdopt = ref(false)
const wYaml = ref('')
const wBusy = ref(false)
const wError = ref('')

// The templates a new site starts from, one per mode.
const composeTemplate = `services:
  web:
    image: nginx:1.28
    ports:
      - "80:80"
`
const swarmTemplate = `version: "3.8"

services:
  web:
    image: nginx:1.28
    ports:
      - "80:80"
`

// openWizard starts a fresh wizard.
function openWizard(): void {
  wizardStep.value = 1
  wName.value = ''
  wDescription.value = ''
  wMode.value = 'compose'
  wTarget.value = ''
  wNamespace.value = ''
  wAdopt.value = false
  wYaml.value = composeTemplate
  wError.value = ''
  creating.value = true
}

// closeWizard closes the wizard without creating anything.
function closeWizard(): void {
  creating.value = false
  wError.value = ''
}

// namespaceOf suggests the name of the project of an application.
//
// The name of the project of Compose and of the stack of Swarm may only
// hold small letters, digits and separators.
function namespaceOf(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/^[^a-z0-9]+/, '')
    .slice(0, 63)
}

// A name that is typed suggests the namespace, unless the administrator
// already wrote one.
watch(wName, (value) => {
  wNamespace.value = namespaceOf(value)
})

// pickMode switches the mode of the wizard and pre-fills the template of
// the third step, so the YAML always matches the mode.
function pickMode(mode: AppMode): void {
  wMode.value = mode
  wYaml.value = mode === 'swarm' ? swarmTemplate : composeTemplate
}

// wizardNext walks one step forward after the checks of the step.
function wizardNext(): void {
  wError.value = ''
  if (wizardStep.value === 1) {
    if (wName.value.trim() === '') {
      wError.value = '请填写站点名称。'
      return
    }
    wizardStep.value = 2
    return
  }
  if (wizardStep.value === 2) {
    if (wTarget.value.trim() === '') {
      wError.value = wMode.value === 'compose' ? '请选择目标节点。' : '请填写集群标识。'
      return
    }
    if (wNamespace.value.trim() === '') {
      wError.value = '请填写命名空间。'
      return
    }
    wizardStep.value = 3
  }
}

// wizardBack walks one step back.
function wizardBack(): void {
  wError.value = ''
  if (wizardStep.value > 1) {
    wizardStep.value -= 1
  }
}

// wizardCreate writes the site and opens it with the YAML of the third
// step carried along, so the editor of the page starts filled.
async function wizardCreate(): Promise<void> {
  if (wBusy.value) {
    return
  }
  if (wYaml.value.trim() === '') {
    wError.value = '请填写站点的 Compose 配置。'
    return
  }
  wBusy.value = true
  wError.value = ''
  try {
    const created = await createApplication({
      name: wName.value.trim(),
      description: wDescription.value.trim(),
      mode: wMode.value,
      target_id: wTarget.value.trim(),
      namespace: wNamespace.value.trim(),
      adopt: wAdopt.value,
    })
    creating.value = false
    notice.value = '站点已创建。配置已载入，预览后即可保存并部署。'
    await router.push({
      path: `/applications/${encodeURIComponent(created.id)}`,
      state: { initialYaml: wYaml.value },
    })
  } catch (failure) {
    // The input of the wizard stays, so the reason of the refusal can be
    // fixed without starting over.
    wError.value = describeError(failure)
  } finally {
    wBusy.value = false
  }
}

const wizardSteps = ['基本信息', '部署目标', '部署配置']

onMounted(load)

// The containers of a site follow the inventory: the page reads itself
// again while it is open, at the pace of the agents.
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

function formatDate(value: string): string {
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
</script>

<template>
  <AppShell>
    <div class="page-head">
      <div>
        <h1 class="page-title">站点管理</h1>
        <p class="page-subtitle">管理 Docker 站点（Compose 项目或 Swarm 服务集合），快速查看运行状态并进行管理。</p>
      </div>
    </div>

    <!-- The four readings -->
    <section class="stats" aria-label="站点统计">
      <div class="stat-card">
        <span class="stat-icon sites">
          <svg viewBox="0 0 24 24">
            <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
            <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
          </svg>
        </span>
        <span class="stat-copy">
          <strong>{{ theStats.total }}</strong>
          <small>站点总数</small>
        </span>
      </div>
      <div class="stat-card">
        <span class="stat-icon running">
          <svg viewBox="0 0 24 24">
            <circle cx="12" cy="12" r="9" fill="currentColor" />
            <path d="m10 8.5 6 3.5-6 3.5V8.5Z" fill="#fff" stroke="none" />
          </svg>
        </span>
        <span class="stat-copy">
          <strong>{{ theStats.running }}</strong>
          <small>运行中</small>
        </span>
      </div>
      <div class="stat-card">
        <span class="stat-icon stopped">
          <svg viewBox="0 0 24 24">
            <circle cx="12" cy="12" r="9" fill="currentColor" />
            <path d="M9.5 8.5v7M14.5 8.5v7" stroke="#fff" stroke-width="2" stroke-linecap="round" />
          </svg>
        </span>
        <span class="stat-copy">
          <strong>{{ theStats.stopped }}</strong>
          <small>已停止</small>
        </span>
      </div>
      <div class="stat-card">
        <span class="stat-icon services">
          <svg viewBox="0 0 24 24">
            <circle cx="12" cy="12" r="8.5" fill="currentColor" />
            <circle cx="12" cy="12" r="3.4" fill="#fff" />
          </svg>
        </span>
        <span class="stat-copy">
          <strong>{{ theStats.services }}</strong>
          <small>服务总数</small>
        </span>
      </div>
    </section>

    <section class="panel">
      <div class="toolbar">
        <div class="toolbar-fields">
          <label class="search">
            <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m20 20-3.8-3.8" /></svg>
            <input
              v-model="search"
              type="search"
              name="site-search"
              autocomplete="off"
              placeholder="搜索站点名称、服务或节点…"
            />
          </label>
          <select v-model="statusFilter" class="select" name="site-status" aria-label="状态">
            <option value="all">全部状态</option>
            <option value="running">运行中</option>
            <option value="partial">部分异常</option>
            <option value="stopped">已停止</option>
          </select>
          <select v-model="modeFilter" class="select" name="site-mode" aria-label="部署类型">
            <option value="all">全部类型</option>
            <option value="compose">Compose</option>
            <option value="swarm">Swarm</option>
          </select>
        </div>
        <button type="button" class="primary-button" @click="openWizard">＋ 创建站点</button>
      </div>

      <p v-if="loading" class="page-loading">正在读取站点…</p>
      <div v-else-if="error" class="page-error">
        <p>{{ error }}</p>
        <button type="button" class="text-button" @click="load()">重试</button>
      </div>

      <template v-else>
        <div v-if="theFilteredRows.length === 0" class="empty">
          <p>{{ appRows.length === 0 ? '还没有站点。创建一个站点，把 Compose 文件或 Swarm Stack 部署到节点上。' : '没有符合搜索条件的站点。' }}</p>
        </div>

        <table v-else class="site-table">
          <thead>
            <tr>
              <th>站点名称</th>
              <th>类型</th>
              <th>服务数量</th>
              <th>状态</th>
              <th>所在节点</th>
              <th>创建时间</th>
              <th class="th-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in thePageRows" :key="row.app.id">
              <td>
                <div class="site-cell">
                  <span class="site-icon" :class="row.app.mode" aria-hidden="true">
                    <svg viewBox="0 0 24 24">
                      <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
                      <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
                    </svg>
                  </span>
                  <div class="site-copy">
                    <RouterLink class="site-link" :to="`/applications/${encodeURIComponent(row.app.id)}`">
                      {{ row.app.name }}
                    </RouterLink>
                    <small>{{ row.app.description || '—' }}</small>
                  </div>
                </div>
              </td>
              <td>
                <span class="chip" :class="row.app.mode">{{ row.app.mode === 'swarm' ? 'Swarm' : 'Compose' }}</span>
              </td>
              <td class="mono">{{ row.serviceCount }}</td>
              <td>
                <span class="pill" :class="statusTones[row.status]">
                  <i aria-hidden="true"></i>{{ statusLabels[row.status] }}
                </span>
              </td>
              <td>{{ hostnameOf(row.app.target_id) }}</td>
              <td class="mono">{{ formatDate(row.app.created_at) }}</td>
              <td>
                <div class="row-actions">
                  <RouterLink
                    class="text-button"
                    :to="{ path: `/applications/${encodeURIComponent(row.app.id)}`, query: { tab: 'config' } }"
                  >
                    管理
                  </RouterLink>
                  <button
                    v-if="row.status !== 'running'"
                    type="button"
                    class="text-button"
                    :disabled="operating === row.app.id"
                    @click="powerApps(row, 'start')"
                  >
                    {{ operating === row.app.id ? '启动中…' : '启动' }}
                  </button>
                  <button
                    v-else
                    type="button"
                    class="text-button"
                    :disabled="operating === row.app.id"
                    @click="powerApps(row, 'stop')"
                  >
                    {{ operating === row.app.id ? '停止中…' : '停止' }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div class="table-foot">
          <span class="muted-cell">共 {{ theFilteredRows.length }} 条</span>
          <div class="pager">
            <button
              type="button"
              class="pager-step"
              :disabled="page <= 1"
              aria-label="上一页"
              @click="page -= 1"
            >
              ‹
            </button>
            <button
              v-for="p in pageOf(page, pageCount)"
              :key="p"
              type="button"
              class="pager-page"
              :class="{ active: p === page }"
              :aria-pressed="p === page"
              @click="page = p"
            >
              {{ p }}
            </button>
            <button
              type="button"
              class="pager-step"
              :disabled="page >= pageCount"
              aria-label="下一页"
              @click="page += 1"
            >
              ›
            </button>
            <select v-model.number="pageSize" class="select compact" name="site-page-size" aria-label="每页条数">
              <option :value="10">10 条/页</option>
              <option :value="20">20 条/页</option>
              <option :value="50">50 条/页</option>
            </select>
          </div>
        </div>
      </template>
    </section>

    <p v-if="actionError" class="banner error" role="alert">{{ actionError }}</p>
    <p v-if="notice" class="banner success" role="status">{{ notice }}</p>

    <!-- The wizard of a new site -->
    <AppModal :open="creating" :title="`创建站点 · ${wizardSteps[wizardStep - 1]}`" wide @close="closeWizard">
      <div class="wizard">
        <ol class="wizard-steps" aria-label="创建步骤">
          <li
            v-for="(label, index) in wizardSteps"
            :key="label"
            :class="{ active: wizardStep === index + 1, done: wizardStep > index + 1 }"
          >
            <span>{{ index + 1 }}</span>{{ label }}
          </li>
        </ol>

        <p v-if="wError" class="wizard-error">{{ wError }}</p>

        <div v-if="wizardStep === 1" class="wizard-body">
          <div class="field">
            <label class="label" for="wizard-name">站点名称 <b class="required">*</b></label>
            <input
              id="wizard-name"
              v-model="wName"
              class="input"
              type="text"
              name="name"
              maxlength="64"
              autocomplete="off"
              placeholder="请输入站点名称，例如：本地开发环境"
            />
          </div>
          <div class="field">
            <label class="label" for="wizard-description">站点描述</label>
            <textarea
              id="wizard-description"
              v-model="wDescription"
              class="area"
              name="description"
              maxlength="200"
              rows="2"
              placeholder="请输入站点描述（选填）"
            ></textarea>
          </div>
          <div class="field">
            <span class="label">部署类型 <b class="required">*</b></span>
            <div class="radio-row">
              <label class="radio-pill" :class="{ checked: wMode === 'compose' }">
                <input type="radio" name="wizard-mode" :checked="wMode === 'compose'" @change="pickMode('compose')" />
                <span>Compose</span>
              </label>
              <label class="radio-pill" :class="{ checked: wMode === 'swarm' }">
                <input type="radio" name="wizard-mode" :checked="wMode === 'swarm'" @change="pickMode('swarm')" />
                <span>Swarm</span>
              </label>
            </div>
            <p class="field-hint">模式在创建之后不能更改。</p>
          </div>
        </div>

        <div v-else-if="wizardStep === 2" class="wizard-body">
          <div class="field">
            <label class="label" for="wizard-target">部署目标 <b class="required">*</b></label>
            <select v-if="wMode === 'compose'" id="wizard-target" v-model="wTarget" class="input" name="target_id">
              <option value="">请选择节点</option>
              <option v-for="node in nodes" :key="node.id" :value="node.id">
                {{ node.hostname }}（{{ node.id }}）
              </option>
            </select>
            <input
              v-else
              id="wizard-target"
              v-model="wTarget"
              class="input"
              type="text"
              name="target_id"
              placeholder="请输入集群标识"
            />
            <p class="field-hint">
              {{ wMode === 'compose' ? 'Compose 应用运行在一个节点上。' : 'Swarm 应用运行在一个集群上，请填写集群的标识。' }}
            </p>
          </div>
          <div class="field">
            <label class="label" for="wizard-namespace">命名空间 <b class="required">*</b></label>
            <input
              id="wizard-namespace"
              v-model="wNamespace"
              class="input"
              type="text"
              name="namespace"
              autocomplete="off"
            />
            <p class="field-hint">
              命名空间是 Compose 项目和 Swarm Stack 的名字，只能包含小写字母、数字、- 和 _。
            </p>
          </div>
          <label class="check">
            <input v-model="wAdopt" type="checkbox" name="adopt" />
            <span>我确认接管该命名空间下已有的资源</span>
          </label>
          <p class="field-hint">只有在该命名空间里已有平台不管理的资源时，才需要勾选这一项。</p>
        </div>

        <div v-else class="wizard-body">
          <div class="field">
            <label class="label" for="wizard-yaml">Compose / Stack 配置 <b class="required">*</b></label>
            <div class="yaml-frame">
              <YamlEditor v-model="wYaml" />
            </div>
            <p class="field-hint">
              站点创建后配置会载入详情页，预览确认后即可保存并部署。
            </p>
          </div>
        </div>
      </div>

      <template #footer>
        <button v-if="wizardStep > 1" type="button" class="ghost-button" @click="wizardBack">上一步</button>
        <button v-if="wizardStep < 3" type="button" class="primary-button" @click="wizardNext">下一步</button>
        <button v-else type="button" class="primary-button" :disabled="wBusy" @click="wizardCreate">
          {{ wBusy ? '创建中…' : '创建站点' }}
        </button>
      </template>
    </AppModal>
  </AppShell>
</template>

<style scoped>
/* The four readings ---------------------------------------------------- */
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

.stat-icon.running {
  background: #e5f9ef;
  color: #0bbf7a;
}

.stat-icon.stopped {
  background: #feeceb;
  color: #f04438;
}

.stat-icon.services {
  background: #f3ecff;
  color: #7839ee;
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

/* The panel and its toolbar --------------------------------------------- */
.panel {
  border: 1px solid #e9eff9;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 14px 30px -32px rgb(20 52 110 / 50%);
  padding: 20px 22px;
  margin-top: 22px;
}

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.toolbar-fields {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  min-width: 0;
}

.search {
  position: relative;
  display: inline-flex;
  align-items: center;
}

.search svg {
  position: absolute;
  left: 11px;
  width: 15px;
  height: 15px;
  fill: none;
  stroke: #98a2b3;
  stroke-width: 1.8;
  stroke-linecap: round;
  pointer-events: none;
}

.search input {
  height: 38px;
  width: 250px;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 0 12px 0 32px;
  font-size: 13px;
  color: #132a56;
  outline: none;
}

.search input:focus {
  border-color: #0255f0;
}

.select {
  height: 38px;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 0 10px;
  font-size: 13px;
  color: #132a56;
  outline: none;
}

.select:focus {
  border-color: #0255f0;
}

.select.compact {
  height: 32px;
  font-size: 12px;
}

/* The buttons ------------------------------------------------------------ */
.primary-button {
  appearance: none;
  border: none;
  border-radius: 10px;
  background: #0255f0;
  color: #fff;
  font-size: 14px;
  font-weight: 600;
  padding: 9px 18px;
  cursor: pointer;
}

.primary-button:hover {
  background: #0249d0;
}

.primary-button:disabled {
  opacity: 0.6;
  cursor: default;
}

.ghost-button {
  appearance: none;
  border: 1px solid #cfdcf3;
  border-radius: 10px;
  background: #fff;
  color: #475467;
  font-size: 14px;
  font-weight: 600;
  padding: 8px 20px;
  cursor: pointer;
}

.ghost-button:hover {
  border-color: #9dbdf5;
  color: #132a56;
}

.text-button {
  appearance: none;
  border: none;
  background: none;
  padding: 0;
  font-size: 13px;
  font-weight: 600;
  color: #0255f0;
  cursor: pointer;
  text-decoration: none;
}

.text-button:hover {
  text-decoration: underline;
}

.text-button:disabled {
  opacity: 0.5;
  cursor: default;
}

/* The table of the sites --------------------------------------------------- */
.site-table {
  width: 100%;
  border-collapse: collapse;
  text-align: left;
}

.site-table th {
  padding: 12px 14px;
  font-size: 12px;
  font-weight: 600;
  color: #667085;
  border-bottom: 1px solid #e6ecf7;
  white-space: nowrap;
}

.site-table td {
  padding: 14px;
  border-bottom: 1px solid #eef2fa;
  font-size: 13px;
  color: #132a56;
  vertical-align: middle;
}

.site-table tbody tr:last-child td {
  border-bottom: none;
}

.site-table tbody tr:hover {
  background: #f8fbff;
}

.th-actions {
  text-align: right;
}

.site-table td:last-child {
  text-align: right;
}

.site-cell {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 220px;
}

.site-icon {
  width: 38px;
  height: 38px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 11px;
  background: #e9f2ff;
  color: #146ef5;
}

.site-icon.swarm {
  background: #f3ecff;
  color: #7839ee;
}

.site-icon svg {
  width: 19px;
  height: 19px;
}

.site-copy {
  min-width: 0;
}

.site-link {
  color: #132a56;
  font-size: 13.5px;
  font-weight: 600;
  text-decoration: none;
}

.site-link:hover {
  color: #0255f0;
}

.site-copy small {
  display: block;
  margin-top: 2px;
  font-size: 12px;
  color: #8ba0bf;
}

.chip {
  display: inline-flex;
  align-items: center;
  border-radius: 8px;
  padding: 3px 10px;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}

.chip.compose {
  background: #f0f5ff;
  color: #1d4fd8;
}

.chip.swarm {
  background: #f3ecff;
  color: #6d28d9;
}

.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  padding: 2px 10px;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}

.pill i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentcolor;
}

.pill.ok {
  background: #ecfdf3;
  color: #067647;
}

.pill.warn {
  background: #fffaeb;
  color: #b54708;
}

.pill.off {
  background: #f2f4f7;
  color: #475467;
}

.mono {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.row-actions {
  display: inline-flex;
  align-items: center;
  gap: 12px;
  white-space: nowrap;
}

.muted-cell {
  color: #8ba0bf;
  font-size: 12.5px;
}

/* The foot of the table ------------------------------------------------------ */
.table-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-top: 14px;
}

.pager {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.pager-step,
.pager-page {
  appearance: none;
  min-width: 30px;
  height: 30px;
  border: 1px solid #d8e2f5;
  border-radius: 8px;
  background: #fff;
  color: #344054;
  font-size: 13px;
  cursor: pointer;
  padding: 0 6px;
}

.pager-step:disabled {
  opacity: 0.45;
  cursor: default;
}

.pager-page.active {
  border-color: #0255f0;
  color: #0255f0;
  font-weight: 700;
}

/* The empty state --------------------------------------------------------------- */
.empty {
  padding: 46px 0;
  text-align: center;
  color: #667085;
  font-size: 13.5px;
}

.page-loading {
  padding: 46px 0;
  text-align: center;
  color: #667085;
}

.page-error {
  padding: 40px 0;
  text-align: center;
  color: #d92d20;
}

/* The banners ---------------------------------------------------------------------- */
.banner {
  margin-top: 14px;
  padding: 10px 14px;
  border-radius: 10px;
  font-size: 13px;
}

.banner.error {
  background: #fef3f2;
  color: #b42318;
}

.banner.success {
  background: #ecfdf3;
  color: #067647;
}

/* The wizard of a new site ------------------------------------------------------------ */
.wizard {
  min-height: 220px;
}

.wizard-steps {
  display: flex;
  gap: 8px;
  margin: 0 0 18px;
  padding: 0;
  list-style: none;
}

.wizard-steps li {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  border: 1px solid #d8e2f5;
  border-radius: 10px;
  background: #fff;
  color: #667085;
  font-size: 12.5px;
  font-weight: 600;
}

.wizard-steps li span {
  width: 20px;
  height: 20px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #f2f4f7;
  color: #667085;
  font-size: 11px;
  font-weight: 700;
}

.wizard-steps li.active {
  border-color: #0255f0;
  background: #f0f5ff;
  color: #0255f0;
}

.wizard-steps li.active span {
  background: #0255f0;
  color: #fff;
}

.wizard-steps li.done {
  color: #067647;
  border-color: #a6e9c7;
}

.wizard-steps li.done span {
  background: #0bbf7a;
  color: #fff;
}

.wizard-error {
  margin: 0 0 12px;
  padding: 9px 12px;
  border-radius: 9px;
  background: #fef3f2;
  color: #b42318;
  font-size: 12.5px;
}

.wizard-body {
  display: grid;
  gap: 14px;
}

.field {
  display: grid;
  gap: 6px;
}

.label {
  font-size: 12.5px;
  font-weight: 600;
  color: #344054;
}

.required {
  color: #f04438;
  font-weight: 700;
}

.input {
  height: 38px;
  width: 100%;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 0 12px;
  font-size: 13px;
  color: #132a56;
  outline: none;
}

.input:focus {
  border-color: #0255f0;
}

.area {
  width: 100%;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 9px 12px;
  font-size: 13px;
  color: #132a56;
  font-family: inherit;
  line-height: 1.5;
  resize: vertical;
  outline: none;
}

.area:focus {
  border-color: #0255f0;
}

.field-hint {
  margin: 0;
  font-size: 12px;
  color: #8ba0bf;
}

.radio-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}

.radio-pill {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  border: 1px solid #d8e2f5;
  border-radius: 999px;
  padding: 6px 14px;
  font-size: 13px;
  color: #475467;
  cursor: pointer;
}

.radio-pill input {
  width: 14px;
  height: 14px;
  accent-color: #0255f0;
  margin: 0;
}

.radio-pill.checked {
  border-color: #0255f0;
  background: #f0f5ff;
  color: #0255f0;
  font-weight: 600;
}

.check {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: #344054;
}

.check input {
  width: 15px;
  height: 15px;
  accent-color: #0255f0;
}

.yaml-frame {
  border: 1px solid #d3dcec;
  border-radius: 10px;
  overflow: hidden;
}

@media (max-width: 1100px) {
  .stats {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 720px) {
  .stats {
    grid-template-columns: 1fr;
  }

  .search input {
    width: 100%;
  }

  .toolbar-fields {
    width: 100%;
  }
}
</style>
