<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppShell from '../components/AppShell.vue'
import AlertRuleModal from '../components/AlertRuleModal.vue'
import {
  acknowledgeAlert,
  createAlertRule,
  deleteAlertRule,
  getWebhook,
  listAlertRules,
  listAlerts,
  saveWebhook,
  testWebhook,
  updateAlertRule,
  type AlertRuleInput,
} from '../api/alerts'
import { listContainers } from '../api/metrics'
import { listNodes } from '../api/nodes'
import {
  ApiError,
  describeError,
  formatTimestamp,
  type AlertEvent,
  type AlertRule,
  type AlertSeverity,
  type NodeSummary,
  type NotificationStatus,
  type WebhookPayload,
} from '../api/types'
import {
  conditionText,
  kindLabels,
  ruleDescription,
  ruleDisplayName,
} from '../components/alertText'

// The tabs of the page: the rules, the records of the events and the target
// of the notifications.
type Tab = 'rules' | 'events' | 'notify'

const tabs: { key: Tab; label: string }[] = [
  { key: 'rules', label: '告警规则' },
  { key: 'events', label: '告警记录' },
  { key: 'notify', label: '通知设置' },
]

// The three ways the records of the events are grouped.
type EventFilter = 'all' | 'active' | 'acknowledged' | 'resolved'

const eventFilters: { key: EventFilter; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'active', label: '当前异常' },
  { key: 'acknowledged', label: '已确认' },
  { key: 'resolved', label: '已恢复' },
]

const tab = ref<Tab>('rules')

const events = ref<AlertEvent[]>([])
const rules = ref<AlertRule[]>([])
const webhook = ref<WebhookPayload | null>(null)
const notifications = ref<NotificationStatus | null>(null)

const loading = ref(true)
const error = ref('')
const notice = ref('')

// The names of the objects the rules watch. A rule of an object the page
// could not resolve falls back to the raw identifier.
const containerNames = ref(new Map<string, string>())
const nodeNames = ref(new Map<string, string>())

// The filters of the rules.
const search = ref('')
const statusFilter = ref<'all' | 'enabled' | 'disabled'>('all')
const severityFilter = ref<'all' | AlertSeverity>('all')

// The filters of the records.
const eventFilter = ref<EventFilter>('all')
const eventSearch = ref('')

// The pagination of the two tables.
const rulesPage = ref(1)
const rulesPageSize = ref(10)
const eventsPage = ref(1)
const eventsPageSize = ref(10)

// The form of a rule. formRule is the rule the form shows, editing is the
// rule a change writes back. A copy of a rule fills the form but holds no
// editing rule, so saving it creates a rule.
const formOpen = ref(false)
const formRule = ref<AlertRule | null>(null)
const editing = ref<AlertRule | null>(null)
const ruleErrors = ref<Record<string, string>>({})
const ruleError = ref('')
const ruleBusy = ref(false)

// The confirmation of the administrator and the switch of a rule.
const acknowledging = ref('')
const acknowledgeError = ref('')
const removing = ref('')
const removeError = ref('')
const toggling = ref('')

// The target of the notifications.
const url = ref('')
const cidrs = ref('')
const notifyEnabled = ref(true)
const webhookError = ref('')
const webhookBusy = ref(false)
const testBusy = ref(false)
const testNotice = ref('')

// now is the clock of the page. The sparkline and the relative times read
// it, so the tests and the page agree on what today means.
const now = ref(new Date())
let clockTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  clockTimer = setInterval(() => {
    now.value = new Date()
  }, 30 * 1000)
})
onBeforeUnmount(() => {
  if (clockTimer) {
    clearInterval(clockTimer)
  }
})

// enabledRules are the rules the engine evaluates.
const enabledRules = computed(() => rules.value.filter((rule) => rule.enabled))

// disabledRules are the rules the administrator paused.
const disabledRules = computed(() => rules.value.filter((rule) => !rule.enabled))

// todayStart is the midnight of the day the page shows.
const todayStart = computed(() => {
  const at = new Date(now.value)
  at.setHours(0, 0, 0, 0)
  return at.getTime()
})

// todayEvents are the events that opened today.
const todayEvents = computed(() =>
  events.value.filter((event) => new Date(event.started_at).getTime() >= todayStart.value),
)

// sparkBuckets counts the events of the last twenty-four hours, one bucket
// per hour, the oldest first.
const sparkBuckets = computed<number[]>(() => {
  const buckets = new Array<number>(24).fill(0)
  const hourStart = now.value.getTime() - 23 * 3600 * 1000
  for (const event of events.value) {
    const at = new Date(event.started_at).getTime()
    if (Number.isNaN(at) || at < hourStart) {
      continue
    }
    const index = Math.min(23, Math.floor((at - hourStart) / 3600000))
    buckets[index] += 1
  }
  return buckets
})

// sparkPaths draws the buckets as one line and one area, the way the
// overview draws its trend. An hour without events keeps the line flat.
const sparkPaths = computed(() => {
  const points = sparkBuckets.value
  const total = points.reduce((sum, value) => sum + value, 0)
  const w = 96
  const h = 28
  if (total === 0) {
    return null
  }
  const ceiling = Math.max(...points, 1) * 1.15
  const x = (index: number) => (index * w) / (points.length - 1)
  const y = (value: number) => h - 1 - (value / ceiling) * (h - 2)
  let line = `M${x(0).toFixed(1)},${y(points[0]).toFixed(1)}`
  for (let index = 1; index < points.length; index += 1) {
    line += ` L${x(index).toFixed(1)},${y(points[index]).toFixed(1)}`
  }
  const area = `${line} L${w},${h} L0,${h} Z`
  return { line, area, w, h }
})

