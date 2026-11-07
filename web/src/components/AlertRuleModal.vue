<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import type { AlertRuleInput } from '../api/alerts'
import type {
  AlertOperator,
  AlertRule,
  AlertRuleKind,
  AlertScopeKind,
  AlertSeverity,
} from '../api/types'
import { rulePreview, unitOf } from './alertText'

const props = defineProps<{
  // open reports whether the dialog is on the page.
  open?: boolean
  // rule is the rule the dialog shows. A rule that is not there means a
  // new rule.
  rule?: AlertRule | null
  // editing reports that the dialog changes an existing rule, so the
  // title and the button of the dialog name a change.
  editing?: boolean
  // errors are the reasons of the fields that the control端 refused. The
  // dialog keeps its input, so the administrator can correct one field and
  // try again.
  errors?: Record<string, string>
  // busy reports that the request is on its way.
  busy?: boolean
  // error is the reason the control端 refused the rule as a whole.
  error?: string
}>()

const emit = defineEmits<{
  save: [input: AlertRuleInput]
  cancel: []
}>()

// The kinds of the rules the platform knows.
const kinds: { value: AlertRuleKind; label: string }[] = [
  { value: 'resource', label: '容器负载' },
  { value: 'container_exit', label: '容器意外退出' },
  { value: 'agent_offline', label: '节点失联' },
]

// The scopes a rule of a container may watch.
const scopeKinds: { value: AlertScopeKind; label: string }[] = [
  { value: 'all', label: '全部容器' },
  { value: 'container', label: '指定容器' },
  { value: 'namespace', label: '指定命名空间' },
]

// The severities of a rule, in the order the administrator reads them.
const severities: { value: AlertSeverity; label: string }[] = [
  { value: 'info', label: '提示' },
  { value: 'warning', label: '警告' },
  { value: 'critical', label: '严重' },
]

// The operators a resource rule compares with, in the order of the
// console.
const operators: { value: string; label: string }[] = [
  { value: '>', label: '大于' },
  { value: '>=', label: '大于等于' },
  { value: '<', label: '小于' },
  { value: '<=', label: '小于等于' },
]

// The metrics a resource rule may watch.
//
// The names are the names that the API uses, so the rule of the
// administrator means the same thing on the server.
const metrics: { value: string; label: string }[] = [
  { value: 'cpu_percent', label: 'CPU 使用率' },
  { value: 'memory_bytes', label: '内存' },
  { value: 'net_rx_per_sec', label: '网络接收' },
  { value: 'net_tx_per_sec', label: '网络发送' },
  { value: 'io_read_per_sec', label: '磁盘读取' },
  { value: 'io_write_per_sec', label: '磁盘写入' },
]

const name = ref('')
const description = ref('')
const kind = ref<AlertRuleKind | ''>('')
const severity = ref<AlertSeverity>('warning')
const scopeKind = ref<AlertScopeKind>('container')
const scope = ref('')
const metric = ref('cpu_percent')
const operator = ref<AlertOperator>('>')
const threshold = ref('0')
const duration = ref('0')
const recoverNotify = ref(true)

// localError is the reason the dialog itself refuses to send the rule.
const localError = ref('')
// nameInput receives the focus when the dialog opens.
const nameInput = ref<HTMLInputElement | null>(null)

// isResource reports whether the rule watches a value of a container.
const isResource = computed(() => kind.value === 'resource')

// isNodeRule reports whether the rule watches a node. A rule of a lost
// agent names its node and has no breadth of containers.
const isNodeRule = computed(() => kind.value === 'agent_offline')

// needsScopeInput reports whether the dialog asks for the object the rule
// watches: a container, a namespace or a node.
const needsScopeInput = computed(() =>
  isNodeRule.value ? true : scopeKind.value !== 'all',
)

// scopeLabel names the object the dialog asks for.
const scopeLabel = computed(() => {
  if (isNodeRule.value) {
    return '监视节点'
  }
  return scopeKind.value === 'namespace' ? '命名空间名称' : '容器 ID'
})

