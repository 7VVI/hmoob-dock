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

// TaskState is the phase of a deployment task.
//
// The phases are the ones the control端 knows: a task waits for an
// executioner, checks the target, changes it, waits for the services to
// become ready and ends. A task whose outcome could not be proven is
// uncertain, which is neither a success nor a failure.
export type TaskState =
  | 'queued'
  | 'preflight'
  | 'running'
  | 'verifying'
  | 'succeeded'
  | 'failed'
  | 'uncertain'

// AppMode is the runtime of an application.
export type AppMode = 'compose' | 'swarm'

// AppConfig is the configuration of an application.
//
// The variables belong to the configuration and not to the machine of the
// administrator, so the same configuration produces the same deployment on
// every node.
export interface AppConfig {
  yaml: string
  variables: Record<string, string>
}

// AppSummary describes one application without its configuration.
export interface AppSummary {
  id: string
  name: string
  mode: AppMode
  target_id: string
  namespace: string
  // latest_version_id is the version that was submitted last.
  latest_version_id: string
  // successful_version_id is the version that was deployed successfully
  // last.
  successful_version_id: string
}

// ApplicationPayload is an application with the configuration of its latest
// version.
export interface ApplicationPayload extends AppSummary {
  config: AppConfig
}

// ApplicationListPayload is the answer of the application list.
export interface ApplicationListPayload {
  applications: AppSummary[]
}

// PreviewResult is the impact of a configuration on the target.
//
// The digest identifies the configuration the preview was computed from. A
// deployment has to name it, so a submission can be told apart from a
// configuration nobody looked at.
export interface PreviewResult {
  digest: string
  removed_services: string[]
  warnings: string[]
  risk_warnings: string[]
  inventory_revision: string
  requires_confirmation: boolean
}

// VersionSummary describes a version of an application without its
// configuration.
export interface VersionSummary {
  id: string
  created_at: string
}

// VersionPayload is a version of an application with its configuration.
export interface VersionPayload {
  id: string
  created_at: string
  config: AppConfig
}

// VersionListPayload is the answer of the version list.
export interface VersionListPayload {
  versions: VersionSummary[]
  total: number
  limit: number
  offset: number
}

// Task is the answer of a submission of a deployment.
//
// The task is the unit the control端 schedules: it holds the phase it is in
// and the version it deploys.
export interface Task {
  id: string
  app_id: string
  version_id: string
  agent_id: string
  state: TaskState
  last_seq: number
  digest: string
}

// ManualResolution is the request of an administrator to end a task by hand.
export interface ManualResolution {
  seq: number
  reason: string
  // pending reports that the node did not confirm the request yet.
  pending: boolean
}

// Deployment is the state of a deployment task.
export interface Deployment {
  id: string
  app_id: string
  version_id: string
  agent_id: string
  state: TaskState
  last_seq: number
  result_code: string
  result_message: string
  rolled_back: boolean
  partial_update: boolean
  dispatch_started_at: string | null
  manual_resolution: ManualResolution | null
  created_at: string
  updated_at: string
  finished_at: string | null
}

// OutputFragment is one fragment of the output of a deployment.
//
// The data of a fragment is Base64, because the API sends the bytes of the
// command line of Docker and JSON has no other way to hold them.
export interface OutputFragment {
  offset: number
  data: string
  truncated: boolean
}

// DeploymentOutput is the output of a deployment.
export interface DeploymentOutput {
  task_id: string
  fragments: OutputFragment[]
  total_bytes: number
  truncated: boolean
}

// ConfigFailure is one field of a configuration the control端 refused.
//
// The path names the place in the document and the line the place in the
// text, so the administrator sees where the document has to change.
export interface ConfigFailure {
  path: string
  message: string
  line?: number
}

// configFailures returns the fields of a refused configuration.
//
// The answer of the API carries the fields of a validation error as a list
// of objects, while the other validation errors carry them as a map. Both
// shapes are read here, so the page can show the places of the document
// that have to change.
export function configFailures(error: unknown): ConfigFailure[] {
  if (!(error instanceof ApiError)) {
    return []
  }
  const fields: unknown = error.fields
  if (!Array.isArray(fields)) {
    return []
  }
  const failures: ConfigFailure[] = []
  for (const field of fields) {
    if (typeof field !== 'object' || field === null) {
      continue
    }
    const entry = field as { path?: unknown; message?: unknown; line?: unknown }
    failures.push({
      path: typeof entry.path === 'string' ? entry.path : '',
      message: typeof entry.message === 'string' ? entry.message : '',
      ...(typeof entry.line === 'number' ? { line: entry.line } : {}),
    })
  }
  return failures
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