// The severities of a rule and the names the administrator reads.
const severityLabels: Record<AlertSeverity, string> = {
  info: '提示',
  warning: '警告',
  critical: '严重',
}

// scopeOf names the breadth a rule watches, in the words of the inventory
// when the page could resolve it.
function scopeOf(rule: AlertRule): string {
  if (rule.kind === 'agent_offline') {
    return nodeNames.value.get(rule.scope_id) ?? rule.scope_id
  }
  if (rule.scope_kind === 'all') {
    return '全部容器'
  }
  if (rule.scope_kind === 'namespace') {
    return `命名空间 ${rule.scope_id}`
  }
  return containerNames.value.get(rule.scope_id) ?? rule.scope_id
}

// ruleSummary is the text a search reads: the title, the kind and the
// object of a rule.
function ruleSummary(rule: AlertRule): string {
  return `${ruleDisplayName(rule)} ${kindLabels[rule.kind] ?? rule.kind} ${scopeOf(rule)} ${rule.description}`.toLowerCase()
}

// filteredRules applies the search and the two selects of the toolbar.
const filteredRules = computed(() =>
  rules.value.filter((rule) => {
    if (search.value.trim() !== '' && !ruleSummary(rule).includes(search.value.trim().toLowerCase())) {
      return false
    }
    if (statusFilter.value === 'enabled' && !rule.enabled) {
      return false
    }
    if (statusFilter.value === 'disabled' && rule.enabled) {
      return false
    }
    if (severityFilter.value !== 'all' && rule.severity !== severityFilter.value) {
      return false
    }
    return true
  }),
)

// rulesPageCount is the number of the pages of the filtered rules.
const rulesPageCount = computed(() =>
  Math.max(1, Math.ceil(filteredRules.value.length / rulesPageSize.value)),
)

// shownRules are the rules of the page the administrator reads.
const shownRules = computed(() => {
  const start = (rulesPage.value - 1) * rulesPageSize.value
  return filteredRules.value.slice(start, start + rulesPageSize.value)
})

// lastTriggerOf names the time a rule last opened an event.
function lastTriggerOf(ruleID: string): string | null {
  let latest: string | null = null
  for (const event of events.value) {
    if (event.rule_id !== ruleID) {
      continue
    }
    if (latest === null || new Date(event.started_at) > new Date(latest)) {
      latest = event.started_at
    }
  }
  return latest
}

// relativeTime writes a time the way a list reads it.
function relativeTime(value: string | null): string {
  if (!value) {
    return '--'
  }
  const at = new Date(value).getTime()
  if (Number.isNaN(at)) {
    return '--'
  }
  const seconds = Math.round((now.value.getTime() - at) / 1000)
  if (seconds < 60) {
    return '刚刚'
  }
  if (seconds < 3600) {
    return `${Math.floor(seconds / 60)}分钟前`
  }
  if (seconds < 86400) {
    return `${Math.floor(seconds / 3600)}小时前`
  }
  if (seconds < 7 * 86400) {
    return `${Math.floor(seconds / 86400)}天前`
  }
  return formatTimestamp(value)
}

// activeEvents are the anomalies that nobody looked at yet.
const activeEvents = computed(() =>
  events.value.filter((event) => event.state === 'active' && !event.acknowledged_at),
)

// acknowledgedEvents are the events the administrator confirmed.
const acknowledgedEvents = computed(() =>
  events.value.filter((event) => event.acknowledged_at !== null),
)

// resolvedEvents are the anomalies that are over.
const resolvedEvents = computed(() =>
  events.value.filter((event) => event.state === 'resolved' && !event.acknowledged_at),
)

// countOf names the number of the events of a group.
function countOf(key: EventFilter): number {
  if (key === 'acknowledged') {
    return acknowledgedEvents.value.length
  }
  if (key === 'resolved') {
    return resolvedEvents.value.length
  }
  if (key === 'active') {
    return activeEvents.value.length
  }
  return events.value.length
}

// severityOf names the severity of an event. The rule may be gone, so the
// severity of a deleted rule is unknown.
function severityOf(event: AlertEvent): AlertSeverity | null {
  const rule = rules.value.find((item) => item.id === event.rule_id)
  return rule ? rule.severity : null
}

// eventSummary is the text a search of the records reads.
function eventSummary(event: AlertEvent): string {
  const rule = rules.value.find((item) => item.id === event.rule_id)
  return `${event.summary} ${rule ? ruleDisplayName(rule) : ''} ${event.object_id}`.toLowerCase()
}

// filteredEvents applies the group and the search of the records.
const filteredEvents = computed(() =>
  events.value.filter((event) => {
    if (eventFilter.value === 'acknowledged' && event.acknowledged_at === null) {
      return false
    }
    if (eventFilter.value === 'active' && !(event.state === 'active' && !event.acknowledged_at)) {
      return false
    }
    if (eventFilter.value === 'resolved' && !(event.state === 'resolved' && !event.acknowledged_at)) {
      return false
    }
    if (eventSearch.value.trim() !== '' && !eventSummary(event).includes(eventSearch.value.trim().toLowerCase())) {
      return false
    }
    return true
  }),
)

// eventsPageCount is the number of the pages of the filtered events.
const eventsPageCount = computed(() =>
  Math.max(1, Math.ceil(filteredEvents.value.length / eventsPageSize.value)),
)

