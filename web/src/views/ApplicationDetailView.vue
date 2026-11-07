<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton } from 'naive-ui'
import { useRoute, useRouter } from 'vue-router'
import { parse as parseYaml, stringify as stringifyYaml } from 'yaml'
import AppShell from '../components/AppShell.vue'
import DeploymentHistory from '../components/DeploymentHistory.vue'
import DeploymentStatus from '../components/DeploymentStatus.vue'
import YamlEditor from '../components/YamlEditor.vue'

import { api } from '../api/client'
import { containerOp, listNodeEvents } from '../api/nodes'
import {
  getApplication,
  getDeployment,
  getDeploymentOutput,
  getVersion,
  listVersions,
  newIdempotencyKey,
  outputText,
  previewDeployment,
  reconcileDeployment,
  resolveDeployment,
  submitDeployment,
  subscribeEvents,
  type ControlEvent,
  type EventSubscription,
} from '../api/deployments'
import { listContainers, type ContainerSummary } from '../api/metrics'
import {
  ApiError,
  configFailures,
  describeError,
  formatTimestamp,
  type AppConfig,
  type Deployment,
  type NodeListPayload,
  type NodeEvent,
  type NodeSummary,
  type PreviewResult,
  type TaskState,
  type VersionSummary,
} from '../api/types'

const route = useRoute()
const router = useRouter()

// The phases in which an application has a deployment that is not finished.
//
// An application has at most one deployment that is not finished, so a
// second one is refused by the control端. The page refuses it before the
// request, so the administrator does not wait for an answer that cannot
// come.
const blockedStates = new Set<TaskState>(['queued', 'preflight', 'running', 'verifying', 'uncertain'])

// isDeployBlocked reports whether the application has a deployment that is
// not finished.
const isDeployBlocked = (state: TaskState | null) => state !== null && blockedStates.has(state)

type Tab = 'overview' | 'containers' | 'services' | 'logs' | 'config' | 'history' | 'events'

const tabs: { key: Tab; label: string; icon: string }[] = [
  {
    key: 'overview',
    label: '概览',
    icon: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 8v.01M12 11v5',
  },
  {
    key: 'containers',
    label: '容器',
    icon: 'M12 3 3 8v8l9 5 9-5V8L12 3ZM3 8l9 5 9-5M12 13v8',
  },
  {
    key: 'services',
    label: '服务',
    icon: 'M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01',
  },
  {
    key: 'logs',
    label: '部署输出',
    icon: 'm5 7 5 5-5 5M12 17h7',
  },
  {
    key: 'config',
    label: 'YAML 配置',
    icon: 'M7 4h7l4 4v12H7V4ZM14 4v4h4M10 12h5M10 16h5',
  },
  {
    key: 'history',
    label: '部署记录',
    icon: 'M4 7a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V7Z',
  },
  {
    key: 'events',
    label: '事件日志',
    icon: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 7v5l3 2',
  },
]

const tab = ref<Tab>('overview')

// theTabs are the tabs of the bar. The output of a deployment has a tab of
// its own only while the page tracks a deployment, so the bar keeps the
// five standing tabs of the prototype.
const theTabs = computed(() =>
  tabs.filter((item) => item.key !== 'logs' || deployment.value !== null),
)

// The editor of the YAML tab starts read-only: a click on 编辑配置 turns
// it into an editor, so a visitor cannot change the configuration by
// accident.
const configEditing = ref(false)
// configNotice carries the word of the copy and of the format of the
// editor.
const configNotice = ref('')
// The restart of a site restarts every container of it, after the
// administrator confirmed.
const confirmingRestart = ref(false)
const restarting = ref(false)
const restartError = ref('')

// The events of the containers of the site: the log of what happened to
// them, newest first.
const events = ref<NodeEvent[]>([])
const eventsError = ref('')
let eventsTimer: ReturnType<typeof setInterval> | undefined

const app = ref<Awaited<ReturnType<typeof getApplication>> | null>(null)
const versions = ref<VersionSummary[]>([])
const deployment = ref<Deployment | null>(null)
const output = ref('')

const loading = ref(true)
const error = ref('')
const notice = ref('')

// The editor holds the configuration the administrator is working on. It
// starts with the configuration of the server and is never written to the
// storage of the browser.
const yaml = ref('')
const variables = ref<Record<string, string>>({})
const drafts = ref<Record<string, string>>({})
const newVariableName = ref('')
const newVariableValue = ref('')

const preview = ref<PreviewResult | null>(null)
const previewedContent = ref('')
const previewing = ref(false)
const previewError = ref('')
const previewFailures = ref<string[]>([])

const submitting = ref(false)
const submitError = ref('')
const confirmedRemoval = ref(false)
const confirmedRisk = ref(false)

// The key of a submission belongs to the content of the submission.
const idempotencyKey = ref('')
const keyedContent = ref('')

const serverChanged = ref(false)
const reconciling = ref(false)
const reconcileError = ref('')
// resolveForm reports whether the form of the release is open. The form is
// not the request: opening it must not keep the request from being sent.
const resolveForm = ref(false)
const resolving = ref(false)
const resolveReason = ref('')
const resolveStopped = ref(false)
const resolveError = ref('')

let subscription: EventSubscription | null = null

// The page names them by their Compose
// service, so the administrator picks one instead of typing an identifier.
const appContainers = ref<ContainerSummary[]>([])
const appContainersError = ref('')

// The search and the filters of the container list.
const containerSearch = ref('')
const containerStateFilter = ref<'all' | 'running' | 'stopped' | 'abnormal'>('all')
const containerImageFilter = ref<'all' | string>('all')
const containerPage = ref(1)
const containerPageSize = ref(10)

//
// The page only needs to know whether the node of the target is connected,
// so a failure to read the list never hides the application itself.
const nodes = ref<NodeSummary[]>([])

// now is the moment of the page.
//
// Whether the data of the curve is fresh depends on the time, so the page
// reads the clock from time to time and tells an old curve from a new one.
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | null = null

function currentConfig(): AppConfig {
  return { yaml: yaml.value, variables: { ...variables.value } }
}

// contentKey identifies the content of the editor.
//
// The key tells a configuration that changed from one that did not, so the
// preview of a configuration is never used for another one.
function contentKey(): string {
  const names = Object.keys(variables.value).sort()
  const values = names.map((name) => `${name}=${variables.value[name]}`).join('\n')
  return `${yaml.value}\n---\n${values}`
}

// loadApplication reads the application and its configuration.
async function loadApplication(): Promise<void> {
  const appID = String(route.params.id ?? '')
  const loaded = await getApplication(appID)
  app.value = loaded
  // The editor starts with the configuration of the server. A local change
  // is never lost by a reload of the page. The wizard of a new site carries
  // the configuration of its third step along, so the editor of a site
  // without a version starts filled with what the administrator wrote.
  const carried = (history.state as { initialYaml?: string } | null)?.initialYaml
  if (carried && loaded.latest_version_id === '') {
    yaml.value = carried
  } else {
    yaml.value = loaded.config.yaml
  }
  variables.value = { ...loaded.config.variables }
  drafts.value = {}
  preview.value = null
  previewedContent.value = ''
}

// loadVersions reads the versions of the application.
async function loadVersions(): Promise<void> {
  const appID = String(route.params.id ?? '')
  const payload = await listVersions(appID)
  versions.value = payload.versions
}

// loadDeployment reads the state of a deployment and its output.
async function loadDeployment(taskID: string): Promise<void> {
  const loaded = await getDeployment(taskID)
  deployment.value = loaded
  await loadOutput(taskID)
}

