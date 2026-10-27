// The client of the subscriptions of the logs.
//
// The pieces of the logs of a subscription travel in the stream of the
// events, so the browser has to hold an open stream before it asks for a
// subscription. A request without an open stream is refused by the
// control端, so the page waits for the stream and never asks for a
// subscription that nobody would receive.
//
// Every call of this module goes to the control端 of the same origin, so
// the session of the administrator travels in the cookie of the browser and
// no token ever appears in the address of a request.
import { api } from './client'
import { ApiError, describeError } from './types'
import type { LogEvent, LogSubscriptionPayload } from './types'

// The reasons a stream of the logs ended without its text.
const logErrorMessages: Record<string, string> = {
  // event_stream_required is the refusal of a subscription without an open
  // stream of the events.
  event_stream_required: '实时连接还没有建立，请稍候重试。',
  // node_disconnected reports a subscription that ended because the agent
  // of its node did not come back.
  node_disconnected: '连接中断',
  // node_revoked reports a subscription that ended because the
  // administrator revoked the node.
  node_revoked: '该节点的接入凭据已被撤销，日志已经停止。',
  // logs_unavailable reports a subscription the agent could not serve.
  logs_unavailable: '日志暂时无法读取，请稍后重试。',
}

// logErrorMessage names a reason of the logs for the administrator.
//
// The reasons of the control端 are codes, so the page names them itself and
// never shows a technical detail.
export function logErrorMessage(code: string): string {
  return logErrorMessages[code] ?? '日志流已经结束。'
}

// describeLogError names a failure of the logs for the administrator.
export function describeLogError(failure: unknown): string {
  if (failure instanceof ApiError) {
    const known = logErrorMessages[failure.code]
    if (known) {
      return known
    }
  }
  return describeError(failure)
}

// LogSubscriptionRequest names the container whose logs the administrator
// wants to read.
export interface LogSubscriptionRequest {
  node_id: string
  container_id: string
  // tail is the number of the lines the subscription starts with.
  tail?: number
  // follow reports whether the subscription follows the stream.
  follow?: boolean
}

// createLogSubscription opens a subscription of the logs of a container.
//
// The stream of the events has to be open already: the control端 refuses a
// subscription without one, because the pieces of the logs travel in it.
export async function createLogSubscription(
  request: LogSubscriptionRequest,
): Promise<LogSubscriptionPayload> {
  return api<LogSubscriptionPayload>('/api/v1/log-subscriptions', {
    method: 'POST',
    body: JSON.stringify(request),
  })
}

// deleteLogSubscription ends a subscription of the logs.
//
// The subscription belongs to the session that created it, so a page can
// only end its own subscription.
export async function deleteLogSubscription(subscriptionID: string): Promise<void> {
  await api<void>(`/api/v1/log-subscriptions/${encodeURIComponent(subscriptionID)}`, {
    method: 'DELETE',
  })
}

// LogStream is the stream of the pieces of the logs.
export interface LogStream {
  // ready tells the caller that the stream is open. A subscription may only
  // be asked for after the stream is open.
  ready: Promise<void>
  // close ends the stream.
  close: () => void
}

// openLogStream opens the stream of the pieces of the logs.
//
// The stream belongs to the administrator and uses the session of the
// administrator: the browser carries the session in its cookie and never in
// the address of the request. The browser reconnects the stream on its own,
// so a page that waits for the stream never asks for a subscription that
// nobody would receive.
export function openLogStream(handlers: {
  onLog?: (event: LogEvent) => void
  onOpen?: () => void
}): LogStream {
  const source = new EventSource('/api/v1/events', { withCredentials: true })

  let open: () => void = () => {}
  const ready = new Promise<void>((resolve) => {
    open = resolve
  })

  const onFrame = (event: MessageEvent) => {
    let payload: LogEvent
    try {
      payload = JSON.parse(event.data) as LogEvent
    } catch {
      // The control端 sends JSON. A frame that holds something else is not
      // a piece of the logs, so the page ignores it.
      return
    }
    handlers.onLog?.(payload)
  }

  source.addEventListener('log', onFrame)
  source.addEventListener('open', () => {
    open()
    handlers.onOpen?.()
  })

  return {
    ready,
    close: () => source.close(),
  }
}