// shownEvents are the events of the page the administrator reads.
const shownEvents = computed(() => {
  const start = (eventsPage.value - 1) * eventsPageSize.value
  return filteredEvents.value.slice(start, start + eventsPageSize.value)
})

// pageOf writes the page numbers of a pager. A short list has one page, a
// long one shows a window around the current page.
function pageOf(current: number, count: number): number[] {
  if (count <= 7) {
    return Array.from({ length: count }, (_, index) => index + 1)
  }
  const start = Math.max(1, Math.min(current - 2, count - 4))
  return Array.from({ length: 5 }, (_, index) => start + index)
}

// A filter that changes resets the page of its table.
watch([search, statusFilter, severityFilter, rulesPageSize], () => {
  rulesPage.value = 1
})
watch([eventFilter, eventSearch, eventsPageSize], () => {
  eventsPage.value = 1
})

// loadScopeNames builds the maps that turn the identifier of a rule into
// the name of a container or of a node.
//
// The inventory is a courtesy of the page: a rule whose object is gone
// falls back to the raw identifier instead of failing the whole page.
async function loadScopeNames(): Promise<void> {
  try {
    const nodes: NodeSummary[] = await listNodes()
    const containers = new Map<string, string>()
    const nodesByName = new Map<string, string>()
    for (const node of nodes) {
      nodesByName.set(node.id, node.hostname)
    }
    await Promise.all(
      nodes.map(async (node) => {
        try {
          for (const container of await listContainers(node.id)) {
            containers.set(container.id, container.name)
          }
        } catch {
          // The containers of one node stay unresolved.
        }
      }),
    )
    containerNames.value = containers
    nodeNames.value = nodesByName
  } catch {
    // The names stay empty and the page shows the identifiers.
  }
}

// load reads the events, the rules, the target of the notifications and the
// names of the objects.
async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const [loadedEvents, loadedRules, bundle] = await Promise.all([
      listAlerts(),
      listAlertRules(),
      getWebhook(),
    ])
    events.value = loadedEvents
    rules.value = loadedRules
    webhook.value = bundle.webhook
    notifications.value = bundle.notifications
    cidrs.value = bundle.webhook.allowed_private_cidrs.join(', ')
    notifyEnabled.value = bundle.webhook.enabled
    await loadScopeNames()
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

// openForm opens the form of a new rule.
function openForm(): void {
  formRule.value = null
  editing.value = null
  ruleErrors.value = {}
  ruleError.value = ''
  formOpen.value = true
}

// editRule opens the form of a rule the administrator changes.
function editRule(rule: AlertRule): void {
  formRule.value = rule
  editing.value = rule
  ruleErrors.value = {}
  ruleError.value = ''
  formOpen.value = true
}

// copyRule opens the form of a new rule that carries the values of an
// existing one. The copy has no identifier, so saving it creates a rule.
function copyRule(rule: AlertRule): void {
  formRule.value = { ...rule, id: '', name: `${ruleDisplayName(rule)} 副本` }
  editing.value = null
  ruleErrors.value = {}
  ruleError.value = ''
  formOpen.value = true
}

// cancelForm closes the form without saving.
function cancelForm(): void {
  formOpen.value = false
  formRule.value = null
  editing.value = null
  ruleErrors.value = {}
  ruleError.value = ''
}

// saveRule writes the rule of the form.
//
// A refused rule keeps the form open and the input of the administrator, so
// only the field that has to change has to be changed.
async function saveRule(input: AlertRuleInput): Promise<void> {
  if (ruleBusy.value) {
    return
  }
  ruleBusy.value = true
  ruleError.value = ''
  ruleErrors.value = {}
  try {
    if (editing.value) {
      await updateAlertRule(editing.value.id, input)
      notice.value = '规则已经保存。'
    } else {
      await createAlertRule(input)
      notice.value = '规则已经创建。'
    }
    cancelForm()
    rules.value = await listAlertRules()
  } catch (failure) {
    ruleError.value = describeError(failure)
    if (failure instanceof ApiError) {
      ruleErrors.value = failure.fields
    }
  } finally {
    ruleBusy.value = false
  }
}

// toggleRule pauses a rule or evaluates it again.
//
// The switch sends the whole rule, because the API of a change writes the
// rule of the request in one piece.
async function toggleRule(rule: AlertRule): Promise<void> {
  if (toggling.value !== '') {
    return
  }
  toggling.value = rule.id
  removeError.value = ''
  try {
    await updateAlertRule(rule.id, {
      name: rule.name,
      description: rule.description,
      kind: rule.kind,
      severity: rule.severity,
      scope_kind: rule.scope_kind,
      scope_id: rule.scope_id,
      metric: rule.metric,
      operator: rule.operator,
      threshold: rule.threshold,
      for_seconds: rule.for_seconds,
      enabled: !rule.enabled,
      recover_notify: rule.recover_notify,
    })
    notice.value = rule.enabled ? '规则已经停用。' : '规则已经启用。'
    rules.value = await listAlertRules()
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    toggling.value = ''
  }
}

// acknowledge marks an event as seen.
//
// Confirming is not a recovery: the event keeps its state until the
// observation shows that the anomaly is over.
async function acknowledge(event: AlertEvent): Promise<void> {
  if (acknowledging.value !== '') {
    return
  }
  acknowledging.value = event.id
  acknowledgeError.value = ''
  try {
    await acknowledgeAlert(event.id)
    notice.value = '已经确认这条异常。'
    events.value = await listAlerts()
  } catch (failure) {
    acknowledgeError.value = describeError(failure)
  } finally {
    acknowledging.value = ''
  }
}