// scopePlaceholder names what the administrator writes into the object.
const scopePlaceholder = computed(() => {
  if (isNodeRule.value) {
    return '请输入节点 ID'
  }
  return scopeKind.value === 'namespace' ? '请输入命名空间名称' : '请输入容器 ID'
})

// scopeHint explains the object the dialog asks for.
const scopeHint = computed(() => {
  if (isNodeRule.value) {
    return '节点失联超过持续时间后开启告警。'
  }
  if (scopeKind.value === 'namespace') {
    return '命名空间是 Compose 项目或 Swarm 栈的名称。'
  }
  return '资源曲线是容器的，不是整台机器的。'
})

// unitLabel names the unit of the threshold of the chosen metric.
const unitLabel = computed(() => unitOf(metric.value))

// preview describes the rule the administrator is typing, so the rule says
// what it will do before it is saved.
const preview = computed(() => {
  if (kind.value === '') {
    return ''
  }
  return rulePreview({
    kind: kind.value,
    metric: metric.value,
    operator: operator.value,
    threshold: Number(threshold.value) || 0,
    for_seconds: Number(duration.value) || 0,
  })
})

// load puts a rule into the dialog.
//
// A rule of a kind the dialog does not know keeps its kind out of the
// fields: the administrator has to choose one again, so a rule is never
// saved with a kind that nobody chose.
function load(rule: AlertRule | null | undefined): void {
  localError.value = ''
  if (!rule) {
    name.value = ''
    description.value = ''
    kind.value = 'resource'
    severity.value = 'warning'
    scopeKind.value = 'container'
    scope.value = ''
    metric.value = 'cpu_percent'
    operator.value = '>'
    threshold.value = '0'
    duration.value = '0'
    recoverNotify.value = true
    return
  }
  name.value = rule.name
  description.value = rule.description
  kind.value = kinds.some((item) => item.value === rule.kind) ? rule.kind : ''
  severity.value = severities.some((item) => item.value === rule.severity)
    ? rule.severity
    : 'warning'
  scopeKind.value = ['all', 'container', 'namespace'].includes(rule.scope_kind)
    ? rule.scope_kind
    : 'container'
  scope.value = rule.scope_id
  metric.value = rule.metric === '' ? 'cpu_percent' : rule.metric
  operator.value = ['>', '>=', '<', '<='].includes(rule.operator) ? rule.operator : '>'
  threshold.value = String(rule.threshold)
  duration.value = String(rule.for_seconds)
  recoverNotify.value = rule.recover_notify
}

watch(
  () => [props.open, props.rule] as const,
  ([open]) => {
    if (open) {
      load(props.rule)
      void nextTick(() => nameInput.value?.focus())
    }
  },
  { immediate: true },
)

