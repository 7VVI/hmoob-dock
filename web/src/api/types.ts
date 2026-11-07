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
  // The role of the node inside its Swarm cluster, standalone when it
  // belongs to no cluster.
  role: string
  // Whether the agent of the node is connected.
  connected: boolean
  // Whether the credential of the node was withdrawn.
  revoked: boolean
  // The version of the Docker engine of the last inventory.
  docker_version: string
  // The type of the operating system of the engine.
  os_type: string
  // The architecture of the engine host.
  os_arch: string
  // The version of the agent of the last connection.
  agent_version: string
  // The number of containers of the last complete inventory.
  container_count: number
  // The time of the last heartbeat, or null while the node never sent one.
  last_seen: string | null
  // The time the node was enrolled.
  created_at: string
}

// NodeListPayload is the answer of the node list.
export interface NodeListPayload {
  nodes: NodeSummary[]
}

// PortMapping is one port of a container or of a service.
export interface PortMapping {
  private_port: number
  public_port?: number
  type?: string
  ip?: string
}

// ContainerMount is one mount of a container.
export interface ContainerMount {
  type?: string
  source?: string
  destination?: string
  mode?: string
  read_write?: boolean
}

// NodeService is one service of a node: a service of a Swarm cluster or a
// service derived from the containers of a Compose project.
export interface NodeService {
  id: string
  name: string
  stack: string
  image: string
  mode: string
  desired: number
  running: number
  update_state: string
  containers: number
  ports: PortMapping[]
  updated_at: string
  source: 'swarm' | 'compose'
}

// NodeNetwork is one network of the Docker engine of a node.
export interface NodeNetwork {
  id: string
  name: string
  // stack is the Compose project or the Swarm stack the network belongs
  // to, when its labels name one.
  stack: string
  driver: string
  scope: string
  internal: boolean
  attachable: boolean
  created: string
  containers: number
  // The address management of the network: the driver and one subnet and
  // one gateway per address family. A network that Docker manages by
  // itself carries none of them.
  ipam_driver: string
  ipam_subnet: string
  ipam_gateway: string
  ipam_ipv6_subnet: string
  ipam_ipv6_gateway: string
}

// NodeImage is one image in the local store of a node.
export interface NodeImage {
  // id is the ID of the image, for example sha256:....
  id: string
  // tags are the references the image carries. An image that no reference
  // names carries none.
  tags: string[]
  // size is the size of the image on the disk of the node, in bytes.
  size: number
  created: string
  // containers is the number of the containers of the node that run the
  // image.
  containers: number
  // used reports whether a container of the node runs the image.
  used: boolean
}

// NodeVolume is one named volume in the local store of a node.
export interface NodeVolume {
  name: string
  // stack is the Compose project or the Swarm stack the volume belongs
  // to, when its labels name one.
  stack: string
  driver: string
  scope: string
  // mountpoint is the path on the node the volume lives at.
  mountpoint: string
  created: string
  // containers is the number of the containers of the node that mount the
  // volume.
  containers: number
  // used reports whether a container of the node mounts the volume.
  used: boolean
}

// NodeEvent is one container event of a node, for the event log of a site.
export interface NodeEvent {
  container_id: string
  service: string
  // action is what happened to the container: start, die, stop, kill...
  action: string
  exit_code: number | null
  occurred_at: string
  namespace: string
}

// NodeSwarmNode is one node of the Swarm cluster a node belongs to.
export interface NodeSwarmNode {
  // id is the ID of the node inside of its Swarm cluster.
  id: string
  hostname: string
  // role is the role of the node inside of the cluster: manager or worker.
  role: string
  // availability is the state of the scheduler for the node: active,
  // pause or drain.
  availability: string
  // status is the state of the node as the manager sees it: ready or down.
  status: string
  // addr is the address the node reports to the cluster.
  addr: string
  // cpus is the number of the cores the node offers the cluster.
  cpus: number
  // memory_bytes is the memory the node offers the cluster, in bytes.
  memory_bytes: number
  // engine_version is the version of the Docker engine of the node.
  engine_version: string
}

// NodeDetailPayload is the answer of the detail of a node.
export interface NodeDetailPayload {
  node: NodeSummary
}

// The operations the platform performs on one container of a node.
export type ContainerAction = 'start' | 'stop' | 'restart' | 'remove'