// askForRemove opens the confirmation of a removal.
function askForRemove(rule: AlertRule): void {
  removeError.value = ''
  removing.value = rule.id
}

// cancelRemove closes the confirmation without doing anything.
function cancelRemove(): void {
  removing.value = ''
}

// remove deletes a rule.
//
// The events of the rule stay in the history, so the administrator still
// reads what happened.
async function remove(rule: AlertRule): Promise<void> {
  removeError.value = ''
  try {
    await deleteAlertRule(rule.id)
    removing.value = ''
    notice.value = '规则已经删除。'
    rules.value = await listAlertRules()
  } catch (failure) {
    removeError.value = describeError(failure)
  }
}

// saveTarget writes the target of the notifications.
//
// An address that the administrator does not write keeps the address that is
// stored already, so the masked form of the address is never written back.
async function saveTarget(): Promise<void> {
  if (webhookBusy.value) {
    return
  }
  webhookBusy.value = true
  webhookError.value = ''
  try {
    const saved = await saveWebhook({
      ...(url.value.trim() === '' ? {} : { url: url.value.trim() }),
      allowed_private_cidrs: cidrs.value
        .split(/[\s,]+/)
        .map((entry) => entry.trim())
        .filter((entry) => entry !== ''),
      enabled: notifyEnabled.value,
    })
    webhook.value = saved
    url.value = ''
    notice.value = '通知设置已经保存。'
  } catch (failure) {
    webhookError.value = describeError(failure)
  } finally {
    webhookBusy.value = false
  }
}

// sendTest sends a test notification to the target.
//
// The test leaves the control端, so it only happens when the administrator
// asks for it.
async function sendTest(): Promise<void> {
  if (testBusy.value) {
    return
  }
  testBusy.value = true
  testNotice.value = ''
  webhookError.value = ''
  try {
    await testWebhook()
    testNotice.value = '测试通知已经发出。'
  } catch (failure) {
    webhookError.value = describeError(failure)
  } finally {
    testBusy.value = false
  }
}

onMounted(load)
</script>