// numberOf reads a field of the dialog as a number.
function numberOf(value: string): number {
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

// save hands the rule to the caller.
//
// The dialog refuses a rule without a name, a rule whose scope names no
// object, and a resource rule that names no limit. The reason stays on the
// dialog and the input of the administrator is kept.
function save(): void {
  localError.value = ''
  if (name.value.trim() === '') {
    localError.value = '请填写这条规则的名称。'
    return
  }
  if (kind.value === '') {
    localError.value = '请选择这条规则监视的类型。'
    return
  }
  if (needsScopeInput.value && scope.value.trim() === '') {
    localError.value = '请填写这条规则监视的对象。'
    return
  }
  if (isResource.value && numberOf(threshold.value) <= 0) {
    localError.value = '请填写一个大于零的阈值。'
    return
  }
  emit('save', {
    name: name.value.trim(),
    description: description.value.trim(),
    kind: kind.value,
    severity: severity.value,
    scope_kind: isNodeRule.value ? 'container' : scopeKind.value,
    scope_id: scope.value.trim(),
    metric: isResource.value ? metric.value : '',
    operator: isResource.value
      ? (operators.some((item) => item.value === operator.value) ? operator.value : '>')
      : '>',
    threshold: isResource.value ? numberOf(threshold.value) : 0,
    for_seconds: numberOf(duration.value),
    // A rule that is created here is evaluated. The pause of a rule lives
    // on the switch of the table: an edit never silently re-enables a
    // rule the administrator paused.
    enabled: props.rule?.enabled ?? true,
    recover_notify: recoverNotify.value,
  })
}

// close asks the caller to close the dialog.
function close(): void {
  emit('cancel')
}

// The native controls stay native: the tests and the keyboards read their
// labels. Tailwind gives them the face of the dialog.
const selectClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
const inputClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay" @click.self="close" @keydown.escape="close">
      <div class="dialog" role="dialog" aria-modal="true" :aria-label="editing ? '编辑告警规则' : '创建告警规则'">
        <header class="dialog-head">
          <span class="head-icon" aria-hidden="true">
            <svg viewBox="0 0 24 24">
              <path d="M18 8a6 6 0 0 0-12 0c0 6-3 8-3 8h18s-3-2-3-8" fill="currentColor" />
              <path d="M10.3 20a2 2 0 0 0 3.4 0" fill="currentColor" />
            </svg>
          </span>
          <h2>{{ editing ? '编辑告警规则' : '创建告警规则' }}</h2>
          <button type="button" class="close" aria-label="关闭" @click="close">
            <svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18" /></svg>
          </button>
        </header>

        <form class="dialog-body" novalidate @submit.prevent="save">
          <section class="block">
            <h3 class="block-title">基础信息</h3>

            <div class="field stacked">
              <label class="label" for="rule-name">规则名称 <b class="required">*</b></label>
              <div class="counter-input">
                <input
                  id="rule-name"
                  ref="nameInput"
                  v-model="name"
                  :class="inputClass"
                  type="text"
                  name="name"
                  maxlength="50"
                  autocomplete="off"
                  placeholder="请输入规则名称，例如：容器 CPU 使用率告警"
                />
                <span class="counter">{{ name.length }} / 50</span>
              </div>
              <p v-if="errors?.name" class="field-error">{{ errors.name }}</p>
            </div>

            <div class="field stacked">
              <label class="label" for="rule-description">规则描述</label>
              <div class="counter-area">
                <textarea
                  id="rule-description"
                  v-model="description"
                  class="area"
                  name="description"
                  maxlength="200"
                  rows="3"
                  placeholder="请输入规则描述（选填）"
                ></textarea>
                <span class="counter">{{ description.length }} / 200</span>
              </div>
              <p v-if="errors?.description" class="field-error">{{ errors.description }}</p>
            </div>

            <div class="field inline">
              <span class="label">级别 <b class="required">*</b></span>
              <div class="control">
                <div class="radio-row" role="radiogroup" aria-label="级别">
                  <label v-for="item in severities" :key="item.value" class="radio-pill">
                    <input v-model="severity" type="radio" name="severity" :value="item.value" />
                    <span>{{ item.label }}</span>
                  </label>
                </div>
                <p v-if="errors?.severity" class="field-error">{{ errors.severity }}</p>
              </div>
            </div>
          </section>

          <section class="block">
            <h3 class="block-title">监控对象</h3>

            <div class="field inline">
              <span class="label">告警类型 <b class="required">*</b></span>
              <div class="control">
                <div class="radio-row" role="radiogroup" aria-label="规则类型">
                  <label v-for="item in kinds" :key="item.value" class="radio-pill">
                    <input v-model="kind" type="radio" name="kind" :value="item.value" />
                    <span>{{ item.label }}</span>
                  </label>
                </div>
                <p v-if="errors?.kind" class="field-error">{{ errors.kind }}</p>
              </div>
            </div>

            <div v-if="!isNodeRule" class="field inline">
              <span class="label">监控范围 <b class="required">*</b></span>
              <div class="control">
                <div class="radio-row" role="radiogroup" aria-label="监控范围">
                  <label v-for="item in scopeKinds" :key="item.value" class="radio-pill">
                    <input v-model="scopeKind" type="radio" name="scope_kind" :value="item.value" />
                    <span>{{ item.label }}</span>
                  </label>
                </div>
                <p v-if="errors?.scope_kind" class="field-error">{{ errors.scope_kind }}</p>
              </div>
            </div>

            <div v-if="needsScopeInput" class="field inline">
              <label class="label" for="rule-scope">{{ scopeLabel }} <b class="required">*</b></label>
              <div class="control">
                <input
                  id="rule-scope"
                  v-model="scope"
                  :class="inputClass"
                  type="text"
                  name="scope_id"
                  autocomplete="off"
                  :placeholder="scopePlaceholder"
                />
                <p class="field-hint">{{ scopeHint }}</p>
                <p v-if="errors?.scope_id" class="field-error">{{ errors.scope_id }}</p>
              </div>
            </div>
          </section>

          <section class="block">
            <h3 class="block-title">告警条件</h3>

            <div v-if="isResource" class="field inline">
              <label class="label" for="rule-metric">监控指标 <b class="required">*</b></label>
              <div class="control">
                <div class="metric-select">
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M4 19h16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
                    <path d="m6 15 3.5-4.5 3 2.5L17 8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />
                  </svg>
                  <select id="rule-metric" v-model="metric" :class="selectClass" name="metric">
                    <option v-for="item in metrics" :key="item.value" :value="item.value">
                      {{ item.label }}
                    </option>
                  </select>
                </div>
                <p v-if="errors?.metric" class="field-error">{{ errors.metric }}</p>
              </div>
            </div>

            <div v-if="isResource" class="field inline">
              <span class="label">触发条件 <b class="required">*</b></span>
              <div class="control">
                <div class="condition-row">
                  <select v-model="operator" class="operator-select" name="operator" aria-label="比较方式">
                    <option v-for="item in operators" :key="item.value" :value="item.value">
                      {{ item.label }}
                    </option>
                  </select>
                  <input
                    v-model="threshold"
                    :class="inputClass"
                    type="number"
                    name="threshold"
                    min="0"
                    step="any"
                    aria-label="阈值"
                  />
                  <span class="unit">{{ unitLabel }}</span>
                </div>
                <p v-if="errors?.operator" class="field-error">{{ errors.operator }}</p>
                <p v-if="errors?.threshold" class="field-error">{{ errors.threshold }}</p>
              </div>
            </div>

            <div class="field inline">
              <label class="label" for="rule-duration">持续时间 <b class="required">*</b></label>
              <div class="control">
                <div class="condition-row">
                  <input
                    id="rule-duration"
                    v-model="duration"
                    class="duration-input"
                    :class="inputClass"
                    type="number"
                    name="for_seconds"
                    min="0"
                    step="1"
                  />
                  <span class="unit">秒</span>
                  <span class="inline-hint">
                    <svg viewBox="0 0 24 24" aria-hidden="true">
                      <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.8" />
                      <path d="M12 8v4.6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
                      <circle cx="12" cy="15.8" r="1" fill="currentColor" />
                    </svg>
                    满足条件持续多长时间后触发告警
                  </span>
                </div>
                <p v-if="errors?.for_seconds" class="field-error">{{ errors.for_seconds }}</p>
              </div>
            </div>

            <p v-if="preview" class="preview">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.8" />
                <path d="M12 8v4.6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
                <circle cx="12" cy="15.8" r="1" fill="currentColor" />
              </svg>
              {{ preview }}
            </p>
          </section>

          <section class="block divided">
            <h3 class="block-title">告警通知</h3>

            <div class="field inline">
              <span class="label">通知方式 <b class="required">*</b></span>
              <div class="control">
                <p class="notify-note">
                  告警通过全局 Webhook 发送通知，目标可在「通知设置」页签配置。
                </p>
              </div>
            </div>

            <div class="field inline">
              <span class="label">恢复通知</span>
              <div class="control">
                <div class="switch-row">
                  <button
                    type="button"
                    class="switch"
                    role="switch"
                    :aria-checked="recoverNotify"
                    aria-label="条件恢复后发送恢复通知"
                    @click="recoverNotify = !recoverNotify"
                  >
                    <span class="knob" aria-hidden="true"></span>
                  </button>
                  <span class="switch-copy">条件恢复后发送恢复通知</span>
                </div>
                <p v-if="errors?.recover_notify" class="field-error">{{ errors.recover_notify }}</p>
              </div>
            </div>
          </section>

          <p v-if="localError" class="dialog-error">{{ localError }}</p>
          <p v-if="error" class="dialog-error">{{ error }}</p>

          <footer class="dialog-foot">
            <button type="button" class="ghost-button" @click="close">取消</button>
            <button type="submit" class="primary-button" :disabled="busy">
              {{ editing ? '确认保存' : '创建规则' }}
            </button>
          </footer>
        </form>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
/* The overlay and the dialog ------------------------------------------- */
.overlay {
  position: fixed;
  inset: 0;
  z-index: 80;
  display: grid;
  place-items: center;
  padding: 24px;
  background: rgb(12 24 48 / 45%);
  backdrop-filter: blur(2px);
}

.dialog {
  width: 100%;
  max-width: 660px;
  max-height: min(88vh, 900px);
  display: flex;
  flex-direction: column;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 24px 60px -24px rgb(12 24 48 / 45%);
  overflow: hidden;
}

.dialog-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex: none;
  padding: 18px 22px;
  border-bottom: 1px solid #eef2fa;
  background: linear-gradient(120deg, #f6faff 0%, #fff 55%, #eef5ff 100%);
}

.head-icon {
  width: 38px;
  height: 38px;
  display: grid;
  place-items: center;
  border-radius: 11px;
  background: #e9f2ff;
  color: #146ef5;
  flex: none;
}

.head-icon svg {
  width: 20px;
  height: 20px;
}

.dialog-head h2 {
  margin: 0;
  font-size: 17px;
  font-weight: 700;
  color: #132a56;
  letter-spacing: 0.01em;
}

.close {
  appearance: none;
  margin-left: auto;
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  border: none;
  border-radius: 8px;
  background: none;
  color: #667085;
  cursor: pointer;
}

.close:hover {
  background: #f2f4f7;
  color: #132a56;
}

.close svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 2;
  stroke-linecap: round;
}

