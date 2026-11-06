<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NCheckbox } from 'naive-ui'
import AppShell from '../components/AppShell.vue'
import AlertRuleForm from '../components/AlertRuleForm.vue'
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
import {
  ApiError,
  describeError,
  formatTimestamp,
  type AlertEvent,
  type AlertRule,
  type NotificationStatus,
  type WebhookPayload,
} from '../api/types'

// The three kinds of the lists of the events.
//
// An event that the administrator confirmed is not an open anomaly any
// more, so it has a list of its own.
type Filter = 'active' | 'acknowledged' | 'resolved'

const filters: { key: Filter; label: string }[] = [
  { key: 'active', label: '当前异常' },
  { key: 'acknowledged', label: '已确认' },
  { key: 'resolved', label: '已恢复' },
]

// The kinds of the rules and the names the administrator reads.
const kindLabels: Record<string, string> = {
  resource: '容器负载',
  container_exit: '容器意外退出',
  agent_offline: '节点失联',
}

const filter = ref<Filter>('active')
const events = ref<AlertEvent[]>([])
const rules = ref<AlertRule[]>([])
const webhook = ref<WebhookPayload | null>(null)
const notifications = ref<NotificationStatus | null>(null)

const loading = ref(true)
const error = ref('')
const notice = ref('')

// The form of a rule. The form keeps its input, so a refused rule can be
// corrected and sent again.
const formOpen = ref(false)
const editing = ref<AlertRule | null>(null)
const ruleErrors = ref<Record<string, string>>({})
const ruleError = ref('')
const ruleBusy = ref(false)

// The confirmation of the administrator.
const acknowledging = ref('')
const acknowledgeError = ref('')
const removing = ref('')
const removeError = ref('')

// The target of the notifications.
const url = ref('')
const cidrs = ref('')
const enabled = ref(true)
const webhookError = ref('')
const webhookBusy = ref(false)
const testBusy = ref(false)
const testNotice = ref('')

// activeEvents are the anomalies that nobody looked at yet.
const activeEvents = computed(() =>
  events.value.filter((event) => event.state === 'active' && !event.acknowledged_at),
)

// acknowledgedEvents are the events the administrator confirmed.
//
// Confirming an event does not recover it, so an event that is still open
// stays open and is only marked as seen.
const acknowledgedEvents = computed(() =>
  events.value.filter((event) => event.acknowledged_at !== null),
)

// resolvedEvents are the anomalies that are over.
const resolvedEvents = computed(() =>
  events.value.filter((event) => event.state === 'resolved' && !event.acknowledged_at),
)

// shownEvents are the events of the list the administrator reads.
const shownEvents = computed(() => {
  if (filter.value === 'acknowledged') {
    return acknowledgedEvents.value
  }
  if (filter.value === 'resolved') {
    return resolvedEvents.value
  }
  return activeEvents.value
})

// countOf names the number of the events of a list.
function countOf(key: Filter): number {
  if (key === 'acknowledged') {
    return acknowledgedEvents.value.length
  }
  if (key === 'resolved') {
    return resolvedEvents.value.length
  }
  return activeEvents.value.length
}

// load reads the events, the rules and the target of the notifications.
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
    enabled.value = bundle.webhook.enabled
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

// openForm opens the form of a rule.
//
// A rule that is not there means a new rule, so the form starts with the
// values of a resource rule of a container.
function openForm(rule: AlertRule | null): void {
  editing.value = rule
  ruleErrors.value = {}
  ruleError.value = ''
  formOpen.value = true
}

