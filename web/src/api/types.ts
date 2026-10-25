// The payloads of the HTTP API of the control端.
//
// The names of the fields are the names that the API uses, so a document of
// the API can be read without a translation. The JSON of the API is
// snake_case, and the frontend keeps that spelling.

// ApiError is a failed answer of the API.
//
// Every error of the API carries a code and a message. A validation error
// carries a message for every field that failed, so the form can show the
// reason next to the field.
export class ApiError extends Error {
  // The status code of the answer.
  readonly status: number
  // The code of the error, for example unauthenticated.
  readonly code: string
  // The messages of the fields that failed the validation.
  readonly fields: Record<string, string>

  constructor(status: number, code: string, message: string, fields: Record<string, string> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.fields = fields
  }
}

// SessionPayload is the answer of the session endpoint.
//
// The answer carries the CSRF token of the session and the time the session
// expires. The session token itself stays in the cookie of the browser.
export interface SessionPayload {
  csrf_token: string
  expires_at: string
}

// LoginPayload is the answer of a successful login. It carries the same
// values as the answer of the session endpoint.
export type LoginPayload = SessionPayload

// InitializePayload is the answer of a successful initialization.
export interface InitializePayload {
  initialized: boolean
}

// NodeSummary describes one enrolled node.
export interface NodeSummary {
  // The identifier of the node.
  id: string
  // The name of the host of the node.
  hostname: string
  // Whether the agent of the node is connected.
  connected: boolean
  // The number of containers of the last complete inventory.
  container_count: number
  // The time of the last heartbeat, or null while the node never sent one.
  last_seen: string | null
}

// NodeListPayload is the answer of the node list.
export interface NodeListPayload {
  nodes: NodeSummary[]
}

// AgentTokenPayload is the answer of the enrollment token endpoint.
//
// The token is only part of this answer, so it can be shown once and never
// again.
export interface AgentTokenPayload {
  token: string
  expires_at: string
}

// describeError returns a short message for the administrator.
//
// The messages of the API are meant for the user already, so only a failure
// that carries no message of the API gets a generic one. A technical detail
// stays in the log of the control端.
export function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    return error.message
  }
  return '操作失败，请稍后重试'
}

// formatTimestamp writes a moment of the API for the page.
//
// The API writes its moments in RFC 3339. A moment that cannot be read
// stays empty, so the page never shows a made up time.
export function formatTimestamp(value: string | null): string {
  if (!value) {
    return '—'
  }
  const moment = new Date(value)
  if (Number.isNaN(moment.getTime())) {
    return '—'
  }
  return moment.toLocaleString('zh-CN', { hour12: false })
}
