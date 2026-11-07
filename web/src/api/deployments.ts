// The client of the API of the applications and of the deployments.
//
// Every call of this module goes to the control端 of the same origin, so
// the session of the administrator travels in the cookie of the browser and
// no token ever appears in the address of a request.
//
// The names of the fields are the names that the API uses, so a document of
// the API can be read without a translation.
import { api } from './client'
import type {
  AppConfig,
  ApplicationListPayload,
  ApplicationPayload,
  AppMode,
  Deployment,
  DeploymentOutput,
  PreviewResult,
  Task,
  VersionListPayload,
  VersionPayload,
} from './types'

// CreateApplicationInput describes the application the administrator wants
// to create.
//
// The adopt flag records that the administrator confirmed the takeover of
// the resources of the namespace that the platform does not manage itself.
export interface CreateApplicationInput {
  name: string
  // description is the sentence the administrator wrote for the site.
  description: string
  mode: AppMode
  target_id: string
  namespace: string
  adopt: boolean
}

// RemovalConfirmation confirms the removal of the services of a preview.
//
// The confirmation is bound to the configuration of the preview, to the
// exact set of the services and to the state of the target, so a
// confirmation of another preview never fits.
export interface RemovalConfirmation {
  digest: string
  services: string[]
  inventory_revision: string
}

// SubmitDeploymentInput is the request of a deployment.
//
// The preview of the configuration the administrator saw is part of the
// request: the platform refuses a deployment that names no preview.
export interface SubmitDeploymentInput {
  idempotency_key: string
  base_version_id: string
  preview_digest: string
  inventory_revision: string
  config: AppConfig
  confirmed_removal: RemovalConfirmation
  accept_warnings: boolean
}

// ResolveDeploymentInput ends a deployment whose outcome is unknown.
//
// The administrator confirms that the executor of the task stopped and
// names the reason of the release.
export interface ResolveDeploymentInput {
  expected_seq: number
  executor_stopped: boolean
  reason: string
}

// ControlEvent is a change of the control端 that the browser learns about.
//
// The event names the kind of the change and the object that changed. It
// holds no configuration, so the browser reads the object again.
export interface ControlEvent {
  type: string
  object_id: string
  seq: number
}

// listApplications reads the applications of the control端.
export async function listApplications(): Promise<ApplicationListPayload['applications']> {
  const payload = await api<ApplicationListPayload>('/api/v1/applications')
  return payload.applications
}

// createApplication creates an application.
export async function createApplication(input: CreateApplicationInput): Promise<ApplicationPayload> {
  return api<ApplicationPayload>('/api/v1/applications', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// getApplication reads an application with the configuration of its latest
// version.
export async function getApplication(appID: string): Promise<ApplicationPayload> {
  return api<ApplicationPayload>(`/api/v1/applications/${encodeURIComponent(appID)}`)
}

// previewDeployment asks for the impact of a configuration on the target.
//
// The preview only reads: it changes neither the database nor the target.
export async function previewDeployment(
  appID: string,
  baseVersionID: string,
  config: AppConfig,
): Promise<PreviewResult> {
  return api<PreviewResult>(`/api/v1/applications/${encodeURIComponent(appID)}/preview`, {
    method: 'POST',
    body: JSON.stringify({ base_version_id: baseVersionID, config }),
  })
}

// submitDeployment writes the version and the task of a deployment.
//
// The idempotency key of the request identifies it: a repeated request with
// the same key and the same content answers with the task of the first
// request.
export async function submitDeployment(appID: string, input: SubmitDeploymentInput): Promise<Task> {
  return api<Task>(`/api/v1/applications/${encodeURIComponent(appID)}/deployments`, {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// listVersions reads the versions of an application.
//
// The list holds no configuration: the configuration of a version is read
// when the administrator opens it.
export async function listVersions(appID: string): Promise<VersionListPayload> {
  return api<VersionListPayload>(`/api/v1/applications/${encodeURIComponent(appID)}/versions`)
}

// getVersion reads one version of an application with its configuration.
export async function getVersion(appID: string, versionID: string): Promise<VersionPayload> {
  return api<VersionPayload>(
    `/api/v1/applications/${encodeURIComponent(appID)}/versions/${encodeURIComponent(versionID)}`,
  )
}

// getDeployment reads the state of a deployment.
export async function getDeployment(taskID: string): Promise<Deployment> {
  return api<Deployment>(`/api/v1/deployments/${encodeURIComponent(taskID)}`)
}

// getDeploymentOutput reads the output of a deployment.
export async function getDeploymentOutput(taskID: string): Promise<DeploymentOutput> {
  return api<DeploymentOutput>(`/api/v1/deployments/${encodeURIComponent(taskID)}/output`)
}

// reconcileDeployment asks the node about the outcome of a deployment.
//
// The request only reads: it changes neither the target nor the ledger of
// the node.
export async function reconcileDeployment(taskID: string): Promise<Deployment> {
  return api<Deployment>(`/api/v1/deployments/${encodeURIComponent(taskID)}/reconcile`, {
    method: 'POST',
  })
}

// resolveDeployment ends a deployment whose outcome is unknown.
export async function resolveDeployment(
  taskID: string,
  input: ResolveDeploymentInput,
): Promise<Deployment> {
  return api<Deployment>(`/api/v1/deployments/${encodeURIComponent(taskID)}/resolve`, {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// EventSubscription is the subscription of the stream of the events.
export interface EventSubscription {
  // close ends the subscription.
  close: () => void
}

// subscribeEvents opens the stream of the changes of the control端.
//
// The stream belongs to the administrator and uses the session of the
// administrator: the browser carries the session in its cookie and never in
// the address of the request.
//
// The browser reconnects the stream on its own. The page reads the state of
// the object again when the stream opens, so a reconnection never produces
// a second deployment.
export function subscribeEvents(handlers: {
  onEvent?: (event: ControlEvent) => void
  onOpen?: () => void
}): EventSubscription {
  const source = new EventSource('/api/v1/events', { withCredentials: true })

  const onFrame = (event: MessageEvent) => {
    let payload: ControlEvent
    try {
      payload = JSON.parse(event.data) as ControlEvent
    } catch {
      // The control端 sends JSON. A frame that holds something else is not
      // a change of a deployment, so the page ignores it.
      return
    }
    handlers.onEvent?.(payload)
  }

  source.addEventListener('deployment', onFrame)
  source.addEventListener('output', onFrame)
  source.addEventListener('open', () => handlers.onOpen?.())

  return {
    close: () => source.close(),
  }
}

// newIdempotencyKey returns the key of a submission.
//
// The key is random, so two deployments of the same configuration are two
// deployments. The key of a request that is repeated stays the same, so the
// platform answers with the task of the first attempt.
export function newIdempotencyKey(): string {
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

// outputText reads the output of a deployment as text.
//
// The bytes of the command line of Docker travel in the answer of the API
// as Base64, because JSON has no other way to hold them.
export function outputText(output: DeploymentOutput | null): string {
  if (!output) {
    return ''
  }
  const decoder = new TextDecoder('utf-8')
  return output.fragments
    .map((fragment) => {
      if (!fragment.data) {
        return ''
      }
      const binary = atob(fragment.data)
      const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
      return decoder.decode(bytes)
    })
    .join('')
}
