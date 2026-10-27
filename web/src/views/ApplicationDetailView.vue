<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import DeploymentHistory from '../components/DeploymentHistory.vue'
import DeploymentStatus from '../components/DeploymentStatus.vue'
import LogViewer from '../components/LogViewer.vue'
import MetricChart from '../components/MetricChart.vue'
import YamlEditor from '../components/YamlEditor.vue'
import { api } from '../api/client'
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
import {
  createLogSubscription,
  deleteLogSubscription,
  describeLogError,
  logErrorMessage,
  openLogStream,
  type LogStream,
} from '../api/logs'
import {
  getMetrics,
  isStale,
  metricNames,
  metricPoints,
  metricScale,
  metricUnit,
  newestPoint,
  listContainers,
  type ContainerSummary,
  type MetricName,
} from '../api/metrics'
import {
  ApiError,
  configFailures,
  describeError,
  formatTimestamp,
  type AppConfig,
  type Deployment,
  type LogEvent,
  type MetricPoint,
  type NodeListPayload,
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

type Tab = 'overview' | 'containers' | 'logs' | 'config' | 'history'

const tabs: { key: Tab; label: string }[] = [
  { key: 'overview', label: '概况' },
  { key: 'containers', label: '容器' },
  { key: 'logs', label: '日志' },
  { key: 'config', label: '配置' },
  { key: 'history', label: '部署记录' },
]

const tab = ref<Tab>('overview')

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

// The limits of the curve of the container.
const curveRanges = [
  { minutes: 60, label: '最近 1 小时' },
  { minutes: 360, label: '最近 6 小时' },
  { minutes: 1440, label: '最近 24 小时' },
]

// The container whose curve and logs the page shows.
//
// The control端 names a container by its identifier, so the administrator
// writes it down.
const containerID = ref('')
// The containers of the application. The page names them by their Compose
// service, so the administrator picks one instead of typing an identifier.
const appContainers = ref<ContainerSummary[]>([])
const appContainersError = ref('')
const metric = ref<MetricName>('cpu_percent')
const range = ref(curveRanges[0].minutes)
const points = ref<MetricPoint[]>([])
const curveLoading = ref(false)
const curveError = ref('')
const curveLoaded = ref(false)

// The nodes of the deployment.
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

// The logs of the container.
//
// The subscription is opened on demand and closed again when the page
// leaves the tab, so no read of the logs stays open behind the back of
// the administrator.
const logBusy = ref(false)
const logOpen = ref(false)
const logLines = ref<string[]>([])
const logTruncated = ref(false)
const logError = ref('')
const logNotice = ref('')
let logStream: LogStream | null = null
let logSubscriptionID = ''
// logPending is the text of the log that does not end with a line break
// yet, so a piece that ends in the middle of a line does not lose it.
let logPending = ''

// maxLogLines is the number of the lines the page keeps.
//
// A container that writes a lot does not fill the memory of the browser:
// the oldest lines leave the buffer and the page says so.
const maxLogLines = 5000

// theMetricName names the metric of the curve.
const theMetricName = computed(
  () => metricNames.find((item) => item.name === metric.value)?.label ?? metric.value,
)

// theUnit is the unit of the curve.
const theUnit = computed(() => metricUnit(metric.value))

// theScale writes a value of the curve in its unit.
const theScale = computed(() => metricScale(metric.value))

// theDataIsStale reports whether the newest observation is too old.
const theDataIsStale = computed(() => curveLoaded.value && isStale(points.value, now.value))

// theNewestAt is the moment of the newest observation of the curve.
const theNewestAt = computed(() => newestPoint(points.value)?.at ?? '')

// theTargetIsConnected reports whether the agent of the node of the target
// is connected.
//
// A node that is not connected is not a machine that is broken: the page
// says that the connection is interrupted and never that the server is down.
const theTargetIsConnected = computed(() => {
  const target = app.value?.target_id ?? ''
  if (target === '') {
    return true
  }
  const node = nodes.value.find((item) => item.id === target)
  if (!node) {
    return true
  }
  return node.connected
})

// shownLogLines are the lines of the log as the viewer shows them.
//
// The last line of the log may still grow, so it is shown with the lines
// that are complete.
const shownLogLines = computed(() =>
  logPending === '' ? logLines.value : [...logLines.value, logPending],
)

// currentConfig is the configuration the editor holds.
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
  // is never lost by a reload of the page.
  yaml.value = loaded.config.yaml
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
    if (!list.some((item) => item.id === containerID.value)) {
      containerID.value = list.find((item) => item.state === 'running')?.id ?? list[0]?.id ?? ''
    }
  } catch (failure) {
    appContainers.value = []
    appContainersError.value = describeError(failure)
  }
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
const observations = computed(() => {
  const message = deployment.value?.result_message ?? ''
  if (message === '') {
    return []
  }
  return message
    .split(';')
    .map((part) => part.trim())
    .filter((part) => part !== '')
})

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