/* The body of the dialog ------------------------------------------------ */
.dialog-body {
  overflow-y: auto;
  padding: 4px 22px 22px;
}

.block {
  padding-top: 14px;
}

.block + .block {
  margin-top: 6px;
}

.block.divided {
  margin-top: 14px;
  padding-top: 18px;
  border-top: 1px solid #eef2fa;
}

.block-title {
  margin: 0 0 14px;
  padding-left: 9px;
  border-left: 3px solid #0255f0;
  font-size: 13.5px;
  font-weight: 700;
  color: #132a56;
  line-height: 1.3;
}

/* The fields: a stacked one names itself above its control, an inline one
   carries its label in the left column of the row. */
.field {
  margin-bottom: 14px;
}

.field.stacked {
  display: grid;
  gap: 7px;
}

.field.inline {
  display: grid;
  grid-template-columns: 96px 1fr;
  gap: 14px;
  align-items: start;
}

.field.inline > .label {
  padding-top: 9px;
}

.label {
  font-size: 12.5px;
  font-weight: 600;
  color: #344054;
  white-space: nowrap;
}

.required {
  color: #f04438;
  font-weight: 700;
}

.control {
  display: grid;
  gap: 6px;
  min-width: 0;
}

.counter-input {
  position: relative;
}

.counter-input .counter {
  position: absolute;
  right: 12px;
  top: 50%;
  transform: translateY(-50%);
  font-size: 11.5px;
  color: #98a2b3;
  font-variant-numeric: tabular-nums;
  pointer-events: none;
}