// loadOutput reads the output of a deployment.
async function loadOutput(taskID: string): Promise<void> {
  try {
    output.value = outputText(await getDeploymentOutput(taskID))
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// loadNodes reads the nodes of the deployment.
//
// The list only says whether the agent of the node of the target is
// connected, so a failure to read it never hides the application.
async function loadNodes(): Promise<void> {
  try {
    const payload = await api<NodeListPayload>('/api/v1/nodes')
    nodes.value = payload.nodes
  } catch {
    // The page works without the list: it simply cannot tell a broken
    // connection from a quiet one.
  }
}

// loadAppContainers reads the containers of the application.
//
// The containers belong to the Compose project of the application, so the
// page asks for them by the namespace of the application. A container is
// named by its Compose service, so the administrator picks one instead of
// typing an identifier.
async function loadAppContainers(): Promise<void> {
  appContainersError.value = ''
  const current = app.value
  if (!current) {
    appContainers.value = []
    return
  }
  try {
    const list = await listContainers(current.target_id, current.namespace)
    appContainers.value = list
  } catch (failure) {
    appContainers.value = []
    appContainersError.value = describeError(failure)
  }
}

// theContainerStats counts the containers by the state they are in.
const theContainerStats = computed(() => {
  let running = 0
  let stopped = 0
  let abnormal = 0
  for (const item of appContainers.value) {
    if (item.state === 'running') {
      if (item.health === 'unhealthy') {
        abnormal += 1
      } else {
        running += 1
      }
    } else if (item.state === 'restarting' || item.state === 'paused') {
      abnormal += 1
    } else {
      stopped += 1
    }
  }
  return { running, stopped, abnormal }
})

// containerImages are the distinct images the containers of the site run.
const containerImages = computed(() => [
  ...new Set(appContainers.value.map((item) => item.image)),
])

// containerStateOf buckets a container for the filters and the stats.
function containerStateOf(item: ContainerSummary): 'running' | 'stopped' | 'abnormal' {
  if (item.state === 'running') {
    return item.health === 'unhealthy' ? 'abnormal' : 'running'
  }
  if (item.state === 'restarting' || item.state === 'paused') {
    return 'abnormal'
  }
  return 'stopped'
}

// theFilteredContainers applies the search and the two filters.
const theFilteredContainers = computed(() =>
  appContainers.value.filter((item) => {
    const needle = containerSearch.value.trim().toLowerCase()
    if (
      needle !== '' &&
      !`${item.name} ${item.image} ${item.service}`.toLowerCase().includes(needle)
    ) {
      return false
    }
    if (
      containerStateFilter.value !== 'all' &&
      containerStateOf(item) !== containerStateFilter.value
    ) {
      return false
    }
    if (containerImageFilter.value !== 'all' && item.image !== containerImageFilter.value) {
      return false
    }
    return true
  }),
)

// thePageContainers are the containers of the page the administrator reads.
const thePageContainers = computed(() => {
  const start = (containerPage.value - 1) * containerPageSize.value
  return theFilteredContainers.value.slice(start, start + containerPageSize.value)
})

const containerPageCount = computed(() =>
  Math.max(1, Math.ceil(theFilteredContainers.value.length / containerPageSize.value)),
)

// pageOf writes the page numbers of a pager.
function pageOf(current: number, count: number): number[] {
  if (count <= 7) {
    return Array.from({ length: count }, (_, index) => index + 1)
  }
  const start = Math.max(1, Math.min(current - 2, count - 4))
  return Array.from({ length: 5 }, (_, index) => start + index)
}

// A filter that changes resets the page.
watch([containerSearch, containerStateFilter, containerImageFilter, containerPageSize], () => {
  containerPage.value = 1
})

// uptimeOf writes the time a container has been up, the way a list reads
// it: days and hours, or minutes when it is young.
function uptimeOf(item: ContainerSummary): string {
  const started = item.started_at || item.created
  if (!started) {
    return '—'
  }
  const at = new Date(started).getTime()
  if (Number.isNaN(at)) {
    return '—'
  }
  const seconds = Math.max(0, Math.floor((now.value - at) / 1000))
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) {
    return `${days}天 ${hours}小时`
  }
  if (hours > 0) {
    return `${hours}小时 ${minutes}分钟`
  }
  return `${minutes}分钟`
}

// statePillLabel and statePillTone name the state of a container the way
// the pill of the list reads it.
function statePillLabel(state: string, health: string): string {
  if (health === 'unhealthy') {
    return '异常'
  }
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
    default:
      return state
  }
}

function statePillTone(state: string, health: string): 'ok' | 'bad' | 'warn' | 'off' {
  if (health === 'unhealthy') {
    return 'bad'
  }
  if (state === 'running') {
    return 'ok'
  }
  if (state === 'exited' || state === 'dead') {
    return 'off'
  }
  return 'warn'
}

// load reads everything the page shows.
async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    await loadApplication()
    await loadVersions()
    await loadAppContainers()
    const taskID = typeof route.query.deployment === 'string' ? route.query.deployment : ''
    if (taskID !== '') {
      await loadDeployment(taskID)
    }
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
  await loadNodes()
}