// loadCurve reads the history of the container of the application.
//
// The curve belongs to the container, not to the machine of the node.
async function loadCurve(): Promise<void> {
  const target = app.value?.target_id ?? ''
  if (target === '' || containerID.value.trim() === '') {
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
      node_id: target,
      container_id: containerID.value.trim(),
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

// openLogs opens the subscription of the logs of the container.
//
// The pieces of the logs travel in the stream of the events, so the page
// opens the stream first and asks for the subscription afterwards. A
// control端 that refuses the subscription gets a clear reason, never a
// silent page.
async function openLogs(): Promise<void> {
  const target = app.value?.target_id ?? ''
  if (logBusy.value) {
    return
  }
  if (target === '' || containerID.value.trim() === '') {
    logError.value = '请先填写容器的 ID。'
    return
  }
  logBusy.value = true
  logError.value = ''
  logNotice.value = ''
  logTruncated.value = false
  logLines.value = []
  logPending = ''
  try {
    logStream = openLogStream({
      onLog: (event) => {
        handleLogEvent(event)
      },
    })
    const opened = await Promise.race([
      logStream.ready.then(() => true),
      new Promise<boolean>((resolve) => {
        setTimeout(() => resolve(false), 5000)
      }),
    ])
    if (!opened) {
      logError.value = '实时连接没有建立，请稍后重试。'
      logStream.close()
      logStream = null
      return
    }
    const subscription = await createLogSubscription({
      node_id: target,
      container_id: containerID.value.trim(),
    })
    logSubscriptionID = subscription.id
    logOpen.value = true
  } catch (failure) {
    logOpen.value = false
    logError.value = describeLogError(failure)
    logStream?.close()
    logStream = null
  } finally {
    logBusy.value = false
  }
}

// closeLogs ends the subscription of the logs.
//
// The subscription belongs to the session of the administrator, so the page
// ends it when it leaves the tab. The read of the logs of the container
// ends with it.
async function closeLogs(): Promise<void> {
  const subscriptionID = logSubscriptionID
  logSubscriptionID = ''
  logOpen.value = false
  logStream?.close()
  logStream = null
  if (subscriptionID === '') {
    return
  }
  try {
    await deleteLogSubscription(subscriptionID)
  } catch {
    // The session ends the subscription on its own when the last stream
    // closes, so a failed request does not leave the page stuck.
  }
}

// handleLogEvent puts a piece of the log into the buffer.
//
// A piece that reports an error ends the stream of the logs and names the
// reason for the administrator. A piece that reports a gap tells the
// reader that the text before it is missing, without calling it a failure.
function handleLogEvent(event: LogEvent): void {
  if (event.object_id !== logSubscriptionID) {
    return
  }
  if (event.error_code) {
    logOpen.value = false
    logError.value = logErrorMessage(event.error_code)
    return
  }
  if (event.truncated) {
    logTruncated.value = true
    if (!event.text) {
      // The control端 rebuilt the tail of the log. The text before it is
      // gone, but nothing failed.
      logNotice.value = '日志已经重新接上，前面的内容缺失。'
      return
    }
  }
  appendText(event.text ?? '')
}

// appendText puts the text of a piece of the log into the buffer.
function appendText(text: string): void {
  if (text === '') {
    return
  }
  logPending += text
  const parts = logPending.split('\n')
  logPending = parts.pop() ?? ''
  if (parts.length === 0) {
    return
  }
  const all = [...logLines.value, ...parts]
  if (all.length > maxLogLines) {
    logTruncated.value = true
    logLines.value = all.slice(all.length - maxLogLines)
    return
  }
  logLines.value = all
}

// The page leaves the tab, so the read of the logs ends with it.
watch(tab, (current) => {
  if (current !== 'containers') {
    void closeLogs()
  }
})

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
  void closeLogs()
})
</script>