<template>
  <AppShell>
    <div v-if="loading" class="page-loading">
      <span class="spinner" aria-hidden="true"></span>正在读取告警…
    </div>

    <div v-else-if="error" class="page-error">
      <p>{{ error }}</p>
      <button type="button" class="text-button" @click="load">重试</button>
    </div>

    <div v-else class="alerts">
      <!-- The head of the page -->
      <section class="page-head">
        <div class="head-copy">
          <h1 class="page-title">告警管理</h1>
          <p class="page-subtitle">设置 Docker 监控告警规则，及时获取容器异常通知，保障服务稳定运行。</p>
        </div>
      </section>

      <p v-if="notice" class="banner success" role="status">{{ notice }}</p>

      <!-- The four readings -->
      <section class="stats" aria-label="告警统计">
        <div class="stat-card">
          <span class="stat-icon rules">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
              <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
            </svg>
          </span>
          <span class="stat-copy">
            <strong>{{ rules.length }}</strong>
            <small>告警规则总数</small>
          </span>
        </div>

        <div class="stat-card">
          <span class="stat-icon running">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <circle cx="12" cy="12" r="9" fill="currentColor" />
              <path d="m8 12.4 2.6 2.6L16.2 9" fill="none" stroke="#fff" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" />
            </svg>
          </span>
          <span class="stat-copy">
            <strong>{{ enabledRules.length }}</strong>
            <small>运行中</small>
          </span>
        </div>

        <div class="stat-card">
          <span class="stat-icon paused">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <circle cx="12" cy="12" r="9" fill="currentColor" />
              <path d="M12 7.4v5.4" stroke="#fff" stroke-width="1.9" stroke-linecap="round" />
              <circle cx="12" cy="16.4" r="1.1" fill="#fff" />
            </svg>
          </span>
          <span class="stat-copy">
            <strong>{{ disabledRules.length }}</strong>
            <small>已停用</small>
          </span>
        </div>

        <div class="stat-card">
          <span class="stat-icon bell">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M18 8a6 6 0 0 0-12 0c0 6-3 8-3 8h18s-3-2-3-8" fill="currentColor" />
              <path d="M10.3 20a2 2 0 0 0 3.4 0" fill="currentColor" />
            </svg>
          </span>
          <span class="stat-copy">
            <strong>{{ todayEvents.length }}</strong>
            <small>今日告警</small>
          </span>
          <svg v-if="sparkPaths" class="stat-spark" :viewBox="`0 0 ${sparkPaths.w} ${sparkPaths.h}`" aria-hidden="true">
            <defs>
              <linearGradient id="alert-spark-fill" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0" stop-color="#4a95ff" stop-opacity=".28" />
                <stop offset="1" stop-color="#4a95ff" stop-opacity=".02" />
              </linearGradient>
            </defs>
            <path :d="sparkPaths.area" fill="url(#alert-spark-fill)" />
            <path :d="sparkPaths.line" fill="none" stroke="#2f7ef0" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
        </div>
      </section>

      <!-- The tabs of the page -->
      <nav class="tabs" aria-label="告警管理">
        <button
          v-for="item in tabs"
          :key="item.key"
          type="button"
          class="tab"
          :class="{ active: tab === item.key }"
          :aria-pressed="tab === item.key"
          @click="tab = item.key"
        >
          {{ item.label }}
        </button>
      </nav>

      <!-- The rules -->
      <section v-if="tab === 'rules'" class="panel">
        <div class="toolbar">
          <div class="toolbar-fields">
            <label class="search">
              <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m20 20-3.8-3.8" /></svg>
              <input
                v-model="search"
                type="search"
                name="rule-search"
                autocomplete="off"
                placeholder="搜索规则名称或容器…"
              />
            </label>
            <select v-model="statusFilter" class="select" name="rule-status" aria-label="状态">
              <option value="all">全部状态</option>
              <option value="enabled">运行中</option>
              <option value="disabled">已停用</option>
            </select>
            <select v-model="severityFilter" class="select" name="rule-severity-filter" aria-label="级别">
              <option value="all">全部级别</option>
              <option value="info">提示</option>
              <option value="warning">警告</option>
              <option value="critical">严重</option>
            </select>
          </div>
          <button type="button" class="primary-button" @click="openForm">＋ 创建告警规则</button>
        </div>

        <AlertRuleModal
          :open="formOpen"
          :rule="formRule"
          :editing="editing !== null"
          :errors="ruleErrors"
          :busy="ruleBusy"
          :error="ruleError"
          @save="saveRule"
          @cancel="cancelForm"
        />

        <div v-if="shownRules.length === 0" class="empty">
          <p>{{ filteredRules.length === 0 && rules.length > 0 ? '没有符合筛选条件的规则。' : '还没有告警规则，创建第一条规则开始监控。' }}</p>
        </div>

        <div v-else class="table-scroll">
          <table class="rule-table">
          <thead>
            <tr>
              <th>规则名称</th>
              <th>监控对象</th>
              <th>告警条件</th>
              <th>通知方式</th>
              <th>状态</th>
              <th>最近触发</th>
              <th class="th-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="rule in shownRules" :key="rule.id">
              <td>
                <div class="rule-cell">
                  <span class="rule-icon" :class="rule.severity" aria-hidden="true">
                    <svg viewBox="0 0 24 24">
                      <path d="M12 3 4 7.4v9.2l8 4.4 8-4.4V7.4L12 3Z" fill="currentColor" />
                      <path d="m12 3 8 4.4-8 4.4-8-4.4L12 3Z" fill="#fff" opacity=".3" />
                    </svg>
                  </span>
                  <div class="rule-copy">
                    <p class="rule-title">
                      <strong>{{ ruleDisplayName(rule) }}</strong>
                      <span class="sev" :class="rule.severity">{{ severityLabels[rule.severity] ?? rule.severity }}</span>
                    </p>
                    <small>{{ ruleDescription(rule) }}</small>
                  </div>
                </div>
              </td>
              <td>
                <span class="scope">
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <rect x="4" y="4" width="16" height="5" rx="1.7" fill="currentColor" />
                    <rect x="4" y="9.5" width="16" height="5" rx="1.7" fill="currentColor" opacity=".7" />
                    <rect x="4" y="15" width="16" height="5" rx="1.7" fill="currentColor" opacity=".45" />
                  </svg>
                  {{ scopeOf(rule) }}
                </span>
              </td>
              <td><span class="chip">{{ conditionText(rule) }}</span></td>
              <td>
                <span v-if="webhook?.configured && webhook.enabled" class="chip notify">
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <rect x="3" y="5.5" width="18" height="13" rx="2.4" fill="currentColor" />
                    <path d="m4 7 8 6 8-6" fill="none" stroke="#fff" stroke-width="1.6" />
                  </svg>
                  Webhook
                </span>
                <span v-else class="chip muted">未配置</span>
              </td>
              <td>
                <button
                  type="button"
                  class="switch"
                  role="switch"
                  :aria-checked="rule.enabled"
                  :aria-label="rule.enabled ? '停用这条规则' : '启用这条规则'"
                  :disabled="toggling === rule.id"
                  @click="toggleRule(rule)"
                >
                  <span class="knob" aria-hidden="true"></span>
                </button>
              </td>
              <td><span class="muted-cell">{{ relativeTime(lastTriggerOf(rule.id)) }}</span></td>
              <td>
                <div class="row-actions">
                  <button type="button" class="text-button" @click="editRule(rule)">编辑</button>
                  <button type="button" class="text-button" @click="copyRule(rule)">复制</button>
                  <template v-if="removing !== rule.id">
                    <button type="button" class="text-button danger" @click="askForRemove(rule)">删除</button>
                  </template>
                  <template v-else>
                    <span class="confirm-hint">确认删除？</span>
                    <button type="button" class="text-button danger" @click="remove(rule)">确认</button>
                    <button type="button" class="text-button" @click="cancelRemove">取消</button>
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        </div>

        <p v-if="removeError" class="form-error">{{ removeError }}</p>

        <div v-if="filteredRules.length > 0" class="table-foot">
          <span class="muted-cell">共 {{ filteredRules.length }} 条</span>
          <div class="pager">
            <button
              type="button"
              class="pager-step"
              :disabled="rulesPage <= 1"
              aria-label="上一页"
              @click="rulesPage -= 1"
            >
              ‹
            </button>
            <button
              v-for="page in pageOf(rulesPage, rulesPageCount)"
              :key="page"
              type="button"
              class="pager-page"
              :class="{ active: page === rulesPage }"
              :aria-pressed="page === rulesPage"
              @click="rulesPage = page"
            >
              {{ page }}
            </button>
            <button
              type="button"
              class="pager-step"
              :disabled="rulesPage >= rulesPageCount"
              aria-label="下一页"
              @click="rulesPage += 1"
            >
              ›
            </button>
            <select v-model.number="rulesPageSize" class="select compact" name="rules-page-size" aria-label="每页条数">
              <option :value="10">10 条/页</option>
              <option :value="20">20 条/页</option>
              <option :value="50">50 条/页</option>
            </select>
          </div>
        </div>
      </section>

      <!-- The records -->
      <section v-else-if="tab === 'events'" class="panel">
        <div class="toolbar">
          <div class="toolbar-fields">
            <nav class="group-chips" aria-label="事件分组">
              <button
                v-for="item in eventFilters"
                :key="item.key"
                type="button"
                class="group-chip"
                :class="{ active: eventFilter === item.key }"
                :aria-pressed="eventFilter === item.key"
                @click="eventFilter = item.key"
              >
                {{ item.label }}（{{ countOf(item.key) }}）
              </button>
            </nav>
            <label class="search">
              <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m20 20-3.8-3.8" /></svg>
              <input
                v-model="eventSearch"
                type="search"
                name="event-search"
                autocomplete="off"
                placeholder="搜索告警记录…"
              />
            </label>
          </div>
        </div>

        <div v-if="shownEvents.length === 0" class="empty">
          <p>这里没有告警记录。</p>
        </div>

        <ul v-else class="event-list">
          <li v-for="event in shownEvents" :key="event.id" class="event-row">
            <span class="event-dot" :class="severityOf(event) ?? 'unknown'" aria-hidden="true"></span>
            <div class="event-copy">
              <p class="event-title">
                {{ event.summary }}
                <span v-if="event.state === 'resolved'" class="state-pill ok">已恢复</span>
                <span v-else-if="event.acknowledged_at" class="state-pill quiet">已确认</span>
                <span v-else class="state-pill bad">异常中</span>
              </p>
              <small class="event-meta">
                开始于 {{ formatTimestamp(event.started_at) }}
                <template v-if="event.resolved_at"> · 恢复于 {{ formatTimestamp(event.resolved_at) }}</template>
                <template v-if="event.acknowledged_at"> · 确认于 {{ formatTimestamp(event.acknowledged_at) }}</template>
              </small>
            </div>
            <div class="event-actions">
              <button
                v-if="!event.acknowledged_at"
                type="button"
                class="text-button"
                :disabled="acknowledging !== ''"
                @click="acknowledge(event)"
              >
                确认
              </button>
            </div>
          </li>
        </ul>

        <p v-if="acknowledgeError" class="form-error">{{ acknowledgeError }}</p>

        <div v-if="filteredEvents.length > 0" class="table-foot">
          <span class="muted-cell">共 {{ filteredEvents.length }} 条</span>
          <div class="pager">
            <button
              type="button"
              class="pager-step"
              :disabled="eventsPage <= 1"
              aria-label="上一页"
              @click="eventsPage -= 1"
            >
              ‹
            </button>
            <button
              v-for="page in pageOf(eventsPage, eventsPageCount)"
              :key="page"
              type="button"
              class="pager-page"
              :class="{ active: page === eventsPage }"
              :aria-pressed="page === eventsPage"
              @click="eventsPage = page"
            >
              {{ page }}
            </button>
            <button
              type="button"
              class="pager-step"
              :disabled="eventsPage >= eventsPageCount"
              aria-label="下一页"
              @click="eventsPage += 1"
            >
              ›
            </button>
            <select v-model.number="eventsPageSize" class="select compact" name="events-page-size" aria-label="每页条数">
              <option :value="10">10 条/页</option>
              <option :value="20">20 条/页</option>
              <option :value="50">50 条/页</option>
            </select>
          </div>
        </div>
      </section>

      <!-- The target of the notifications -->
      <section v-else class="panel">
        <div class="notify-panel">
          <h3 class="panel-title">通知目标</h3>
          <dl class="notify-facts">
            <div class="fact">
              <dt>通知地址</dt>
              <dd class="break-all">
                <template v-if="webhook?.configured">
                  <code>{{ webhook.url_masked }}</code>
                  <span class="muted-cell"> · 地址只显示脱敏后的形式</span>
                </template>
                <template v-else>还没有配置通知地址。</template>
              </dd>
            </div>
            <div class="fact">
              <dt>允许的内网</dt>
              <dd class="break-all">
                <code v-if="webhook && webhook.allowed_private_cidrs.length > 0">
                  {{ webhook.allowed_private_cidrs.join(', ') }}
                </code>
                <template v-else>没有允许的内网地址。</template>
              </dd>
            </div>
            <div class="fact">
              <dt>通知结果</dt>
              <dd v-if="notifications">
                等待中 {{ notifications.pending }} · 已送达 {{ notifications.delivered }} · 失败
                {{ notifications.failed }} · 已跳过 {{ notifications.skipped }}
                <span v-if="notifications.last_delivered_at" class="muted-cell">
                  · 最近送达于 {{ formatTimestamp(notifications.last_delivered_at) }}
                </span>
              </dd>
              <dd v-else>—</dd>
            </div>
          </dl>

          <form class="notify-form" novalidate @submit.prevent="saveTarget">
            <div class="field">
              <label for="webhook-url">新的通知地址</label>
              <input
                id="webhook-url"
                v-model="url"
                type="url"
                name="webhook-url"
                autocomplete="off"
                placeholder="留空表示保持现在的地址"
              />
              <p class="field-hint">地址里可以带凭据，平台只以脱敏的形式显示它。</p>
            </div>

            <div class="field">
              <label for="webhook-cidrs">新增允许的内网网段</label>
              <input
                id="webhook-cidrs"
                v-model="cidrs"
                type="text"
                name="webhook-cidrs"
                autocomplete="off"
                placeholder="例如 10.0.0.0/8"
              />
              <p class="field-hint">
                允许内网地址后，通知会发送到这些网段里的地址。只填写你信任的网段，平台不会把通知转发到其它地方。
              </p>
            </div>

            <label class="check">
              <input v-model="notifyEnabled" type="checkbox" name="webhook-enabled" />
              <span>启用通知</span>
            </label>

            <p v-if="webhookError" class="form-error">{{ webhookError }}</p>
            <p v-if="testNotice" class="banner success">{{ testNotice }}</p>

            <div class="form-actions">
              <button type="submit" class="primary-button" :disabled="webhookBusy">保存通知设置</button>
              <button
                type="button"
                class="ghost-button"
                :disabled="testBusy || !webhook?.configured"
                @click="sendTest"
              >
                发送测试通知
              </button>
            </div>
          </form>
        </div>
      </section>
    </div>
  </AppShell>
