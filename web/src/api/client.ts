// The client of the HTTP API of the control端.
import { ApiError } from './types'
import { clearSession, sessionState } from '../auth/session'

// The header that carries the CSRF token of a modifying request.
const csrfHeader = 'X-CSRF-Token'

// The codes of a refusal that end the session of the administrator.
//
// A refusal of the credentials of the administrator is not one of them: the
// session of the control端 is still good, and the page shows the reason of
// the refusal next to the input that carries it.
const sessionEndedCodes = new Set(['unauthenticated', 'not_initialized'])

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
// An answer with the status 401 ends the session of the page only when the
// control端 says that the session is over, so a session that expired does
// not stay behind in the frontend and a refusal of the credentials of the
// administrator does not throw the administrator out of the page.
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
    // The body of the answer is read before the session is touched, because
    // the code of the error tells whether the session is over at all.
    const error = payload as ErrorPayload | undefined
    const code = error?.code ?? 'unknown_error'
    if (response.status === 401 && sessionEndedCodes.has(code)) {
      clearSession()
    }
    throw new ApiError(
      response.status,
      code,
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