<template>
  <AppShell>
    <p v-if="loading" class="muted">正在读取应用…</p>

    <div v-else-if="error && !app" class="alert" role="alert">
      <p>{{ error }}</p>
      <button type="button" class="button button--quiet" @click="load">重试</button>
    </div>

    <template v-else-if="app">
      <div class="detail__head">
        <h1 class="page__title">{{ app.name }}</h1>
        <button type="button" class="button button--quiet" @click="reload">重新加载</button>
      </div>

      <p v-if="error" class="alert" role="alert">{{ error }}</p>
      <p v-if="notice" class="notice">{{ notice }}</p>

      <nav class="tabs" aria-label="应用详情">
        <button
          v-for="item in tabs"
          :key="item.key"
          type="button"
          class="tabs__tab"
          :class="{ 'tabs__tab--active': tab === item.key }"
          :aria-current="tab === item.key ? 'page' : undefined"
          @click="tab = item.key"
        >
          {{ item.label }}
        </button>
      </nav>

      <section v-if="tab === 'overview'" class="panel">
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
            <dt>目标</dt>
            <dd>{{ app.target_id }}</dd>
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
              <span v-else class="muted">还没有部署过这个应用。</span>
            </dd>
          </div>
        </dl>

        <div v-if="deployment && deployment.state === 'uncertain'" class="resolve">
          <h3 class="panel__title">人工核对</h3>
          <p class="muted">
            平台无法证明执行端已停止。只有在确认执行端不再继续操作之后，才能释放这个部署。
          </p>
          <div class="resolve__actions">
            <button type="button" class="button button--quiet" :disabled="reconciling" @click="reconcile">
              {{ reconciling ? '核对中…' : '重新核对' }}
            </button>
            <button type="button" class="button button--quiet" @click="resolveForm = true">
              人工核对并释放
            </button>
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
              <button class="button button--primary" type="submit" :disabled="resolving">释放</button>
              <button type="button" class="button button--quiet" @click="resolveForm = false">取消</button>
            </div>
          </form>
        </div>
      </section>

      <section v-else-if="tab === 'containers'" class="panel">
        <h2 class="panel__title">
          {{ app.mode === 'swarm' ? '服务、副本与任务' : '容器' }}
        </h2>
        <p class="muted">运行状态来自节点的最近一次清单。</p>

        <p v-if="appContainersError" class="alert" role="alert">{{ appContainersError }}</p>
        <p v-else-if="appContainers.length === 0" class="empty">这个应用在目标节点上还没有容器。</p>
        <table v-else class="nodes">
          <thead>
            <tr>
              <th scope="col">服务</th>
              <th scope="col">容器</th>
              <th scope="col">镜像</th>
              <th scope="col">状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in appContainers" :key="item.id">
              <th scope="row">{{ item.service === '' ? '—' : item.service }}</th>
              <td>{{ item.name }}</td>
              <td>{{ item.image }}</td>
              <td>{{ item.state }}{{ item.health === '' ? '' : ' · ' + item.health }}</td>
            </tr>
          </tbody>
        </table>

        <ul v-if="observations.length > 0" class="observations">
          <li v-for="observation in observations" :key="observation">{{ observation }}</li>
        </ul>

        <h3 class="panel__title">资源曲线</h3>
        <p class="muted">曲线是容器的，不是整台机器的。</p>

        <form class="form form--inline" novalidate @submit.prevent="loadCurve">
          <div class="field">
            <label class="field__label" for="detail-container">容器</label>
            <select
              id="detail-container"
              v-model="containerID"
              class="field__input"
              name="detail-container"
              :disabled="appContainers.length === 0"
            >
              <option v-if="appContainers.length === 0" value="">这个应用还没有容器</option>
              <option v-for="item in appContainers" :key="item.id" :value="item.id">
                {{ item.name }} · {{ item.state }}{{ item.health === '' ? '' : ' · ' + item.health }}
              </option>
            </select>
          </div>

          <div class="field">
            <label class="field__label" for="detail-metric">指标</label>
            <select id="detail-metric" v-model="metric" class="field__input" name="detail-metric">
              <option v-for="item in metricNames" :key="item.name" :value="item.name">
                {{ item.label }}
              </option>
            </select>
          </div>

          <div class="field">
            <label class="field__label" for="detail-range">时间范围</label>
            <select id="detail-range" v-model.number="range" class="field__input" name="detail-range">
              <option v-for="item in curveRanges" :key="item.minutes" :value="item.minutes">
                {{ item.label }}
              </option>
            </select>
          </div>

          <button class="button button--primary" type="submit" :disabled="curveLoading">
            {{ curveLoading ? '读取中…' : '查看曲线' }}
          </button>
        </form>

        <p v-if="curveError" class="alert" role="alert">
          <span>{{ curveError }}</span>
          <button type="button" class="button button--quiet" @click="loadCurve">重试</button>
        </p>

        <p v-else-if="!theTargetIsConnected" class="notice">
          连接中断。曲线停在最后一次收到数据的时候。
        </p>

        <p v-else-if="theDataIsStale" class="notice">
          数据已过期。最后一次数据来自
          {{ theNewestAt === '' ? '更早' : formatTimestamp(theNewestAt) }}。
        </p>

        <p v-if="!curveLoaded && !curveLoading && curveError === ''" class="empty">
          选择一个容器之后可以看到它的资源曲线。
        </p>

        <MetricChart
          v-if="curveLoaded"
          :points="points"
          :unit="theUnit"
          :title="theMetricName"
          :format="theScale"
        />

        <h3 class="panel__title">日志</h3>
        <p class="muted">日志按需打开，离开这个页签就会关闭。</p>

        <div class="actions">
          <button
            v-if="!logOpen"
            type="button"
            class="button button--quiet"
            :disabled="logBusy"
            @click="openLogs"
          >
            {{ logBusy ? '打开中…' : '打开日志' }}
          </button>
          <button v-else type="button" class="button button--quiet" @click="closeLogs">
            关闭日志
          </button>
        </div>

        <p v-if="logError" class="alert" role="alert">{{ logError }}</p>
        <p v-if="logNotice" class="notice">{{ logNotice }}</p>

        <LogViewer :lines="shownLogLines" :truncated="logTruncated" />
      </section>

      <section v-else-if="tab === 'logs'" class="panel">
        <h2 class="panel__title">日志</h2>

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

      <section v-else-if="tab === 'config'" class="panel">
        <h2 class="panel__title">配置</h2>

        <div v-if="serverChanged" class="notice">
          <p>服务端的配置与本页面的编辑不同。本页面的编辑一直保留着。</p>
          <div class="resolve__actions">
            <button type="button" class="button button--quiet" @click="useServerConfig">
              使用服务端的配置
            </button>
            <button type="button" class="button button--quiet" @click="serverChanged = false">
              保留我的编辑
            </button>
          </div>
        </div>

        <YamlEditor v-model="yaml" />

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
            <button type="button" class="button button--quiet" @click="removeVariable(name)">删除</button>
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
          <button class="button button--quiet" type="submit">添加变量</button>
        </form>

        <h3 class="panel__title">影响预览</h3>
        <p class="muted">部署之前先看这次改动会带来什么。</p>

        <div class="actions">
          <button type="button" class="button button--quiet" :disabled="previewing" @click="runPreview">
            {{ previewing ? '预览中…' : '预览影响' }}
          </button>
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
        <p v-else-if="contentChanged" class="muted">
          配置在预览之后发生了变化，请重新预览。
        </p>

        <div class="actions">
          <button
            type="button"
            class="button button--primary"
            :disabled="!canSubmit"
            @click="submit"
          >
            {{ submitting ? '提交中…' : '保存并部署' }}
          </button>
        </div>
      </section>

      <section v-else class="panel">
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