</template>

<style scoped>
/* The head of the page ----------------------------------------------- */
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
}

/* The four readings --------------------------------------------------- */
.stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 20px;
  margin-top: 22px;
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

.stat-icon.paused {
  background: #feeceb;
  color: #f04438;
}

.stat-icon.bell {
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

.stat-spark {
  width: 96px;
  height: 28px;
  margin-left: auto;
  flex: none;
}

/* The tabs ------------------------------------------------------------ */
.tabs {
  display: flex;
  gap: 26px;
  margin: 26px 0 14px;
  border-bottom: 1px solid #e6ecf7;
}

.tab {
  appearance: none;
  padding: 0 2px 12px;
  border: none;
  border-bottom: 2px solid transparent;
  background: none;
  font-size: 15px;
  font-weight: 600;
  color: #667085;
  cursor: pointer;
}

.tab.active {
  border-bottom-color: #0255f0;
  color: #101828;
}

/* The panel and its toolbar ------------------------------------------- */
.panel {
  border: 1px solid #e9eff9;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 14px 30px -32px rgb(20 52 110 / 50%);
  padding: 20px 22px;
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
  width: 240px;
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

/* The buttons ---------------------------------------------------------- */
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
  color: #0255f0;
  font-size: 14px;
  font-weight: 600;
  padding: 8px 16px;
  cursor: pointer;
}

.ghost-button:hover {
  border-color: #9dbdf5;
}

.ghost-button:disabled {
  opacity: 0.55;
  cursor: default;
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
}

.text-button:hover {
  text-decoration: underline;
}

.text-button:disabled {
  opacity: 0.5;
  cursor: default;
}

.text-button.danger {
  color: #d92d20;
}

.form-error {
  margin: 10px 2px 0;
  font-size: 13px;
  color: #d92d20;
}

/* The table of the rules ------------------------------------------------ */
.table-scroll {
  overflow-x: auto;
  margin-top: 6px;
}

.rule-table {
  width: 100%;
  border-collapse: collapse;
  text-align: left;
}

.rule-table th {
  padding: 12px 14px;
  font-size: 12px;
  font-weight: 600;
  color: #667085;
  border-bottom: 1px solid #e6ecf7;
  white-space: nowrap;
}

.rule-table td {
  padding: 14px;
  border-bottom: 1px solid #eef2fa;
  font-size: 13px;
  color: #132a56;
  vertical-align: middle;
}

.rule-table tbody tr:last-child td {
  border-bottom: none;
}

.rule-table tbody tr:hover {
  background: #f8fbff;
}

.th-actions {
  text-align: right;
}

.rule-table td:last-child {
  text-align: right;
}

.rule-cell {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 220px;
}

.rule-icon {
  width: 38px;
  height: 38px;
  display: grid;
  place-items: center;
  flex: none;
  border-radius: 11px;
  background: #e9f2ff;
  color: #146ef5;
}

.rule-icon svg {
  width: 19px;
  height: 19px;
}

.rule-icon.info {
  background: #e9f2ff;
  color: #2f6fe4;
}

.rule-icon.warning {
  background: #fef4e6;
  color: #f79009;
}

.rule-icon.critical {
  background: #feeceb;
  color: #f04438;
}

.rule-copy {
  min-width: 0;
}

.rule-title {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
}

.rule-title strong {
  font-size: 13.5px;
  color: #132a56;
}

.rule-copy small {
  display: block;
  margin-top: 2px;
  font-size: 12px;
  color: #8ba0bf;
}

.sev {
  flex: none;
  border-radius: 999px;
  padding: 1px 8px;
  font-size: 11px;
  font-weight: 600;
}

.sev.info {
  background: #e9f2ff;
  color: #2f6fe4;
}

.sev.warning {
  background: #fef4e6;
  color: #b54708;
}

.sev.critical {
  background: #feeceb;
  color: #b42318;
}

.scope {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: #344054;
}

.scope svg {
  width: 15px;
  height: 15px;
  color: #2f6fe4;
  flex: none;
}

.chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-radius: 8px;
  background: #f0f5ff;
  color: #1d4fd8;
  padding: 4px 10px;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}

