<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
</script>

<template>
  <form class="form" novalidate @submit.prevent="save">
    <div class="field">
      <label class="field__label" for="rule-kind">类型</label>
      <select id="rule-kind" v-model="kind" class="field__input" name="kind">
        <option value="">请选择类型</option>
        <option v-for="item in kinds" :key="item.value" :value="item.value">{{ item.label }}</option>
      </select>
      <p v-if="errors?.kind" class="field__error">{{ errors.kind }}</p>
    </div>

    <div class="field">
      <label class="field__label" for="rule-scope">监视对象</label>
      <input
        id="rule-scope"
        v-model="scope"
        class="field__input"
        type="text"
        name="scope_id"
        autocomplete="off"
      />
      <p class="field__hint">{{ scopeHint }}</p>
      <p v-if="errors?.scope_id" class="field__error">{{ errors.scope_id }}</p>
    </div>

    <template v-if="isResource">
      <div class="field">
        <label class="field__label" for="rule-metric">指标</label>
        <select id="rule-metric" v-model="metric" class="field__input" name="metric">
          <option v-for="item in metrics" :key="item.value" :value="item.value">{{ item.label }}</option>
        </select>
        <p v-if="errors?.metric" class="field__error">{{ errors.metric }}</p>
      </div>

      <div class="field">
        <label class="field__label" for="rule-threshold">阈值</label>
        <input
          id="rule-threshold"
          v-model="threshold"
          class="field__input"
          type="number"
          name="threshold"
          min="0"
          step="any"
        />
        <p class="field__hint">超过这个值并持续到持续时间后开启告警。</p>
        <p v-if="errors?.threshold" class="field__error">{{ errors.threshold }}</p>
      </div>
    </template>

    <div class="field">
      <label class="field__label" for="rule-duration">持续时间（秒）</label>
      <input
        id="rule-duration"
        v-model="duration"
        class="field__input"
        type="number"
        name="for_seconds"
        min="0"
        step="1"
      />
      <p class="field__hint">条件要保持多久才开启告警，0 表示立即开启。</p>
      <p v-if="errors?.for_seconds" class="field__error">{{ errors.for_seconds }}</p>
    </div>

    <div class="field">
      <label class="field__check">
        <input v-model="enabled" type="checkbox" name="enabled" />
        <span>启用这条规则</span>
      </label>
    </div>

    <p v-if="localError" class="alert" role="alert">{{ localError }}</p>

    <div class="rule__actions">
      <button class="button button--primary" type="submit" :disabled="busy">保存</button>
      <button class="button button--quiet" type="button" @click="emit('cancel')">取消</button>
    </div>
  </form>
</template>

<style scoped>
.rule__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
}

.field__check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.9rem;
}
</style>