// cancelForm closes the form without saving.
function cancelForm(): void {
  formOpen.value = false
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
    formOpen.value = false
    editing.value = null
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
      enabled: enabled.value,
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

// The face of the inputs of the form of the target.
const inputClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
</script>

<template>
  <AppShell>
    <h1 class="page-title">告警管理</h1>
    <p class="page-subtitle">查看异常事件与告警规则。确认只表示已查看，事件保留到恢复为止。</p>

    <p v-if="loading" class="text-[#667085]">正在读取告警…</p>

    <NAlert v-else-if="error" type="error" :show-icon="false">
      <p>{{ error }}</p>
      <NButton quaternary type="primary" class="mt-2" @click="load">重试</NButton>
    </NAlert>

    <template v-else>
      <NAlert v-if="notice" type="success" :show-icon="false" class="mb-6">{{ notice }}</NAlert>

      <section class="mb-6">
        <NCard title="异常">
          <template #header-extra>
            <NButton quaternary type="primary" @click="load">刷新</NButton>
          </template>

          <nav class="mb-4 flex flex-wrap gap-4 border-b border-[#e6ecf7]" aria-label="异常列表">
            <button
              v-for="item in filters"
              :key="item.key"
              type="button"
              class="border-0 border-b-2 border-transparent bg-transparent px-1 pb-2 text-sm text-[#667085]"
              :class="filter === item.key ? 'border-[#025ffb] font-semibold text-[#101828]' : 'cursor-pointer'"
              :aria-pressed="filter === item.key"
              @click="filter = item.key"
            >
              {{ item.label }}（{{ countOf(item.key) }}）
            </button>
          </nav>

          <p v-if="shownEvents.length === 0" class="py-6 text-center text-[#667085]">这里没有异常。</p>
          <ul v-else class="grid gap-3">
            <li
              v-for="event in shownEvents"
              :key="event.id"
              class="flex flex-wrap items-start justify-between gap-3 border-b border-[#e6ecf7] py-3 last:border-b-0"
            >
              <div class="grid min-w-0 flex-1 basis-72 gap-1">
                <p class="font-semibold">{{ event.summary }}</p>
                <p class="text-xs text-[#667085]">
                  对象 <code>{{ event.object_id }}</code> · 规则 <code>{{ event.rule_id }}</code> ·
                  开始于 {{ formatTimestamp(event.started_at) }}
                  <template v-if="event.resolved_at">
                    · 恢复于 {{ formatTimestamp(event.resolved_at) }}
                  </template>
                </p>
                <p v-if="event.acknowledged_at" class="text-xs text-[#667085]">
                  已确认于 {{ formatTimestamp(event.acknowledged_at) }}
                </p>
              </div>
              <div class="whitespace-nowrap">
                <NButton
                  v-if="!event.acknowledged_at"
                  quaternary
                  type="primary"
                  size="small"
                  :disabled="acknowledging !== ''"
                  @click="acknowledge(event)"
                >
                  确认
                </NButton>
                <span v-else class="text-xs text-[#667085]">已确认</span>
              </div>
            </li>
          </ul>

          <NAlert v-if="acknowledgeError" type="error" :show-icon="false" class="mt-4">{{ acknowledgeError }}</NAlert>
        </NCard>
      </section>

      <section class="mb-6">
        <NCard title="告警规则">
          <template #header-extra>
            <NButton quaternary type="primary" @click="openForm(null)">新建规则</NButton>
          </template>

          <p v-if="rules.length === 0" class="py-6 text-center text-[#667085]">还没有告警规则。</p>
          <ul v-else class="grid gap-3">
            <li
              v-for="rule in rules"
              :key="rule.id"
              class="flex flex-wrap items-start justify-between gap-3 border-b border-[#e6ecf7] py-3 last:border-b-0"
            >
              <div class="grid min-w-0 flex-1 basis-72 gap-1">
                <p class="font-semibold">{{ kindLabels[rule.kind] ?? rule.kind }}</p>
                <p class="text-xs text-[#667085]">
                  对象 <code>{{ rule.scope_id }}</code>
                  <template v-if="rule.kind === 'resource'">
                    · 指标 {{ rule.metric }} · 阈值 {{ rule.threshold }} · 持续
                    {{ rule.for_seconds }} 秒
                  </template>
                  <template v-else> · 持续 {{ rule.for_seconds }} 秒</template>
                  <template v-if="!rule.enabled"> · 已停用</template>
                </p>
              </div>
              <div class="flex items-center gap-2 whitespace-nowrap">
                <NButton quaternary type="primary" size="small" @click="openForm(rule)">编辑</NButton>
                <template v-if="removing !== rule.id">
                  <NButton quaternary type="error" size="small" @click="askForRemove(rule)">删除</NButton>
                </template>
                <template v-else>
                  <span class="text-xs text-[#667085]">确认删除？</span>
                  <NButton size="small" type="error" @click="remove(rule)">确认</NButton>
                  <NButton size="small" quaternary @click="cancelRemove">取消</NButton>
                </template>
              </div>
            </li>
          </ul>

          <NAlert v-if="removeError" type="error" :show-icon="false" class="mt-4">{{ removeError }}</NAlert>

          <template v-if="formOpen">
            <h3 class="mb-3 mt-6 text-base font-semibold">{{ editing ? '编辑规则' : '新建规则' }}</h3>
            <AlertRuleForm
              :rule="editing"
              :errors="ruleErrors"
              :busy="ruleBusy"
              @save="saveRule"
              @cancel="cancelForm"
            />
            <NAlert v-if="ruleError" type="error" :show-icon="false" class="mt-4">{{ ruleError }}</NAlert>
          </template>
        </NCard>
      </section>

      <section>
        <NCard title="通知">
          <dl class="mb-5 grid gap-3">
            <div class="grid grid-cols-[8rem_1fr] items-baseline gap-3">
              <dt class="text-[13px] text-[#667085]">通知地址</dt>
              <dd class="break-all">
                <template v-if="webhook?.configured">
                  <code>{{ webhook.url_masked }}</code>
                  <span class="text-xs text-[#667085]"> · 地址只显示脱敏后的形式</span>
                </template>
                <template v-else>还没有配置通知地址。</template>
              </dd>
            </div>
            <div class="grid grid-cols-[8rem_1fr] items-baseline gap-3">
              <dt class="text-[13px] text-[#667085]">允许的内网</dt>
              <dd class="break-all">
                <code v-if="webhook && webhook.allowed_private_cidrs.length > 0">
                  {{ webhook.allowed_private_cidrs.join(', ') }}
                </code>
                <template v-else>没有允许的内网地址。</template>
              </dd>
            </div>
            <div class="grid grid-cols-[8rem_1fr] items-baseline gap-3">
              <dt class="text-[13px] text-[#667085]">通知结果</dt>
              <dd v-if="notifications">
                等待中 {{ notifications.pending }} · 已送达 {{ notifications.delivered }} · 失败
                {{ notifications.failed }} · 已跳过 {{ notifications.skipped }}
                <span v-if="notifications.last_delivered_at" class="text-xs text-[#667085]">
                  · 最近送达于 {{ formatTimestamp(notifications.last_delivered_at) }}
                </span>
              </dd>
              <dd v-else>—</dd>
            </div>
          </dl>

          <form class="grid max-w-2xl gap-4" novalidate @submit.prevent="saveTarget">
            <div class="grid gap-1">
              <label class="text-xs font-medium text-[#344054]" for="webhook-url">新的通知地址</label>
              <input
                id="webhook-url"
                v-model="url"
                :class="inputClass"
                type="url"
                name="webhook-url"
                autocomplete="off"
                placeholder="留空表示保持现在的地址"
              />
              <p class="text-xs text-[#667085]">地址里可以带凭据，平台只以脱敏的形式显示它。</p>
            </div>

            <div class="grid gap-1">
              <label class="text-xs font-medium text-[#344054]" for="webhook-cidrs">新增允许的内网网段</label>
              <input
                id="webhook-cidrs"
                v-model="cidrs"
                :class="inputClass"
                type="text"
                name="webhook-cidrs"
                autocomplete="off"
                placeholder="例如 10.0.0.0/8"
              />
              <p class="text-xs text-[#667085]">
                允许内网地址后，通知会发送到这些网段里的地址。只填写你信任的网段，平台不会把通知转发到其它地方。
              </p>
            </div>

            <NCheckbox v-model:checked="enabled">启用通知</NCheckbox>

            <NAlert v-if="webhookError" type="error" :show-icon="false">{{ webhookError }}</NAlert>
            <NAlert v-if="testNotice" type="success" :show-icon="false">{{ testNotice }}</NAlert>

            <div class="flex flex-wrap gap-3">
              <NButton type="primary" attr-type="submit" :loading="webhookBusy">保存通知设置</NButton>
              <NButton
                quaternary
                type="primary"
                :disabled="testBusy || !webhook?.configured"
                :loading="testBusy"
                @click="sendTest"
              >
                发送测试通知
              </NButton>
            </div>
          </form>
        </NCard>
      </section>
    </template>
  </AppShell>
</template>