.detail__head .page__title {
  margin-bottom: var(--space-5);
}

.tabs {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-4);
  margin-bottom: var(--space-4);
  border-bottom: 1px solid var(--color-border);
}

.tabs__tab {
  font: inherit;
  background: transparent;
  border: none;
  border-bottom: 2px solid transparent;
  padding: var(--space-2) 0;
  color: var(--color-text-muted);
  cursor: pointer;
}

.tabs__tab--active {
  color: var(--color-text);
  border-bottom-color: var(--color-accent);
  font-weight: 600;
}

.facts {
  display: grid;
  gap: var(--space-3);
  margin: 0;
}

.facts__row {
  display: grid;
  grid-template-columns: 8rem 1fr;
  gap: var(--space-3);
  align-items: baseline;
}

.facts__row dt {
  color: var(--color-text-muted);
  font-size: 0.9rem;
}

.facts__row dd {
  margin: 0;
  overflow-wrap: anywhere;
}

.observations,
.variables {
  display: grid;
  gap: var(--space-2);
  padding: 0;
  margin: var(--space-3) 0 0;
  list-style: none;
}

.observations li {
  padding: var(--space-2) 0;
  border-bottom: 1px solid var(--color-border);
}

.variables__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-2) 0;
  border-bottom: 1px solid var(--color-border);
}

.variables__name {
  min-width: 8rem;
}

.variables__input {
  flex: 1 1 12rem;
  max-width: 20rem;
}

.form--inline {
  grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
  align-items: end;
  max-width: none;
}

.form--inline .button {
  margin-bottom: var(--space-1);
}

.actions {
  display: flex;
  gap: var(--space-3);
  margin-top: var(--space-4);
}

.preview {
  margin-top: var(--space-4);
  padding: var(--space-4);
  border: 1px solid var(--color-border);
  border-radius: 6px;
  background: var(--color-page);
}

.preview__list {
  display: grid;
  gap: var(--space-1);
  padding-left: var(--space-5);
  font-size: 0.9rem;
}

.preview__block {
  margin-top: var(--space-3);
}

.resolve {
  margin-top: var(--space-5);
  padding-top: var(--space-4);
  border-top: 1px solid var(--color-border);
}

.resolve__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  margin-top: var(--space-3);
}

.logs {
  max-height: 30rem;
  overflow: auto;
  padding: var(--space-3);
  border: 1px solid var(--color-border);
  border-radius: 6px;
  background: var(--color-page);
  font-family: var(--font-mono);
  font-size: 0.8rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.alert__list {
  padding-left: var(--space-5);
}

.field__check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.9rem;
}
</style>