.counter-input input {
  padding-right: 56px;
}

.counter-area {
  position: relative;
}

.counter-area .counter {
  position: absolute;
  right: 12px;
  bottom: 10px;
  font-size: 11.5px;
  color: #98a2b3;
  font-variant-numeric: tabular-nums;
  pointer-events: none;
}

.area {
  width: 100%;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 9px 12px 26px;
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

.field-error {
  margin: 0;
  font-size: 12px;
  color: #d92d20;
}

/* The radio pills -------------------------------------------------------- */
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
  transition: border-color 120ms ease, background 120ms ease, color 120ms ease;
}

.radio-pill input {
  width: 14px;
  height: 14px;
  accent-color: #0255f0;
  margin: 0;
}

.radio-pill:has(input:checked) {
  border-color: #0255f0;
  background: #f0f5ff;
  color: #0255f0;
  font-weight: 600;
}

/* The rows of a condition -------------------------------------------------- */
.condition-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.operator-select {
  height: 38px;
  width: 128px;
  border: 1px solid #d3dcec;
  border-radius: 10px;
  background: #fff;
  padding: 0 10px;
  font-size: 13px;
  color: #132a56;
  outline: none;
}

.operator-select:focus {
  border-color: #0255f0;
}

.condition-row input {
  max-width: 160px;
}

