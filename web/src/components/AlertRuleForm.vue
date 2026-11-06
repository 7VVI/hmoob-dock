<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NAlert, NButton } from 'naive-ui'
import type { AlertRuleInput } from '../api/alerts'
import type { AlertRule, AlertRuleKind } from '../api/types'

const props = defineProps<{
  // rule is the rule the administrator changes. A rule that is not there
  // means a new rule.
  rule?: AlertRule | null
  // errors are the reasons of the fields that the control端 refused. The
  // form keeps its input, so the administrator can correct one field and
  // try again.
  errors?: Record<string, string>
  // busy reports that the request is on its way.
  busy?: boolean
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

// The metrics a resource rule may watch.
//
// The names are the names that the API uses, so the rule of the
// administrator means the same thing on the server.
const metrics: { value: string; label: string }[] = [
  { value: 'cpu_percent', label: 'CPU（每核 100%）' },
  { value: 'memory_bytes', label: '内存（字节）' },
  { value: 'net_rx_per_sec', label: '网络接收（字节/秒）' },
  { value: 'net_tx_per_sec', label: '网络发送（字节/秒）' },
  { value: 'io_read_per_sec', label: '磁盘读取（字节/秒）' },
  { value: 'io_write_per_sec', label: '磁盘写入（字节/秒）' },
]

const kind = ref<AlertRuleKind | ''>('')
const scope = ref('')
const metric = ref('cpu_percent')
const threshold = ref('0')
const duration = ref('0')
const enabled = ref(true)

// localError is the reason the form itself refuses to send the rule.
const localError = ref('')

// isResource reports whether the rule watches a value of a container.
const isResource = computed(() => kind.value === 'resource')

// scopeHint names the object the rule watches.
const scopeHint = computed(() =>
  kind.value === 'agent_offline'
    ? '填节点 ID，节点失联超过持续时间后开启告警。'
    : kind.value === 'container_exit'
      ? '填容器 ID，容器意外退出后开启告警。'
      : '填容器 ID，资源曲线是容器的，不是整台机器的。',
)

// load puts a rule into the form.
//
// A rule of a kind the form does not know keeps its kind out of the form:
// the administrator has to choose one again, so a rule is never saved with
// a kind that nobody chose.
function load(rule: AlertRule | null | undefined): void {
  localError.value = ''
  if (!rule) {
    kind.value = 'resource'
    scope.value = ''
    metric.value = 'cpu_percent'
    threshold.value = '0'
    duration.value = '0'
    enabled.value = true
    return
  }
  kind.value = kinds.some((item) => item.value === rule.kind) ? rule.kind : ''
  scope.value = rule.scope_id
  metric.value = rule.metric === '' ? 'cpu_percent' : rule.metric
  threshold.value = String(rule.threshold)
  duration.value = String(rule.for_seconds)
  enabled.value = rule.enabled
}

watch(() => props.rule, load, { immediate: true })

// numberOf reads a field of the form as a number.
function numberOf(value: string): number {
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

// save hands the rule to the caller.
//
// The form refuses a rule that names no object, and a resource rule that
// names no limit. The reason stays on the page and the input of the
// administrator is kept.
function save(): void {
  localError.value = ''
  if (kind.value === '') {
    localError.value = '请选择这条规则监视的类型。'
    return
  }
  if (scope.value.trim() === '') {
    localError.value = '请填写这条规则监视的对象。'
    return
  }
  if (isResource.value && numberOf(threshold.value) <= 0) {
    localError.value = '请填写一个大于零的阈值。'
    return
  }
  emit('save', {
    kind: kind.value,
    scope_id: scope.value.trim(),
    metric: isResource.value ? metric.value : '',
    threshold: isResource.value ? numberOf(threshold.value) : 0,
    for_seconds: numberOf(duration.value),
    enabled: enabled.value,
  })
}

// The native controls stay native: the tests and the keyboards read their
// labels. Tailwind gives them the face of the panel.
const selectClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
const inputClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
</script>

<template>
  <form class="grid max-w-xl gap-4" novalidate @submit.prevent="save">
    <div class="grid gap-1">
      <label class="text-xs font-medium text-[#344054]" for="rule-kind">类型</label>
      <select id="rule-kind" v-model="kind" :class="selectClass" name="kind">
        <option value="">请选择类型</option>
        <option v-for="item in kinds" :key="item.value" :value="item.value">{{ item.label }}</option>
      </select>
      <p v-if="errors?.kind" class="text-xs text-[#e02e2e]">{{ errors.kind }}</p>
    </div>

    <div class="grid gap-1">
      <label class="text-xs font-medium text-[#344054]" for="rule-scope">监视对象</label>
      <input
        id="rule-scope"
        v-model="scope"
        :class="inputClass"
        type="text"
        name="scope_id"
        autocomplete="off"
      />
      <p class="text-xs text-[#667085]">{{ scopeHint }}</p>
      <p v-if="errors?.scope_id" class="text-xs text-[#e02e2e]">{{ errors.scope_id }}</p>
    </div>

    <template v-if="isResource">
      <div class="grid gap-1">
        <label class="text-xs font-medium text-[#344054]" for="rule-metric">指标</label>
        <select id="rule-metric" v-model="metric" :class="selectClass" name="metric">
          <option v-for="item in metrics" :key="item.value" :value="item.value">{{ item.label }}</option>
        </select>
        <p v-if="errors?.metric" class="text-xs text-[#e02e2e]">{{ errors.metric }}</p>
      </div>

      <div class="grid gap-1">
        <label class="text-xs font-medium text-[#344054]" for="rule-threshold">阈值</label>
        <input
          id="rule-threshold"
          v-model="threshold"
          :class="inputClass"
          type="number"
          name="threshold"
          min="0"
          step="any"
        />
        <p class="text-xs text-[#667085]">超过这个值并持续到持续时间后开启告警。</p>
        <p v-if="errors?.threshold" class="text-xs text-[#e02e2e]">{{ errors.threshold }}</p>
      </div>
    </template>

    <div class="grid gap-1">
      <label class="text-xs font-medium text-[#344054]" for="rule-duration">持续时间（秒）</label>
      <input
        id="rule-duration"
        v-model="duration"
        :class="inputClass"
        type="number"
        name="for_seconds"
        min="0"
        step="1"
      />
      <p class="text-xs text-[#667085]">条件要保持多久才开启告警，0 表示立即开启。</p>
      <p v-if="errors?.for_seconds" class="text-xs text-[#e02e2e]">{{ errors.for_seconds }}</p>
    </div>

    <label class="flex items-center gap-2 text-sm">
      <input v-model="enabled" type="checkbox" name="enabled" class="h-4 w-4 accent-[#025ffb]" />
      <span>启用这条规则</span>
    </label>

    <NAlert v-if="localError" type="error" :show-icon="false">{{ localError }}</NAlert>

    <div class="flex flex-wrap gap-3">
      <NButton attr-type="submit" type="primary" :disabled="busy">保存</NButton>
      <NButton quaternary @click="emit('cancel')">取消</NButton>
    </div>
  </form>
</template>