// ContainerOpResult is the answer of a container operation. The container
// is the state the node read after the operation; it is absent for a
// removal.
export interface ContainerOpResult {
  request_id: string
  ok: boolean
  error_code?: string
  message?: string
  container?: {
    id: string
    name: string
    image: string
    state: string
    health: string
  } | null
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
  // description is the sentence the administrator wrote for the site.
  description: string
  mode: AppMode
  target_id: string
  namespace: string
  // latest_version_id is the version that was submitted last.
  latest_version_id: string
  // successful_version_id is the version that was deployed successfully
  // last.
  successful_version_id: string
  // created_at is the time the site was created.
  created_at: string
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

// MetricValues holds the values of one observation of a container.
//
// The names of the fields are the names that the API uses, so a document of
// the API can be read without a translation.
export interface MetricValues {
  cpu_percent: number
  memory_bytes: number
  net_rx_per_sec: number
  net_tx_per_sec: number
  io_read_per_sec: number
  io_write_per_sec: number
}

// MetricBucket is one minute of the history of a container.
//
// A minute that was not observed has no bucket at all: the reader fills the
// gap with an empty value and never with a zero.
export interface MetricBucket {
  // start is the start of the minute in UTC.
  start: string
  // average holds the mean of the valid samples of the minute.
  average: MetricValues
  // peak holds the largest value of every field of the minute.
  peak: MetricValues
  // sample_count is the number of the valid samples.
  sample_count: number
  // expected_count is the number of the samples a complete minute holds.
  expected_count: number
  // complete reports whether the minute holds every expected sample.
  complete: boolean
}

// MetricsPayload is the answer of the history of the metrics.
export interface MetricsPayload {
  node_id: string
  container_id: string
  from: string
  to: string
  // units names the unit of every value of a bucket.
  units: Record<string, string>
  buckets: MetricBucket[]
  limit: number
  offset: number
  has_more: boolean
}

// MetricPoint is one point of a curve.
//
// A minute that was not observed holds no value: the point keeps its place
// on the time line and shows a gap instead of a made up number.
export interface MetricPoint {
  // at is the start of the minute in UTC.
  at: string
  // average is the mean of the minute, or null when the minute was not
  // observed.
  average: number | null
  // peak is the largest value of the minute, or null when the minute was
  // not observed.
  peak: number | null
  // complete reports whether the minute holds every expected sample.
  complete: boolean
}

// AlertRuleKind is the kind of the observation a rule watches.
export type AlertRuleKind = 'resource' | 'container_exit' | 'agent_offline'

// AlertSeverity is the weight the administrator gives a rule.
export type AlertSeverity = 'info' | 'warning' | 'critical'

// AlertScopeKind is the breadth a rule watches.
export type AlertScopeKind = 'all' | 'container' | 'namespace'

// AlertOperator compares the observed value with the threshold of a rule.
export type AlertOperator = '>' | '>=' | '<' | '<='

// AlertRule is a rule of the alerts.
export interface AlertRule {
  id: string
  // name is the title the administrator reads. A rule without a name falls
  // back to the label of its kind in the console.
  name: string
  // description is the sentence the administrator wrote for the rule.
  description: string
  kind: AlertRuleKind
  // severity is the weight of the rule.
  severity: AlertSeverity
  // scope_kind names the breadth the rule watches.
  scope_kind: AlertScopeKind
  // scope_id is the object the rule watches: a container, a namespace or
  // a node. It is empty for a rule of all containers.
  scope_id: string
  // metric is the value a resource rule watches.
  metric: string
  // operator compares the observed value with the threshold.
  operator: AlertOperator
  // threshold is the limit of a resource rule.
  threshold: number
  // for_seconds is the time the condition has to hold, in seconds.
  for_seconds: number
  enabled: boolean
  // recover_notify reports whether the recovery of the rule speaks.
  recover_notify: boolean
}

// AlertEventState is the state of an event of the alerts.
export type AlertEventState = 'active' | 'resolved'

// AlertEvent is one anomaly of one object.
export interface AlertEvent {
  id: string
  // rule_id is the rule that opened the event.
  rule_id: string
  // object_id is the object the event is about.
  object_id: string
  state: AlertEventState
  summary: string
  started_at: string
  resolved_at: string | null
  acknowledged_at: string | null
}

// WebhookPayload is the state of the target of the notifications.
//
// The answer holds the masked form of the address only: the address of a
// target can carry a secret in its path, so it never leaves the control端
// in the clear.
export interface WebhookPayload {
  id: string
  // configured reports whether a target was configured already.
  configured: boolean
  // url_masked is the display form of the address of the target.
  url_masked: string
  // allowed_private_cidrs are the private networks the administrator
  // allowed the target to live in.
  allowed_private_cidrs: string[]
  enabled: boolean
  updated_at: string | null
}

// NotificationStatus is the state of the queue of the notifications.
export interface NotificationStatus {
  // pending is the number of the notifications that wait for their
  // delivery.
  pending: number
  // delivered is the number of the notifications that reached their
  // target.
  delivered: number
  // failed is the number of the notifications that gave up after their last
  // try.
  failed: number
  // skipped is the number of the notifications that were not queued,
  // because the queue was full.
  skipped: number
  last_delivered_at: string | null
}

// WebhookBundle is the target of the notifications with the state of their
// queue.
export interface WebhookBundle {
  webhook: WebhookPayload
  notifications: NotificationStatus
}

// LogSubscriptionPayload is a subscription of the logs of a container.
export interface LogSubscriptionPayload {
  id: string
  node_id: string
  container_id: string
  tail: number
  follow: boolean
  expires_at: string
}

// LogEvent is a piece of the logs of a container.
//
// The event of a piece of the logs carries the text and its position. An
// empty piece without a text reports a gap: the control端 lost a piece of
// the logs or rebuilt the tail of the log.
export interface LogEvent {
  type: string
  // object_id is the subscription the piece belongs to.
  object_id: string
  seq: number
  offset?: number
  text?: string
  // truncated reports that a piece before this one was lost.
  truncated?: boolean
  // error_code names the reason a stream of the logs ended.
  error_code?: string
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