.duration-input {
  max-width: 120px !important;
}

.unit {
  flex: none;
  font-size: 13px;
  color: #667085;
}

.inline-hint {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #8ba0bf;
}

.inline-hint svg {
  width: 14px;
  height: 14px;
  flex: none;
}

/* The metric select with its icon --------------------------------------------- */
.metric-select {
  position: relative;
  max-width: 240px;
}

.metric-select svg {
  position: absolute;
  left: 11px;
  top: 50%;
  transform: translateY(-50%);
  width: 15px;
  height: 15px;
  color: #146ef5;
  pointer-events: none;
}

.metric-select select {
  padding-left: 32px;
}

/* The preview of the condition ---------------------------------------------- */
.preview {
  display: flex;
  align-items: flex-start;
  gap: 9px;
  margin: 2px 0 0 110px;
  padding: 10px 14px;
  border-radius: 10px;
  background: #f0f5ff;
  color: #1d4fd8;
  font-size: 12.5px;
}

.preview svg {
  width: 15px;
  height: 15px;
  flex: none;
  margin-top: 1px;
}

.notify-note {
  margin: 0;
  padding: 10px 14px;
  border-radius: 10px;
  background: #f7faff;
  border: 1px dashed #c9dcf8;
  color: #475467;
  font-size: 12.5px;
}

/* The switch of the recovery -------------------------------------------------- */
.switch-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

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
  flex: none;
}

.switch[aria-checked='true'] {
  background: #0255f0;
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

.switch-copy {
  font-size: 12.5px;
  color: #475467;
}

/* The errors and the footer of the dialog -------------------------------------- */
.dialog-error {
  margin: 4px 0 0;
  font-size: 12.5px;
  color: #d92d20;
}

.dialog-foot {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  margin-top: 20px;
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

.primary-button {
  appearance: none;
  border: none;
  border-radius: 10px;
  background: #0255f0;
  color: #fff;
  font-size: 14px;
  font-weight: 600;
  padding: 9px 22px;
  cursor: pointer;
}

.primary-button:hover {
  background: #0249d0;
}

.primary-button:disabled {
  opacity: 0.6;
  cursor: default;
}

@media (max-width: 640px) {
  .overlay {
    padding: 12px;
  }

  .field.inline {
    grid-template-columns: 1fr;
    gap: 7px;
  }

  .field.inline > .label {
    padding-top: 0;
  }

  .preview {
    margin-left: 0;
  }
}
</style>
