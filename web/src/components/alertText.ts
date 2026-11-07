// The words the console reads a rule with.
//
// The chip of a condition, the sentence under the title of a rule and the
// preview of the dialog all read the same rule, so they share the way a
// limit, an operator and a condition are written.
import type { AlertOperator, AlertRule, AlertRuleKind } from '../api/types'

// The kinds of the rules and the names the administrator reads.
export const kindLabels: Record<AlertRuleKind, string> = {
  resource: '容器负载',
  container_exit: '容器意外退出',
  agent_offline: '节点失联',
}

// The metrics a resource rule watches and the short names of a condition.
export const metricLabels: Record<string, string> = {
  cpu_percent: 'CPU',
  memory_bytes: '内存',
  net_rx_per_sec: '网络接收',
  net_tx_per_sec: '网络发送',
  io_read_per_sec: '磁盘读取',
  io_write_per_sec: '磁盘写入',
}

// operatorSymbols are the signs a comparison reads with.
const operatorSymbols: Record<AlertOperator, string> = {
  '>': '>',
  '>=': '≥',
  '<': '<',
  '<=': '≤',
}

// operatorSymbol writes the sign of a comparison.
export function operatorSymbol(operator: string): string {
  return operatorSymbols[operator as AlertOperator] ?? operator
}

// limitText writes the limit of a resource rule in the unit of its metric:
// the CPU in percent, a volume in bytes, a rate per second.
export function limitText(metric: string, value: number): string {
  if (metric === 'cpu_percent') {
    return `${value}%`
  }
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let scaled = value
  let unit = 0
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024
    unit += 1
  }
  const text = scaled >= 10 ? String(Math.round(scaled)) : scaled.toFixed(1)
  return metric === 'memory_bytes' ? `${text} ${units[unit]}` : `${text} ${units[unit]}/s`
}

// unitOf names the unit the dialog shows beside a threshold.
export function unitOf(metric: string): string {
  return metric === 'cpu_percent' ? '%' : metric === 'memory_bytes' ? 'KiB' : 'KiB/s'
}

// conditionText names the condition of a rule for the chip of the table.
export function conditionText(rule: AlertRule): string {
  if (rule.kind === 'container_exit') {
    return '容器退出'
  }
  if (rule.kind === 'agent_offline') {
    return '节点失联'
  }
  const label = metricLabels[rule.metric] ?? rule.metric
  return `${label} ${operatorSymbol(rule.operator)} ${limitText(rule.metric, rule.threshold)}`
}

// ruleDisplayName is the title of a rule. A rule without a name of its own
// falls back to the label of its kind.
export function ruleDisplayName(rule: AlertRule): string {
  const name = rule.name.trim()
  if (name !== '') {
    return name
  }
  return kindLabels[rule.kind] ?? rule.kind
}

// ruleSentence describes what a rule does, in one sentence.
export function ruleSentence(rule: AlertRule): string {
  switch (rule.kind) {
    case 'resource': {
      const label = metricLabels[rule.metric] ?? rule.metric
      const sign = operatorSymbol(rule.operator)
      const limit = limitText(rule.metric, rule.threshold)
      if (rule.for_seconds > 0) {
        return `当容器 ${label} 持续 ${rule.for_seconds} 秒 ${sign} ${limit} 时告警`
      }
      return `当容器 ${label} ${sign} ${limit} 时告警`
    }
    case 'container_exit':
      return '容器异常退出时立即告警'
    default:
      if (rule.for_seconds > 0) {
        return `节点失联持续 ${rule.for_seconds} 秒后告警`
      }
      return '节点失联后立即告警'
  }
}

// ruleDescription is the sentence under the title of a rule: the words of
// the administrator when the rule has a description, the derived sentence
// when it has none.
export function ruleDescription(rule: AlertRule): string {
  const description = rule.description.trim()
  return description !== '' ? description : ruleSentence(rule)
}

// rulePreview describes the rule the dialog is typing, so the rule says
// what it will do before it is saved.
export function rulePreview(rule: {
  kind: string
  metric: string
  operator: string
  threshold: number
  for_seconds: number
}): string {
  switch (rule.kind) {
    case 'container_exit':
      return '容器异常退出时立即触发告警'
    case 'agent_offline':
      if (rule.for_seconds > 0) {
        return `节点失联持续 ${rule.for_seconds} 秒后触发告警`
      }
      return '节点失联后立即触发告警'
    case 'resource': {
      const label = metricLabels[rule.metric] ?? rule.metric
      const sign = operatorSymbol(rule.operator)
      const limit = limitText(rule.metric, rule.threshold)
      if (rule.for_seconds > 0) {
        return `当容器 ${label} 连续 ${rule.for_seconds} 秒 ${sign} ${limit} 时触发告警`
      }
      return `当容器 ${label} ${sign} ${limit} 时立即触发告警`
    }
    default:
      return ''
  }
}