.chip svg {
  width: 14px;
  height: 14px;
}

.chip.notify {
  background: #e5f9ef;
  color: #067647;
}

.chip.muted {
  background: #f2f4f7;
  color: #667085;
}

/* The switch of a rule --------------------------------------------------- */
.switch {
  appearance: none;
  position: relative;
  width: 40px;
  height: 22px;
  border: none;
  border-radius: 999px;
  background: #d0d5dd;
  cursor: pointer;
  transition: background 140ms ease;
  padding: 0;
}

.switch[aria-checked='true'] {
  background: #0255f0;
}

.switch:disabled {
  opacity: 0.6;
  cursor: default;
}

.switch .knob {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: #fff;
  box-shadow: 0 1px 3px rgb(16 24 40 / 30%);
  transition: transform 140ms ease;
}

.switch[aria-checked='true'] .knob {
  transform: translateX(18px);
}

.muted-cell {
  color: #8ba0bf;
  font-size: 12.5px;
}

.row-actions {
  display: inline-flex;
  align-items: center;
  gap: 12px;
  white-space: nowrap;
}

.confirm-hint {
  font-size: 12px;
  color: #d92d20;
}

/* The foot of a table ----------------------------------------------------- */
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

/* The records ------------------------------------------------------------- */
.group-chips {
  display: inline-flex;
  gap: 8px;
  flex-wrap: wrap;
}

