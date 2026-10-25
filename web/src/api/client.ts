// The client of the HTTP API of the control端.
import { ApiError } from './types'
import { clearSession, sessionState } from '../auth/session'

// The header that carries the CSRF token of a modifying request.
const csrfHeader = 'X-CSRF-Token'

// The shape of an error document of the API.
interface ErrorPayload {
  code?: string
  message?: string
  fields?: Record<string, string>
}

// api calls the HTTP API of the control端.
//
// The call is same origin, so the session cookie of the browser travels with
// it and no token ever appears in the address of a request. A modifying
// request carries the CSRF token of the session, because the control端
// refuses a request without it.
//
// An answer with the status 401 clears the session of the page, so a session
// that expired does not stay behind in the frontend.
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method ?? 'GET').toUpperCase()
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  if (method !== 'GET' && method !== 'HEAD') {
    const token = sessionState.csrfToken
    if (token) {
      headers.set(csrfHeader, token)
    }
  }

  let response: Response
  try {
    response = await fetch(path, { ...init, method, headers, credentials: 'same-origin' })
  } catch {
    throw new ApiError(0, 'network_error', '无法连接服务，请稍后重试')
  }

  if (response.status === 204) {
    return undefined as T
  }

  const payload = await readJSON(response)
  if (!response.ok) {
    if (response.status === 401) {
      clearSession()
    }
    const error = payload as ErrorPayload | undefined
    throw new ApiError(
      response.status,
      error?.code ?? 'unknown_error',
      error?.message ?? '请求失败，请稍后重试',
      error?.fields ?? {},
    )
  }
  return payload as T
}

// readJSON reads the body of an answer.
//
// A body that is not JSON is no error: the control端 answers with JSON, but
// something in front of it may answer with a document of another kind.
async function readJSON(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch {
    return undefined
  }
}