// onEvent follows the changes of the deployment the page shows.
//
// The page reads the state again instead of guessing it: a deployment is
// only ever written once, so a change of the state never starts a second
// deployment.
async function onEvent(event: ControlEvent): Promise<void> {
  const current = deployment.value
  if (!current || event.object_id !== current.id) {
    return
  }
  try {
    if (event.type === 'output') {
      await loadOutput(current.id)
      return
    }
    await loadDeployment(current.id)
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// refreshCurrentDeployment reads the deployment again.
//
// The browser reconnects the stream of the events on its own. A
// reconnection reads the state of the deployment, it never sends it again.
async function refreshCurrentDeployment(): Promise<void> {
  const current = deployment.value
  if (!current) {
    return
  }
  try {
    await loadDeployment(current.id)
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// conflictMessage explains a conflict of the API.
//
// The API answers a conflict for three reasons: a deployment without a
// preview, a configuration that differs from the preview and a page that
// is out of date. The page tells them apart by what it knows itself.
function conflictMessage(failure: unknown, phase: 'preview' | 'submit'): string {
  if (!(failure instanceof ApiError) || failure.code !== 'conflict') {
    return describeError(failure)
  }
  if (phase === 'preview') {
    return '应用已经有了更新的版本，本页面的编辑已保留。请重新加载最新配置后比较。'
  }
  return '部署请求与应用的当前状态不一致：应用可能已经被其他页面更新，或目标在预览之后发生了变化。本页面的编辑已保留，请重新预览并比较差异。'
}

// theNode names the node the application runs on.
const theNode = computed(() => nodes.value.find((node) => node.id === app.value?.target_id) ?? null)

// theServices groups the containers of the application by the service they
// belong to, the way the list of a node reads them.
const theServices = computed(() => {
  const groups = new Map<string, ContainerSummary[]>()
  for (const item of appContainers.value) {
    const key = item.service || item.name
    const group = groups.get(key) ?? []
    group.push(item)
    groups.set(key, group)
  }
  return [...groups.entries()].map(([service, items]) => ({
    service,
    containers: items.length,
    running: items.filter((item) => item.state === 'running').length,
    image: items[0]?.image ?? '',
    ports: items[0]?.ports ?? [],
    state:
      items.every((item) => item.state === 'running')
        ? 'running'
        : items.some((item) => item.state === 'running')
          ? 'partial'
          : 'stopped',
  }))
})

// yamlFacts counts what the configuration declares: the services and the
// networks of the document in the editor.
const yamlFacts = computed(() => {
  try {
    const document = parseYaml(yaml.value) as Record<string, Record<string, unknown>> | null
    if (!document || typeof document !== 'object') {
      return { services: 0, networks: 0, valid: true }
    }
    const services = document.services && typeof document.services === 'object' ? Object.keys(document.services).length : 0
    const networks = document.networks && typeof document.networks === 'object' ? Object.keys(document.networks).length : 0
    return { services, networks, valid: true }
  } catch {
    return { services: 0, networks: 0, valid: false }
  }
})

// configReadOnly is the face of the editor on the YAML tab: the document
// only edits when the administrator asked for the editor.
const configReadOnly = computed(() => !configEditing.value)

// startConfigEditing turns the editor of the YAML tab editable.
function startConfigEditing(): void {
  configEditing.value = true
}

// copyConfig writes the configuration to the clipboard.
async function copyConfig(): Promise<void> {
  try {
    await navigator.clipboard.writeText(yaml.value)
    configNotice.value = '配置已经复制。'
    setTimeout(() => {
      configNotice.value = ''
    }, 1800)
  } catch {
    configNotice.value = '浏览器未允许复制，请手动选择文本。'
  }
}

// formatConfig re-indents the document of the editor. A document that does
// not parse stays as it is and the reason shows beside the editor.
function formatConfig(): void {
  try {
    const document = parseYaml(yaml.value)
    yaml.value = stringifyYaml(document)
    configNotice.value = ''
  } catch (failure) {
    configNotice.value = `格式化失败：${failure instanceof Error ? failure.message : 'YAML 无法解析'}`
  }
}

// loadEvents reads the recent container events of the target, filtered by
// the namespace of the application.
async function loadEvents(): Promise<void> {
  const target = app.value?.target_id ?? ''
  if (target === '') {
    return
  }
  eventsError.value = ''
  try {
    events.value = await listNodeEvents(target, app.value?.namespace ?? '')
  } catch (failure) {
    eventsError.value = describeError(failure)
  }
}

// eventTone colors an action of a container: an end is red, a start is
// green, the rest is grey.
function eventTone(action: string): 'ok' | 'bad' | 'off' {
  if (action === 'start') return 'ok'
  if (action === 'die' || action === 'kill' || action === 'stop') return 'bad'
  return 'off'
}

// formatDate writes a time the way the header reads it.
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

// The events follow the page: they load when the tab opens and refresh
// every fifteen seconds while it stays open.
watch(tab, (value) => {
  if (value === 'events') {
    void loadEvents()
    if (eventsTimer === undefined) {
      eventsTimer = setInterval(() => {
        void loadEvents()
      }, 15 * 1000)
    }
  } else if (eventsTimer !== undefined) {
    clearInterval(eventsTimer)
    eventsTimer = undefined
  }
})
onBeforeUnmount(() => {
  if (eventsTimer !== undefined) {
    clearInterval(eventsTimer)
  }
})

// theSitePill names the state of the site, the way the header reads it.
const theSitePill = computed<{ label: string; tone: string }>(() => {
  let running = 0
  let abnormal = 0
  for (const item of appContainers.value) {
    const state = containerStateOf(item)
    if (state === 'running') {
      running += 1
    } else if (state === 'abnormal') {
      abnormal += 1
    }
  }
  if (abnormal > 0) {
    return { label: '部分异常', tone: 'warn' }
  }
  if (running > 0 && running === appContainers.value.length) {
    return { label: '运行中', tone: 'ok' }
  }
  return { label: '已停止', tone: 'off' }
})

// The menu of the further actions of the head of the page.
const openHeadMenu = ref(false)

// toggleHeadMenu opens or closes the menu of the head of the page.
function toggleHeadMenu(): void {
  openHeadMenu.value = !openHeadMenu.value
}

// The menu of the further operations of one container, the operation
// that runs and the reason one was refused.
const openOpsMenu = ref('')
const containerOpBusy = ref('')
const containerOpError = ref('')

// toggleOpsMenu opens or closes the menu of one container.
function toggleOpsMenu(id: string): void {
  openOpsMenu.value = openOpsMenu.value === id ? '' : id
}

// operateContainer runs one operation on one container of the site.
async function operateContainer(
  item: ContainerSummary,
  action: 'start' | 'stop' | 'restart',
): Promise<void> {
  const target = app.value?.target_id ?? ''
  if (containerOpBusy.value !== '' || target === '') {
    return
  }
  containerOpBusy.value = item.id
  containerOpError.value = ''
  openOpsMenu.value = ''
  try {
    await containerOp(target, item.id, action)
    const label = action === 'start' ? '启动' : action === 'stop' ? '停止' : '重启'
    notice.value = `${item.name} 的${label}已经完成。`
    await loadAppContainers()
  } catch (failure) {
    containerOpError.value = `${item.name}: ${describeError(failure)}`
  } finally {
    containerOpBusy.value = ''
  }
}

// restartAll restarts every container of the application, one container
// at a time. A container that refuses keeps going: the page reports the
// site as a whole.
async function restartAll(): Promise<void> {
  const target = app.value?.target_id ?? ''
  if (restarting.value || target === '' || appContainers.value.length === 0) {
    return
  }
  restarting.value = true
  restartError.value = ''
  const failures: string[] = []
  for (const item of appContainers.value) {
    try {
      await containerOp(target, item.id, 'restart')
    } catch (failure) {
      failures.push(`${item.name}: ${describeError(failure)}`)
    }
  }
  restarting.value = false
  confirmingRestart.value = false
  if (failures.length > 0) {
    restartError.value = failures.join('；')
  } else {
    notice.value = '重启已经完成。'
  }
  await load()
}

// runPreview asks for the impact of the configuration of the editor.
//
// The preview only reads: it shows the warnings of the configuration, the
// services the deployment removes and the risks the administrator has to
// confirm.
async function runPreview(): Promise<void> {
  if (!app.value || previewing.value) {
    return
  }
  previewing.value = true
  previewError.value = ''
  previewFailures.value = []
  try {
    const result = await previewDeployment(
      app.value.id,
      app.value.latest_version_id,
      currentConfig(),
    )
    preview.value = result
    previewedContent.value = contentKey()
    confirmedRemoval.value = false
    confirmedRisk.value = false
  } catch (failure) {
    preview.value = null
    previewedContent.value = ''
    previewError.value = conflictMessage(failure, 'preview')
    previewFailures.value = configFailures(failure).map(
      (field) => `${field.path}${field.line ? ` 第 ${field.line} 行` : ''}：${field.message}`,
    )
  } finally {
    previewing.value = false
  }
}

// idempotencyKeyFor returns the key of a submission.
//
// The key belongs to the content of the submission: a retry of the same
// content reuses the key, so the platform answers with the task of the
// first attempt instead of a second deployment. A content that changed
// gets a key of its own.
function idempotencyKeyFor(content: string): string {
  if (idempotencyKey.value === '' || keyedContent.value !== content) {
    idempotencyKey.value = newIdempotencyKey()
    keyedContent.value = content
  }
  return idempotencyKey.value
}

// canSubmit reports whether the deployment may be sent.
//
// A deployment needs a preview of the very configuration that is sent, the
// confirmations of the removals and of the risks, and no other deployment
// that is not finished.
const canSubmit = computed(() => {
  if (!app.value || !preview.value || submitting.value) {
    return false
  }
  if (isDeployBlocked(deployment.value?.state ?? null)) {
    return false
  }
  if (previewedContent.value !== contentKey()) {
    return false
  }
  if (preview.value.removed_services.length > 0 && !confirmedRemoval.value) {
    return false
  }
  if (preview.value.risk_warnings.length > 0 && !confirmedRisk.value) {
    return false
  }
  return true
})

// contentChanged reports whether the configuration changed after the
// preview.
const contentChanged = computed(
  () => preview.value !== null && previewedContent.value !== contentKey(),
)

// previewNotices are the warnings that ask for no confirmation.
//
// The API lists a risk among the warnings as well, so a risk is shown once,
// in the block that asks the administrator to confirm it.
const previewNotices = computed(() => {
  const current = preview.value
  if (!current) {
    return []
  }
  return current.warnings.filter((warning) => !current.risk_warnings.includes(warning))
})

// submit sends the deployment.
//
// The request names the preview the administrator saw, so the platform can
// tell whether the configuration is the one that was looked at.
async function submit(): Promise<void> {
  if (!app.value || !preview.value || submitting.value) {
    return
  }
  submitting.value = true
  submitError.value = ''
  notice.value = ''
  try {
    const task = await submitDeployment(app.value.id, {
      idempotency_key: idempotencyKeyFor(contentKey()),
      base_version_id: app.value.latest_version_id,
      preview_digest: preview.value.digest,
      inventory_revision: preview.value.inventory_revision,
      config: currentConfig(),
      confirmed_removal: {
        digest: preview.value.digest,
        services: preview.value.removed_services,
        inventory_revision: preview.value.inventory_revision,
      },
      accept_warnings: confirmedRisk.value,
    })
    preview.value = null
    previewedContent.value = ''
    confirmedRemoval.value = false
    confirmedRisk.value = false
    notice.value = '部署已经提交。'
    // The page follows the deployment from now on, so a reload of the page
    // finds it again.
    await router.replace({ path: route.path, query: { ...route.query, deployment: task.id } })
    await loadDeployment(task.id)
    await loadApplication()
    await loadVersions()
  } catch (failure) {
    submitError.value = conflictMessage(failure, 'submit')
  } finally {
    submitting.value = false
  }
}

// redeploy loads a version of the history into the editor.
//
// The version is not deployed right away: the impact of the configuration
// is shown first, so the administrator sees what the deployment does.
async function redeploy(versionID: string): Promise<void> {
  if (!app.value) {
    return
  }
  error.value = ''
  try {
    const version = await getVersion(app.value.id, versionID)
    yaml.value = version.config.yaml
    variables.value = { ...version.config.variables }
    drafts.value = {}
    preview.value = null
    previewedContent.value = ''
    tab.value = 'config'
    notice.value = '已载入该版本的配置，预览影响之后可以再次部署。'
    await runPreview()
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// reload reads the configuration of the server again.
//
// The edit of the administrator is not thrown away: the page shows that the
// server holds another configuration and lets the administrator decide.
async function reload(): Promise<void> {
  error.value = ''
  try {
    const loaded = await getApplication(String(route.params.id ?? ''))
    app.value = loaded
    await loadVersions()
    serverChanged.value =
      loaded.config.yaml !== yaml.value ||
      JSON.stringify(loaded.config.variables) !== JSON.stringify(variables.value)
    if (serverChanged.value) {
      notice.value = '服务端的配置与本页面的编辑不同。'
    }
  } catch (failure) {
    error.value = describeError(failure)
  }
}

// useServerConfig puts the configuration of the server into the editor.
//
// The edit of the administrator is only lost when he asks for it.
function useServerConfig(): void {
  if (!app.value) {
    return
  }
  yaml.value = app.value.config.yaml
  variables.value = { ...app.value.config.variables }
  drafts.value = {}
  preview.value = null
  previewedContent.value = ''
  serverChanged.value = false
  notice.value = '已经使用服务端的配置。'
}

// updateDraft remembers what the administrator typed for a variable.
function updateDraft(name: string, value: string): void {
  drafts.value = { ...drafts.value, [name]: value }
}

// applyVariable writes the value of a variable.
//
// An empty field keeps the value the variable has, so a value is never
// lost by a click that changed nothing.
function applyVariable(name: string): void {
  const value = drafts.value[name] ?? ''
  if (value === '') {
    return
  }
  variables.value = { ...variables.value, [name]: value }
  const rest = { ...drafts.value }
  delete rest[name]
  drafts.value = rest
}

// removeVariable removes a variable of the configuration.
function removeVariable(name: string): void {
  const rest = { ...variables.value }
  delete rest[name]
  variables.value = rest
  const remaining = { ...drafts.value }
  delete remaining[name]
  drafts.value = remaining
}

// addVariable adds a variable to the configuration.
function addVariable(): void {
  const name = newVariableName.value.trim()
  if (name === '') {
    return
  }
  variables.value = { ...variables.value, [name]: newVariableValue.value }
  newVariableName.value = ''
  newVariableValue.value = ''
}

// variableNames are the names of the variables of the configuration.
const variableNames = computed(() => Object.keys(variables.value).sort())

// observations are the states of the services of the application.
//
// The control端 reports the state of every service in the message of the
// outcome of the deployment, so the page shows them one by one.
// versionTime is the time of a version of the history.
function versionTime(versionID: string): string {
  const found = versions.value.find((version) => version.id === versionID)
  return found ? formatTimestamp(found.created_at) : '—'
}

// reconcile asks the node about the outcome of the deployment.
async function reconcile(): Promise<void> {
  const current = deployment.value
  if (!current || reconciling.value) {
    return
  }
  reconciling.value = true
  reconcileError.value = ''
  try {
    deployment.value = await reconcileDeployment(current.id)
  } catch (failure) {
    reconcileError.value = describeError(failure)
  } finally {
    reconciling.value = false
  }
}

// resolve ends a deployment whose outcome is unknown.
//
// The release waits for the confirmation of the node, so the page says so
// when the node does not answer yet.
async function resolve(): Promise<void> {
  const current = deployment.value
  if (!current || resolving.value) {
    return
  }
  if (!resolveStopped.value || resolveReason.value.trim() === '') {
    resolveError.value = '请确认执行端已经停止，并写下释放的原因。'
    return
  }
  resolving.value = true
  resolveError.value = ''
  try {
    deployment.value = await resolveDeployment(current.id, {
      expected_seq: current.last_seq,
      executor_stopped: true,
      reason: resolveReason.value.trim(),
    })
    notice.value = '释放已经提交。'
    resolveReason.value = ''
    resolveStopped.value = false
    resolveForm.value = false
  } catch (failure) {
    resolveError.value = describeError(failure)
  } finally {
    resolving.value = false
  }
}

onMounted(async () => {
  await load()
  ticker = setInterval(() => {
    now.value = Date.now()
  }, 30_000)
  subscription = subscribeEvents({
    onEvent: (event) => {
      void onEvent(event)
    },
    onOpen: () => {
      void refreshCurrentDeployment()
    },
  })
})

onBeforeUnmount(() => {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
  subscription?.close()
  subscription = null
})
</script>

<template>
  <AppShell>
    <p v-if="loading" class="muted">正在读取应用…</p>

    <div v-else-if="error && !app" class="alert" role="alert">
      <p>{{ error }}</p>
      <NButton   quaternary type="primary" @click="load">重试</NButton>
    </div>

    <template v-else-if="app">
      <div class="detail__head">
        <div class="head-copy">
          <div class="title-line">
            <h1 class="page-title">{{ app.name }}</h1>
            <span class="pill" :class="theSitePill.tone">
              <i aria-hidden="true"></i>{{ theSitePill.label }}
            </span>
          </div>
          <p class="head-meta">
            {{ app.mode === 'swarm' ? 'Swarm 项目' : 'Compose 项目' }} · 运行在
            {{ theNode?.hostname ?? app.target_id }} · 创建于 {{ formatDate(app.created_at) }}
          </p>
        </div>
        <div class="head-actions">
          <template v-if="confirmingRestart">
            <span class="confirm-inline">重启该站点的全部容器？</span>
            <button type="button" class="ghost-button" :disabled="restarting" @click="restartAll">
              {{ restarting ? '重启中…' : '确认重启' }}
            </button>
            <button type="button" class="ghost-button" @click="confirmingRestart = false">取消</button>
          </template>
          <template v-else>
            <button type="button" class="ghost-button" @click="confirmingRestart = true">重启</button>
            <button
              type="button"
              class="ghost-button"
              @click="
                tab = 'config';
                configEditing = true
              "
            >
              更新配置
            </button>
            <div class="ops-menu">
              <button
                type="button"
                class="ghost-button ops-head-toggle"
                :aria-expanded="openHeadMenu"
                @click="toggleHeadMenu"
              >
                更多
                <svg viewBox="0 0 24 24" aria-hidden="true">
                  <path d="m6 9 6 6 6-6" />
                </svg>
              </button>
              <div v-if="openHeadMenu" class="ops-list">
                <button
                  type="button"
                  @click="
                    openHeadMenu = false;
                    reload()
                  "
                >
                  重新加载
                </button>
              </div>
            </div>
          </template>
        </div>
      </div>

      <p v-if="error" class="alert" role="alert">{{ error }}</p>
      <p v-if="notice" class="notice">{{ notice }}</p>

      <nav class="tabs" aria-label="应用详情">
        <button
          v-for="item in theTabs"
          :key="item.key"
          type="button"
          class="tabs__tab"
          :class="{ 'tabs__tab--active': tab === item.key }"
          :aria-current="tab === item.key ? 'page' : undefined"
          @click="tab = item.key"
        >
          <svg class="tabs__icon" viewBox="0 0 24 24" aria-hidden="true">
            <path :d="item.icon" />
          </svg>
          {{ item.label }}
        </button>
      </nav>

      <section v-if="tab === 'overview'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <h2 class="panel__title">概况</h2>

        <dl class="facts">
          <div class="facts__row">
            <dt>模式</dt>
            <dd>{{ app.mode === 'swarm' ? 'Swarm' : 'Compose' }}</dd>
          </div>
          <div class="facts__row">
            <dt>命名空间</dt>
            <dd>{{ app.namespace }}</dd>
          </div>
          <div class="facts__row">
            <dt>所属节点</dt>
            <dd>
              {{ theNode?.hostname ?? app.target_id }}
              <span v-if="theNode" class="muted">· {{ theNode.id }}</span>
            </dd>
          </div>
          <div class="facts__row">
            <dt>描述</dt>
            <dd>{{ app.description || '—' }}</dd>
          </div>
          <div class="facts__row">
            <dt>服务端配置</dt>
            <dd>
              {{
                app.latest_version_id === ''
                  ? '服务端还没有保存过配置。'
                  : '服务端保存着最近提交的版本的配置。'
              }}
            </dd>
          </div>
          <div class="facts__row">
            <dt>最近提交</dt>
            <dd>
              <template v-if="app.latest_version_id !== ''">
                <code>{{ app.latest_version_id }}</code>
                <span class="muted"> · {{ versionTime(app.latest_version_id) }}</span>
              </template>
              <template v-else>—</template>
            </dd>
          </div>
          <div class="facts__row">
            <dt>最近成功</dt>
            <dd>
              <template v-if="app.successful_version_id !== ''">
                <code>{{ app.successful_version_id }}</code>
                <span class="muted"> · {{ versionTime(app.successful_version_id) }}</span>
              </template>
              <template v-else>—</template>
            </dd>
          </div>
          <div class="facts__row">
            <dt>实际运行状态</dt>
            <dd>
              <DeploymentStatus
                v-if="deployment"
                :state="deployment.state"
                :partial-update="deployment.partial_update"
                :rolled-back="deployment.rolled_back"
              />
              <span v-else>{{ theSitePill.label }} · {{ appContainers.length }} 个容器</span>
            </dd>
          </div>
        </dl>

        <div v-if="deployment && deployment.state === 'uncertain'" class="resolve">
          <h3 class="panel__title">人工核对</h3>
          <p class="muted">
            平台无法证明执行端已停止。只有在确认执行端不再继续操作之后，才能释放这个部署。
          </p>
          <div class="resolve__actions">
            <NButton   quaternary type="primary" :disabled="reconciling" @click="reconcile">
              {{ reconciling ? '核对中…' : '重新核对' }}
            </NButton>
            <NButton   quaternary type="primary" @click="resolveForm = true">
              人工核对并释放
            </NButton>
          </div>
          <p v-if="reconcileError" class="alert" role="alert">{{ reconcileError }}</p>

          <form v-if="resolveForm" class="form" novalidate @submit.prevent="resolve">
            <div class="field">
              <label class="field__check">
                <input v-model="resolveStopped" type="checkbox" name="resolve-stopped" />
                <span>我已确认执行端不再继续操作</span>
              </label>
            </div>
            <div class="field">
              <label class="field__label" for="resolve-reason">释放原因</label>
              <input
                id="resolve-reason"
                v-model="resolveReason"
                class="field__input"
                type="text"
                name="resolve-reason"
                required
              />
            </div>
            <p v-if="resolveError" class="alert" role="alert">{{ resolveError }}</p>
            <div class="resolve__actions">
              <NButton  attr-type="submit" type="primary" :disabled="resolving">释放</NButton>
              <NButton   quaternary type="primary" @click="resolveForm = false">取消</NButton>
            </div>
          </form>
        </div>
      </section>

      <section v-else-if="tab === 'containers'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <div class="stat-grid">
          <div class="stat-card">
            <span class="stat-icon total">
              <svg viewBox="0 0 24 24">
                <path d="M12 3 3 8v8l9 5 9-5V8L12 3ZM3 8l9 5 9-5M12 13v8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
              </svg>
            </span>
            <span class="stat-copy"><strong>{{ appContainers.length }}</strong><small>容器总数</small></span>
          </div>
          <div class="stat-card">
            <span class="stat-icon run">
              <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="currentColor" /><path d="m10 8.5 6 3.5-6 3.5V8.5Z" fill="#fff" stroke="none" /></svg>
            </span>
            <span class="stat-copy"><strong>{{ theContainerStats.running }}</strong><small>运行中</small></span>
          </div>
          <div class="stat-card">
            <span class="stat-icon stop">
              <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="currentColor" /><path d="M9.5 8.5v7M14.5 8.5v7" stroke="#fff" stroke-width="2" stroke-linecap="round" /></svg>
            </span>
            <span class="stat-copy"><strong>{{ theContainerStats.stopped }}</strong><small>已停止</small></span>
          </div>
          <div class="stat-card">
            <span class="stat-icon bad">
              <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="currentColor" /><path d="M12 7.4v5.4" stroke="#fff" stroke-width="2" stroke-linecap="round" /><circle cx="12" cy="16.4" r="1.1" fill="#fff" /></svg>
            </span>
            <span class="stat-copy"><strong>{{ theContainerStats.abnormal }}</strong><small>异常</small></span>
          </div>
        </div>

        <p v-if="appContainersError" class="alert" role="alert">{{ appContainersError }}</p>
        <p v-if="containerOpError" class="alert" role="alert">{{ containerOpError }}</p>

        <div class="list-toolbar">
          <div class="toolbar-fields">
            <label class="search">
              <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m20 20-3.8-3.8" /></svg>
              <input v-model="containerSearch" type="search" placeholder="搜索容器名称、镜像…" />
            </label>
            <label class="filter">
              <span>状态：</span>
              <select v-model="containerStateFilter" class="select" name="container-state">
                <option value="all">全部</option>
                <option value="running">运行中</option>
                <option value="stopped">已停止</option>
                <option value="abnormal">异常</option>
              </select>
            </label>
            <label class="filter">
              <span>镜像：</span>
              <select v-model="containerImageFilter" class="select" name="container-image">
                <option value="all">全部</option>
                <option v-for="image in containerImages" :key="image" :value="image">{{ image }}</option>
              </select>
            </label>
          </div>
          <span class="toolbar-note">{{ theFilteredContainers.length }} / {{ appContainers.length }} 个容器</span>
        </div>

        <div v-if="theFilteredContainers.length === 0" class="empty">
          <p v-if="appContainers.length > 0">没有符合筛选条件的容器。</p>
          <p v-else>这个应用在目标节点上还没有容器。部署之后容器会出现在这里。</p>
        </div>

        <div v-else class="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">容器名称</th>
                <th scope="col">状态</th>
                <th scope="col">镜像</th>
                <th scope="col">所属服务</th>
                <th scope="col">创建时间</th>
                <th scope="col">运行时长</th>
                <th scope="col" class="actions-col">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="item in thePageContainers" :key="item.id">
                <td class="name-cell">
                  <RouterLink
                    class="container-link"
                    :to="`/applications/${encodeURIComponent(app.id)}/containers/${encodeURIComponent(item.id)}`"
                  >
                    {{ item.name }}
                  </RouterLink>
                </td>
                <td>
                  <span class="pill" :class="statePillTone(item.state, item.health)">
                    <i aria-hidden="true"></i>{{ statePillLabel(item.state, item.health) }}
                  </span>
                </td>
                <td class="image-cell" :title="item.image">{{ item.image }}</td>
                <td>
                  <span class="service-link">{{ item.service || '—' }}</span>
                </td>
                <td class="mono">{{ formatDate(item.created ?? '') }}</td>
                <td class="mono">{{ uptimeOf(item) }}</td>
                <td class="actions-col">
                  <div class="ops-menu">
                      <button
                        type="button"
                        class="text-button ops-toggle"
                        :aria-expanded="openOpsMenu === item.id"
                        @click="toggleOpsMenu(item.id)"
                      >
                        更多
                        <svg viewBox="0 0 24 24" aria-hidden="true">
                          <path d="m6 9 6 6 6-6" />
                        </svg>
                      </button>
                      <div v-if="openOpsMenu === item.id" class="ops-list">
                        <button
                          v-if="item.state !== 'running'"
                          type="button"
                          :disabled="containerOpBusy !== ''"
                          @click="operateContainer(item, 'start')"
                        >
                          启动
                        </button>
                        <button
                          v-if="item.state === 'running'"
                          type="button"
                          :disabled="containerOpBusy !== ''"
                          @click="operateContainer(item, 'restart')"
                        >
                          重启
                        </button>
                        <button
                          v-if="item.state === 'running'"
                          type="button"
                          class="danger"
                          :disabled="containerOpBusy !== ''"
                          @click="operateContainer(item, 'stop')"
                        >
                          停止
                        </button>
                      </div>
                    </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="table-foot">
          <span class="muted-cell">共 {{ theFilteredContainers.length }} 条</span>
          <div class="pager">
            <button type="button" class="pager-step" :disabled="containerPage <= 1" @click="containerPage -= 1">‹</button>
            <button
              v-for="p in pageOf(containerPage, containerPageCount)"
              :key="p"
              type="button"
              class="pager-page"
              :class="{ active: p === containerPage }"
              :aria-pressed="p === containerPage"
              @click="containerPage = p"
            >
              {{ p }}
            </button>
            <button type="button" class="pager-step" :disabled="containerPage >= containerPageCount" @click="containerPage += 1">›</button>
            <select v-model.number="containerPageSize" class="select" name="container-page-size" aria-label="每页条数">
              <option :value="10">10 条/页</option>
              <option :value="20">20 条/页</option>
              <option :value="50">50 条/页</option>
            </select>
          </div>
        </div>
      </section>

      <!-- Services -->
      <section v-else-if="tab === 'services'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <h2 class="panel__title">服务</h2>
        <p class="muted">
          {{ app.mode === 'swarm' ? 'Swarm 服务与它们的副本。' : 'Compose 服务与它们的容器。' }}
        </p>

        <div v-if="theServices.length === 0" class="empty">
          <p>这个站点还没有可展示的服务。部署之后服务会出现在这里。</p>
        </div>

        <div v-else class="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">服务</th>
                <th scope="col">容器数</th>
                <th scope="col">运行数</th>
                <th scope="col">状态</th>
                <th scope="col">镜像</th>
                <th scope="col">端口</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="service in theServices" :key="service.service">
                <td class="name-cell">
                  <strong>{{ service.service }}</strong>
                </td>
                <td class="mono">{{ service.containers }}</td>
                <td class="mono">{{ service.running }}</td>
                <td>
                  <span class="pill" :class="service.state === 'running' ? 'ok' : service.state === 'partial' ? 'warn' : 'off'">
                    <i aria-hidden="true"></i>
                    {{ service.state === 'running' ? '运行中' : service.state === 'partial' ? '部分运行' : '已停止' }}
                  </span>
                </td>
                <td class="image-cell" :title="service.image">{{ service.image || '—' }}</td>
                <td class="ports-cell">
                  <span v-if="!service.ports?.length" class="muted">—</span>
                  <span v-else>{{ service.ports.map((port) => `${port.public_port}:${port.private_port}`).join(', ') }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- Events -->
      <section v-else-if="tab === 'events'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <h2 class="panel__title">事件日志</h2>
        <p class="muted">这个站点的容器最近发生的事件，每 15 秒刷新一次。</p>

        <p v-if="eventsError" class="alert" role="alert">{{ eventsError }}</p>

        <div v-else-if="events.length === 0" class="empty">
          <p>还没有事件。容器启动、退出或被操作时，事件会出现在这里。</p>
        </div>

        <div v-else class="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">时间</th>
                <th scope="col">容器</th>
                <th scope="col">动作</th>
                <th scope="col">服务</th>
                <th scope="col">退出码</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(event, index) in events" :key="event.container_id + '-' + index">
                <td class="mono">{{ formatTimestamp(event.occurred_at) }}</td>
                <td class="mono">{{ event.container_id.slice(0, 12) }}</td>
                <td>
                  <span class="pill" :class="eventTone(event.action)">
                    <i aria-hidden="true"></i>{{ event.action }}
                  </span>
                </td>
                <td>{{ event.service || '—' }}</td>
                <td class="mono">{{ event.exit_code ?? '—' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section v-else-if="tab === 'logs'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <h2 class="panel__title">部署输出</h2>
        <p class="muted">这是部署任务的执行输出（整站），不是某个容器的运行日志。容器日志在「概况」页签按容器查看。</p>

        <p v-if="!deployment" class="empty">
          在部署记录里选择一次部署，查看它的执行输出。
        </p>
        <template v-else>
          <p class="muted">
            部署 {{ deployment.id }} 的输出{{ deployment.finished_at ? `，结束于 ${formatTimestamp(deployment.finished_at)}` : '' }}。
          </p>
          <pre v-if="output !== ''" class="logs">{{ output }}</pre>
          <p v-else class="empty">这次部署还没有输出。</p>
        </template>
      </section>

      <section v-else-if="tab === 'config'" class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <div class="config-head">
          <h2 class="panel__title config-title">
            <span class="config-title__icon" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <path d="M7 4h7l4 4v12H7V4Z" />
                <path d="M14 4v4h4M10 12h5M10 16h5" />
              </svg>
            </span>
            YAML 配置
          </h2>
          <div class="config-head__actions">
            <template v-if="!configEditing">
              <button type="button" class="edit-link" @click="startConfigEditing">
                <span>编辑配置</span>
                <svg viewBox="0 0 24 24" aria-hidden="true">
                  <path d="m14.5 5.5 4 4L9 19H5v-4L14.5 5.5Z" />
                </svg>
              </button>
            </template>
            <template v-else>
              <button type="button" class="edit-link" :disabled="!yamlFacts.valid" @click="formatConfig">格式化</button>
              <button type="button" class="edit-link" @click="configEditing = false">完成编辑</button>
            </template>
          </div>
        </div>
        <div class="config-grid">
          <div class="config-editor">
            <button type="button" class="copy-float" @click="copyConfig">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <rect x="8" y="8" width="11" height="12" rx="2" />
                <path d="M5 16V6a2 2 0 0 1 2-2h8" />
              </svg>
              复制
            </button>
            <YamlEditor v-model="yaml" :read-only="configReadOnly" />
            <p v-if="configNotice" class="config-notice">{{ configNotice }}</p>
          </div>
          <aside class="config-facts">
            <div class="config-facts__head">
              <span class="config-facts__icon" aria-hidden="true">
                <svg viewBox="0 0 24 24">
                  <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.8" />
                  <path d="M12 8v4.6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
                  <circle cx="12" cy="15.8" r="1" fill="currentColor" />
                </svg>
              </span>
              <div>
                <h3>配置说明</h3>
                <p>这是当前站点的 Compose 配置，支持在线编辑并保存。</p>
              </div>
            </div>

            <h4 class="config-facts__subtitle">基本信息</h4>
            <dl class="config-facts__list">
              <div><dt>部署类型</dt><dd>{{ app.mode === 'swarm' ? 'Swarm' : 'Compose' }}</dd></div>
              <div><dt>服务数量</dt><dd>{{ yamlFacts.valid ? yamlFacts.services : '—' }}</dd></div>
              <div><dt>网络数量</dt><dd>{{ yamlFacts.valid ? yamlFacts.networks : '—' }}</dd></div>
            </dl>

            <div class="config-facts__divider"></div>

            <dl class="config-facts__list">
              <div><dt>更新时间</dt><dd>{{ versionTime(app.latest_version_id) }}</dd></div>
              <div><dt>修改人</dt><dd>admin</dd></div>
            </dl>
          </aside>
        </div>

        <div class="config-banner">
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.8" />
            <path d="M12 8v4.6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
            <circle cx="12" cy="15.8" r="1" fill="currentColor" />
          </svg>
          修改配置后，点击头部「更新配置」将会重新部署站点
        </div>

        <template v-if="configEditing || preview !== null">
        <div v-if="serverChanged" class="notice">
          <p>服务端的配置与本页面的编辑不同。本页面的编辑一直保留着。</p>
          <div class="resolve__actions">
            <NButton   quaternary type="primary" @click="useServerConfig">
              使用服务端的配置
            </NButton>
            <NButton   quaternary type="primary" @click="serverChanged = false">
              保留我的编辑
            </NButton>
          </div>
        </div>

        <h3 class="panel__title">变量</h3>
        <p class="muted">变量的值不会显示在页面上，输入新的值会覆盖旧的值。</p>

        <p v-if="variableNames.length === 0" class="empty">这个应用还没有变量。</p>
        <ul v-else class="variables">
          <li v-for="name in variableNames" :key="name" class="variables__row">
            <span class="variables__name"><code>{{ name }}</code></span>
            <span class="muted">{{ variables[name] === '' ? '未设置' : '已设置' }}</span>
            <input
              class="field__input variables__input"
              type="password"
              autocomplete="new-password"
              placeholder="新的值"
              :value="drafts[name] ?? ''"
              @input="updateDraft(name, ($event.target as HTMLInputElement).value)"
              @change="applyVariable(name)"
            />
            <NButton   quaternary type="primary" @click="removeVariable(name)">删除</NButton>
          </li>
        </ul>

        <form class="form form--inline" novalidate @submit.prevent="addVariable">
          <div class="field">
            <label class="field__label" for="variable-name">变量名</label>
            <input
              id="variable-name"
              v-model="newVariableName"
              class="field__input"
              type="text"
              name="variable-name"
            />
          </div>
          <div class="field">
            <label class="field__label" for="variable-value">变量的值</label>
            <input
              id="variable-value"
              v-model="newVariableValue"
              class="field__input"
              type="password"
              autocomplete="new-password"
              name="variable-value"
            />
          </div>
          <NButton quaternary type="primary" attr-type="submit">添加变量</NButton>
        </form>

        <h3 class="panel__title">影响预览</h3>
        <p class="muted">部署之前先看这次改动会带来什么。</p>

        <div class="actions">
          <NButton   quaternary type="primary" :disabled="previewing" @click="runPreview">
            {{ previewing ? '预览中…' : '预览影响' }}
          </NButton>
        </div>

        <p v-if="previewError" class="alert" role="alert">
          <span>{{ previewError }}</span>
          <ul v-if="previewFailures.length > 0" class="alert__list">
            <li v-for="failure in previewFailures" :key="failure">{{ failure }}</li>
          </ul>
        </p>

        <div v-if="preview" class="preview">
          <ul v-if="previewNotices.length > 0" class="preview__list">
            <li v-for="warning in previewNotices" :key="warning">{{ warning }}</li>
          </ul>

          <div v-if="preview.removed_services.length > 0" class="preview__block">
            <p>这次部署会删除这些服务：</p>
            <ul class="preview__list">
              <li v-for="service in preview.removed_services" :key="service">{{ service }}</li>
            </ul>
            <label class="field__check">
              <input v-model="confirmedRemoval" type="checkbox" name="confirm-removal" />
              <span>我确认删除这些服务</span>
            </label>
          </div>

          <div v-if="preview.risk_warnings.length > 0" class="preview__block">
            <p>这次部署有风险：</p>
            <ul class="preview__list">
              <li v-for="warning in preview.risk_warnings" :key="warning">{{ warning }}</li>
            </ul>
            <label class="field__check">
              <input v-model="confirmedRisk" type="checkbox" name="confirm-risk" />
              <span>我已知晓这些风险</span>
            </label>
          </div>

          <p v-if="preview.warnings.length === 0 && preview.removed_services.length === 0" class="muted">
            这次改动没有风险提示，也不会删除服务。
          </p>
        </div>

        <p v-if="submitError" class="alert" role="alert">{{ submitError }}</p>

        <p v-if="isDeployBlocked(deployment?.state ?? null)" class="muted">
          这个应用有一个还没有结束的部署，结束后才能再次提交。
        </p>
        <p v-else-if="!preview" class="muted">
          请先点击「预览影响」确认这次改动，然后才能保存并部署。
        </p>
        <p v-else-if="contentChanged" class="muted">
          配置在预览之后发生了变化，请重新预览。
        </p>

        <div class="actions">
          <NButton
            
             type="primary"
            :disabled="!canSubmit"
            @click="submit"
          >
            {{ submitting ? '提交中…' : '保存并部署' }}
          </NButton>
        </div>
        </template>
      </section>

      <section v-else class="rounded-xl border border-[#e6ecf7] bg-white p-6 shadow-sm">
        <h2 class="panel__title">部署记录</h2>
        <DeploymentHistory
          :versions="versions"
          :latest-version-id="app.latest_version_id"
          :successful-version-id="app.successful_version_id"
          :busy="submitting"
          @redeploy="redeploy"
        />
      </section>
    </template>

    <!-- The detail of a container opens in the page of the site -->
  </AppShell>
</template>

<style scoped>
.detail__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}

/* The picker of the container whose logs the page shows. The identifier
 * travels beside it, so a copy for an API call is at hand. */
.log-picker {
  display: flex;
  gap: var(--space-4);
  align-items: end;
  flex-wrap: wrap;
}

.log-picker .field {
  min-width: 18rem;
}

.log-picker__id {
  font-size: 0.71875rem;
  padding-bottom: var(--space-2);
}

.log-picker__id code {
  color: var(--color-ink-soft);
}

/* The classes the template shares with the rest of the platform. They live
 * here because the global sheet moved to Tailwind and the library. */
.muted {
  color: #667085;
}

.empty {
  padding: 1.5rem 0;
  color: #667085;
}

.panel__title {
  font-size: 1rem;
  font-weight: 600;
  margin-bottom: 0.75rem;
}

.panel__title + .muted,
.panel__title ~ p.muted {
  margin-bottom: 0.75rem;
}

.alert {
  display: grid;
  gap: 0.5rem;
  padding: 13px 16px;
  border-radius: 8px;
  background: #fdeaea;
  color: #e02e2e;
  font-size: 0.75rem;
  line-height: 1.7;
}

.alert__list {
  padding-left: 1rem;
}

.notice {
  padding: 13px 16px;
  border-radius: 8px;
  background: #fffaeb;
  color: #84510a;
  font-size: 0.75rem;
  line-height: 1.7;
}

.form {
  display: grid;
  gap: 1rem;
}

.field {
  display: grid;
  gap: 0.25rem;
  align-content: start;
}

.field__label {
  font-size: 0.75rem;
  font-weight: 500;
  color: #344054;
}

.field__input {
  height: 38px;
  padding: 0 12px;
  border: 1px solid #d3dcec;
  border-radius: 8px;
  background: #ffffff;
  color: var(--color-ink);
  font: inherit;
  transition: border-color 140ms ease, box-shadow 140ms ease;
}

textarea.field__input,
select.field__input {
  height: auto;
  min-height: 38px;
  padding: 8px 12px;
}

.field__input:focus-visible {
  outline: none;
  border-color: #025ffb;
  box-shadow: 0 0 0 3px rgb(2 95 251 / 10%);
}

.field__input:disabled {
  background: #f2f6fe;
  color: #667085;
}

.field__hint {
  color: #667085;
  font-size: 0.6875rem;
}

.field__error {
  color: #e02e2e;
  font-size: 0.6875rem;
}

.field__check {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  font-size: 0.875rem;
}

.tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 1.5rem;
  border-bottom: 1px solid #e6ecf7;
  overflow-x: auto;
}

.tabs__tab {
  font: inherit;
  background: transparent;
  border: none;
  border-bottom: 2px solid transparent;
  padding: 0 1px 13px;
  color: #667085;
  font-size: 0.8125rem;
  white-space: nowrap;
  cursor: pointer;
  transition: color 120ms ease;
}

.tabs__tab:hover {
  color: #101828;
}

.tabs__tab--active {
  color: #025ffb;
  border-bottom-color: #025ffb;
  font-weight: 600;
}

.facts {
  display: grid;
  gap: 0.75rem;
  margin: 0;
}

.facts__row {
  display: grid;
  grid-template-columns: 8rem 1fr;
  gap: 0.75rem;
  align-items: baseline;
}

.facts__row dt {
  color: #667085;
  font-size: 0.875rem;
}

.facts__row dd {
  margin: 0;
  overflow-wrap: anywhere;
}

.observations,
.variables {
  display: grid;
  gap: 0.5rem;
  padding: 0;
  margin: 0.75rem 0 0;
  list-style: none;
}

.observations li {
  padding: 0.5rem 0;
  border-bottom: 1px solid #e6ecf7;
}

.variables__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.75rem;
  padding: 0.5rem 0;
  border-bottom: 1px solid #e6ecf7;
}

.variables__name {
  min-width: 8rem;
}

.variables__input {
  flex: 1 1 12rem;
  max-width: 20rem;
}

.logs {
  margin: 0.75rem 0;
  padding: 1rem;
  border: 1px solid #e6ecf7;
  border-radius: 8px;
  background: #f2f6fe;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12.8px;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  max-height: 26rem;
  overflow: auto;
}

.state {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 3px 9px;
  border-radius: 999px;
  background: #f2f4f7;
  color: #667085;
  font-size: 0.6875rem;
  font-weight: 500;
  white-space: nowrap;
}

.state::before {
  content: "";
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: currentcolor;
  flex: none;
}

.state--online {
  background: #e8f1fe;
  color: #025ffb;
}

.state--offline {
  background: #fffaeb;
  color: #b54708;
}

.actions {
  display: flex;
  gap: 0.75rem;
  margin-top: 1rem;
}

.preview {
  margin-top: 1rem;
  padding: 1rem;
  border: 1px solid #e6ecf7;
  border-radius: 8px;
  background: #f2f6fe;
}

.preview__list {
  margin: 0.25rem 0;
  padding-left: 1.25rem;
}

.preview__block p {
  font-weight: 600;
}

.resolve {
  margin-top: 1.5rem;
  padding: 1rem;
  border: 1px dashed #d3dcec;
  border-radius: 8px;
  background: #f2f6fe;
}

.resolve__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 0.75rem;
  margin-top: 0.75rem;
}

.form--inline {
  display: flex;
  flex-wrap: wrap;
  align-items: end;
  gap: 0.75rem;
  max-width: none;
}

.form--inline > NButton {
  margin-bottom: 0.25rem;
}

.detail__head .text-xl {
  margin-bottom: 0;
}

/* The head of the YAML tab: the actions at the right of the title. */
.config-head {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 14px;
  margin-bottom: 10px;
}

.config-head__actions {
  display: inline-flex;
  align-items: center;
  gap: 14px;
}

.edit-link {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: none;
  background: none;
  padding: 0;
  color: #2f6fe4;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
}

.edit-link:hover:not(:disabled) {
  text-decoration: underline;
}

.edit-link:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.edit-link svg {
  width: 14px;
  height: 14px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.config-notice {
  margin: 8px 0 0;
  font-size: 12px;
  color: #2f6fe4;
}

.config-banner {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 14px;
  padding: 11px 16px;
  border-radius: 10px;
  background: #f0f5ff;
  color: #1d4fd8;
  font-size: 12.5px;
}

.config-banner svg {
  width: 15px;
  height: 15px;
  flex: none;
}

.config-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.6fr) minmax(240px, 0.9fr);
  gap: 18px;
  align-items: start;
}

.config-editor {
  position: relative;
  min-width: 0;
}

.copy-float {
  position: absolute;
  right: 14px;
  top: 14px;
  z-index: 3;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 30px;
  padding: 0 12px;
  border: 1px solid #d3dcec;
  border-radius: 8px;
  background: #fff;
  color: #132a56;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
  box-shadow: 0 6px 16px -10px rgb(20 52 110 / 45%);
}

.copy-float:hover {
  border-color: #9dbdf5;
  color: #0255f0;
}

.copy-float svg {
  width: 13px;
  height: 13px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}

@media (max-width: 980px) {
  .config-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

/* The tabs carry a line icon beside the label, the way the prototype
   draws them. */
.tabs__tab {
  display: inline-flex;
  align-items: center;
  gap: 7px;
}

.tabs__icon {
  width: 15px;
  height: 15px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
  opacity: 0.85;
}

/* The head of a site: the identity on the left, the actions on the right. */
.head-copy {
  min-width: 0;
}

.title-line {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.head-meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin: 8px 0 0;
  font-size: 12.5px;
  color: #667085;
}

.chip {
  display: inline-flex;
  align-items: center;
  border-radius: 8px;
  padding: 2px 10px;
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

.head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.confirm-inline {
  color: #b42318;
  font-size: 12.5px;
  font-weight: 600;
}

.ghost-button {
  height: 34px;
  padding: 0 14px;
  border: 1px solid #d3dcec;
  border-radius: 9px;
  background: #fff;
  color: #344054;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
  white-space: nowrap;
}

.ghost-button:hover:not(:disabled) {
  border-color: #9dbdf5;
  color: #0255f0;
}

.ghost-button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

/* The four readings of the containers tab. */
.stat-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 18px;
  margin-bottom: 18px;
}

.stat-card {
  display: flex;
  align-items: center;
  gap: 16px;
  min-width: 0;
  padding: 20px 22px;
  border: 1px solid #e9eff9;
  border-radius: 14px;
  background: #fff;
  box-shadow: 0 10px 24px -26px rgb(20 52 110 / 45%);
}

.stat-icon {
  width: 56px;
  height: 56px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #146ef5;
}

.stat-icon svg {
  width: 26px;
  height: 26px;
}

.stat-icon.run {
  background: #e5f9ef;
  color: #0bbf7a;
}

.stat-icon.stop {
  background: #feeceb;
  color: #f04438;
}

.stat-icon.bad {
  background: #eef4ff;
  color: #2f6fe4;
}

.stat-copy {
  display: grid;
  gap: 1px;
  min-width: 0;
}

.stat-copy strong {
  color: #132a56;
  font-size: 30px;
  font-weight: 800;
  line-height: 1.15;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
}

.stat-copy small {
  color: #6e80a0;
  font-size: 13.5px;
  white-space: nowrap;
}

/* The toolbar of the container list. */
.list-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 14px;
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
  width: 280px;
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

.filter {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: #475467;
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

.toolbar-note {
  color: #8ba0bf;
  font-size: 12.5px;
  white-space: nowrap;
}

/* The table of the containers: the header of the list sits on a light
   band, the rows are white and hover. */
.table-scroll {
  overflow-x: auto;
  border: 1px solid #eef2fa;
  border-radius: 12px;
}

.table-scroll table {
  width: 100%;
  margin: 0;
  border-collapse: collapse;
}

.table-scroll thead th {
  background: #f8f9fc;
  border-bottom: 1px solid #e6ecf7;
  padding: 12px 14px;
  font-size: 12.5px;
  font-weight: 600;
  color: #475467;
  text-align: left;
  white-space: nowrap;
}

.table-scroll tbody td {
  padding: 14px;
  border-bottom: 1px solid #eef2fa;
  font-size: 13px;
  color: #132a56;
  vertical-align: middle;
}

.table-scroll tbody tr:last-child td {
  border-bottom: none;
}

.table-scroll tbody tr:hover {
  background: #f8fbff;
}

.name-cell strong {
  font-size: 13px;
}

.container-link {
  color: #2f6fe4;
  font-weight: 500;
  text-decoration: none;
}

.container-link:hover {
  text-decoration: underline;
}

.service-link {
  color: #2f6fe4;
}

.row-actions {
  display: inline-flex;
  align-items: center;
  gap: 12px;
  white-space: nowrap;
}

.text-button {
  appearance: none;
  border: none;
  background: none;
  padding: 0;
  font-size: 13px;
  font-weight: 500;
  color: #2f6fe4;
  cursor: pointer;
  text-decoration: none;
}

.text-button:hover {
  text-decoration: underline;
}

.actions-col {
  width: 1%;
  text-align: right;
}

.table-scroll td.actions-col {
  text-align: right;
}

.mono {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.image-cell {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  font-size: 12px;
}

.ports-cell {
  max-width: 200px;
  font-family: var(--font-mono, monospace);
  font-size: 11.5px;
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

.pill.bad {
  background: #fef3f2;
  color: #b42318;
}

/* The foot of the table. */
.table-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-top: 14px;
}

.muted-cell {
  color: #8ba0bf;
  font-size: 12.5px;
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

@media (max-width: 1100px) {
  .stat-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

/* The title of the YAML panel carries the icon of the prototype. */
.config-title {
  display: inline-flex;
  align-items: center;
  gap: 12px;
}

.config-title__icon {
  width: 42px;
  height: 42px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #e9f2ff;
  color: #2f6fe4;
}

.config-title__icon svg {
  width: 20px;
  height: 20px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.config-head {
  justify-content: space-between;
}

/* The facts panel: the tinted head with the icon, the basic facts, the
   divider and the facts of the update. */
.config-facts {
  border: 1px solid #e9eff9;
  border-radius: 14px;
  background: #fff;
  padding: 18px;
}

.config-facts__head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin-bottom: 16px;
  padding: 14px 16px;
  border-radius: 12px;
  background: #f0f5ff;
}

.config-facts__icon {
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 50%;
  background: #fff;
  color: #2f6fe4;
}

.config-facts__icon svg {
  width: 18px;
  height: 18px;
}

.config-facts__head h3 {
  margin: 0 0 4px;
  color: #132a56;
  font-size: 15px;
  font-weight: 700;
}

.config-facts__head p {
  margin: 0;
  color: #475467;
  font-size: 12.5px;
  line-height: 1.6;
}

.config-facts__subtitle {
  margin: 0 0 10px;
  color: #132a56;
  font-size: 13px;
  font-weight: 700;
}

.config-facts__divider {
  height: 1px;
  margin: 14px 0;
  background: #eef2fa;
}

.config-facts__list div {
  grid-template-columns: 96px 1fr;
}

/* The menu of the further operations of one container. */
.ops-menu {
  position: relative;
  display: inline-flex;
}

.ops-toggle {
  display: inline-flex;
  align-items: center;
  gap: 3px;
}

.ops-toggle svg {
  width: 12px;
  height: 12px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.ops-list {
  position: absolute;
  right: 0;
  top: calc(100% + 6px);
  z-index: 5;
  min-width: 104px;
  display: grid;
  padding: 6px;
  border: 1px solid #e6ecf7;
  border-radius: 10px;
  background: #fff;
  box-shadow: 0 12px 28px -14px rgb(20 52 110 / 45%);
}

.ops-list button {
  appearance: none;
  border: none;
  border-radius: 7px;
  background: none;
  padding: 7px 12px;
  text-align: left;
  font-size: 12.5px;
  font-weight: 500;
  color: #344054;
  cursor: pointer;
  white-space: nowrap;
}

.ops-list button:hover:not(:disabled) {
  background: #f2f7ff;
  color: #0255f0;
}

.ops-list button.danger {
  color: #d92d20;
}

.ops-list button.danger:hover:not(:disabled) {
  background: #fef3f2;
  color: #b42318;
}

.ops-list button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

/* The tabs of the prototype: a wide row with a line icon, the active label
   in ink, a blue icon and a blue underline that sits on the border. */
nav.tabs {
  gap: 32px;
  margin: 22px 0 24px;
  padding: 0;
}

nav.tabs .tabs__tab {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 42px;
  margin-bottom: -1px;
  padding: 0 2px;
  font-size: 14px;
  font-weight: 500;
  color: #667085;
  border-bottom: 2px solid transparent;
}

nav.tabs .tabs__tab:hover {
  color: #132a56;
}

nav.tabs .tabs__tab--active {
  color: #132a56;
  font-weight: 600;
  border-bottom-color: #2f6ef0;
}

nav.tabs .tabs__icon {
  width: 16px;
  height: 16px;
  color: #98a2b3;
}

nav.tabs .tabs__tab--active .tabs__icon {
  color: #2f6ef0;
}

/* The rows of the facts are a label column and a value column. */
.config-facts__list div {
  display: grid;
  grid-template-columns: 96px 1fr;
  gap: 10px;
  align-items: baseline;
}

.config-facts__list dt {
  font-size: 12.5px;
  color: #667085;
}

.config-facts__list dd {
  margin: 0;
  font-size: 13px;
  color: #132a56;
  overflow-wrap: anywhere;
}

.ops-head-toggle {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.ops-head-toggle svg {
  width: 12px;
  height: 12px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
}

/* The facts panel of the prototype reads a little larger than the rest of
   the page. */
.config-facts__head h3 {
  font-size: 16px;
}

.config-facts__subtitle {
  font-size: 14.5px;
}


.config-editor :deep(.cm-editor) {
  min-height: 24rem;
}

.config-facts__list div {
  grid-template-columns: 110px 1fr;
}

/* The dialog of a container: the tabs of its two views and the facts of
   the container above the logs. */
.container-dialog {
  display: grid;
  gap: 14px;
}

.detail-tabs {
  display: inline-flex;
  gap: 20px;
  border-bottom: 1px solid #e6ecf7;
}

.detail-tabs button {
  appearance: none;
  border: none;
  border-bottom: 2px solid transparent;
  background: none;
  margin-bottom: -1px;
  padding: 0 2px 10px;
  font-size: 13.5px;
  font-weight: 500;
  color: #667085;
  cursor: pointer;
}

.detail-tabs button.active {
  border-bottom-color: #2f6ef0;
  color: #132a56;
  font-weight: 600;
}

.dialog-state {
  display: flex;
  align-items: center;
  gap: 10px;
}

.dialog-logbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

/* The back button of the dialog returns to the list the way the
   administrator came in. */
.dialog-back {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: none;
  background: none;
  padding: 0;
  color: #667085;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
}

.dialog-back:hover {
  color: #0255f0;
}

.dialog-back svg {
  width: 14px;
  height: 14px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.9;
  stroke-linecap: round;
  stroke-linejoin: round;
}
</style>