.group-chip {
  appearance: none;
  border: 1px solid #d8e2f5;
  border-radius: 999px;
  background: #fff;
  color: #475467;
  font-size: 12.5px;
  padding: 6px 14px;
  cursor: pointer;
}

.group-chip.active {
  border-color: #0255f0;
  background: #f0f5ff;
  color: #0255f0;
  font-weight: 600;
}

.event-list {
  list-style: none;
  margin: 10px 0 0;
  padding: 0;
  display: grid;
  gap: 0;
}

.event-row {
  display: flex;
  align-items: flex-start;
  gap: 13px;
  padding: 14px 4px;
  border-bottom: 1px solid #eef2fa;
}

.event-row:last-child {
  border-bottom: none;
}

.event-dot {
  width: 9px;
  height: 9px;
  border-radius: 50%;
  flex: none;
  margin-top: 6px;
  background: #98a2b3;
}

.event-dot.info {
  background: #2f6fe4;
}

.event-dot.warning {
  background: #f79009;
}

.event-dot.critical {
  background: #f04438;
}

.event-copy {
  min-width: 0;
  flex: 1;
}

.event-title {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  font-size: 13.5px;
  font-weight: 600;
  color: #132a56;
}

.state-pill {
  border-radius: 999px;
  padding: 1px 9px;
  font-size: 11px;
  font-weight: 600;
}

.state-pill.ok {
  background: #ecfdf3;
  color: #067647;
}

.state-pill.bad {
  background: #fef3f2;
  color: #b42318;
}

.state-pill.quiet {
  background: #f2f4f7;
  color: #475467;
}

.event-meta {
  display: block;
  margin-top: 3px;
  font-size: 12px;
  color: #8ba0bf;
}

.event-actions {
  flex: none;
}

/* The empty state ---------------------------------------------------------- */
.empty {
  padding: 46px 0;
  text-align: center;
  color: #667085;
  font-size: 13.5px;
}

/* The notifications ---------------------------------------------------------- */
.notify-panel {
  max-width: 720px;
}

.panel-title {
  margin: 0 0 16px;
  font-size: 15px;
  color: #132a56;
}

.notify-facts {
  margin: 0 0 20px;
  display: grid;
  gap: 10px;
}

.fact {
  display: grid;
  grid-template-columns: 8rem 1fr;
  align-items: baseline;
  gap: 12px;
}

.fact dt {
  font-size: 13px;
  color: #667085;
}

.fact dd {
  margin: 0;
  font-size: 13px;
  color: #132a56;
}

.fact dd code {
  font-size: 12.5px;
}

.break-all {
  overflow-wrap: anywhere;
}

.notify-form {
  display: grid;
  gap: 16px;
  border-top: 1px solid #eef2fa;
  padding-top: 18px;
}

.field {
  display: grid;
  gap: 6px;
}

.field label {
  font-size: 12px;
  font-weight: 600;
  color: #344054;
}

.field input {
  height: 38px;
  width: 100%;
  max-width: 520px;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 0 12px;
  font-size: 13px;
  color: #132a56;
  outline: none;
}

.field input:focus {
  border-color: #0255f0;
}

.field-hint {
  margin: 0;
  font-size: 12px;
  color: #8ba0bf;
}

.check {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13.5px;
  color: #344054;
}

.check input {
  width: 15px;
  height: 15px;
  accent-color: #0255f0;
}

.form-actions {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
}

/* The banners, the loading and the error of the page --------------------------- */
.banner {
  margin: 14px 0 0;
  padding: 10px 14px;
  border-radius: 10px;
  font-size: 13px;
}

.banner.success {
  background: #ecfdf3;
  color: #067647;
}

.page-loading {
  padding: 60px 0;
  text-align: center;
  color: #667085;
}

.page-error {
  padding: 40px 0;
  text-align: center;
  color: #d92d20;
}

.page-error .text-button {
  margin-top: 10px;
}

/* The spinner of the page ------------------------------------------------------- */
.spinner {
  display: inline-block;
  width: 18px;
  height: 18px;
  margin-right: 8px;
  vertical-align: -4px;
  border: 2px solid #cfdcf3;
  border-top-color: #0255f0;
  border-radius: 50%;
  animation: alerts-spin 0.8s linear infinite;
}

@keyframes alerts-spin {
  to {
    transform: rotate(360deg);
  }
}

/* The page is narrow ------------------------------------------------------------- */
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
