// The client of the rules of the alerts, of the events and of the target of
// the notifications.
//
// Every call of this module goes to the control端 of the same origin, so
// the session of the administrator travels in the cookie of the browser and
// no token ever appears in the address of a request. A request that writes
// carries the token of the session, because the control端 refuses a request
// without it.
import { api } from './client'
import type {
  AlertEvent,
  AlertRule,
  AlertRuleKind,
  WebhookBundle,
  WebhookPayload,
} from './types'

// AlertRuleInput describes the rule the administrator wants to save.
export interface AlertRuleInput {
  kind: AlertRuleKind
  // scope_id is the object the rule watches.
  scope_id: string
  // metric is the value a resource rule watches.
  metric: string
  // threshold is the limit of a resource rule.
  threshold: number
  // for_seconds is the time the condition has to hold.
  for_seconds: number
  enabled: boolean
}

// WebhookInput describes the target of the notifications.
//
// An address that the request does not carry keeps the address that is
// stored already.
export interface WebhookInput {
  url?: string
  allowed_private_cidrs: string[]
  enabled?: boolean
}

// listAlertRules reads the rules of the alerts.
export async function listAlertRules(): Promise<AlertRule[]> {
  const payload = await api<{ rules: AlertRule[] }>('/api/v1/alert-rules')
  return payload.rules
}

// createAlertRule writes a new rule.
export async function createAlertRule(input: AlertRuleInput): Promise<AlertRule> {
  return api<AlertRule>('/api/v1/alert-rules', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// updateAlertRule changes one rule.
export async function updateAlertRule(ruleID: string, input: AlertRuleInput): Promise<AlertRule> {
  return api<AlertRule>(`/api/v1/alert-rules/${encodeURIComponent(ruleID)}`, {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

// deleteAlertRule removes a rule.
//
// The events of the rule stay in the history: the administrator still reads
// what happened.
export async function deleteAlertRule(ruleID: string): Promise<void> {
  await api<void>(`/api/v1/alert-rules/${encodeURIComponent(ruleID)}`, { method: 'DELETE' })
}

// listAlerts reads the events of the alerts.
//
// The list holds the events that are still open when the administrator asks
// only for them.
export async function listAlerts(activeOnly = false): Promise<AlertEvent[]> {
  const query = activeOnly ? '?active=true' : ''
  const payload = await api<{ alerts: AlertEvent[] }>(`/api/v1/alerts${query}`)
  return payload.alerts
}

// acknowledgeAlert marks an event as seen.
//
// Acknowledging is not a recovery: the event keeps its state until the
// observation shows that the anomaly is over.
export async function acknowledgeAlert(eventID: string): Promise<void> {
  await api<{ status: string }>(`/api/v1/alerts/${encodeURIComponent(eventID)}/acknowledge`, {
    method: 'POST',
  })
}

// getWebhook reads the target of the notifications and the state of their
// queue.
//
// The answer holds the masked form of the address only, so the page never
// shows the address of the target in the clear.
export async function getWebhook(): Promise<WebhookBundle> {
  return api<WebhookBundle>('/api/v1/webhook')
}

// saveWebhook saves the target of the notifications.
export async function saveWebhook(input: WebhookInput): Promise<WebhookPayload> {
  return api<WebhookPayload>('/api/v1/webhook', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

// testWebhook sends a test notification to the target.
//
// The test leaves the control端, so it only happens when the administrator
// asks for it.
export async function testWebhook(): Promise<void> {
  await api<{ status: string }>('/api/v1/webhook/test', { method: 'POST' })
}
