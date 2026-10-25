import { beforeEach, expect, test, vi } from 'vitest'
import { api } from '../api/client'
import { ApiError } from '../api/types'
import {
  beginSession,
  clearSession,
  onSessionEnded,
  restoreSession,
  sessionState,
} from './session'

// Node 25 brings a localStorage of its own that shadows the storage of the
// test environment and that reports a missing file. The tests install a
// storage of their own, so the storage of the browser can be watched.
function installStorage(): Map<string, string> {
  const entries = new Map<string, string>()
  const storage = {
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => {
      entries.set(key, String(value))
    },
    removeItem: (key: string) => {
      entries.delete(key)
    },
    clear: () => entries.clear(),
    key: (index: number) => [...entries.keys()][index] ?? null,
    get length() {
      return entries.size
    },
  }
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true })
  Object.defineProperty(globalThis, 'sessionStorage', { value: storage, configurable: true })
  return entries
}

const storage = installStorage()

// The payload that the session endpoint answers with.
const payload = { csrf_token: 'csrf-token', expires_at: '2026-10-25T16:00:00Z' }

// A response of the API with the given status and body.
function answer(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  clearSession(false)
  sessionState.status = 'unknown'
  storage.clear()
})

test('keeps the session of the administrator in the memory of the page only', () => {
  beginSession(payload)
  expect(sessionState.status).toBe('authenticated')
  expect(sessionState.csrfToken).toBe('csrf-token')
  expect(sessionState.expiresAt).toBe('2026-10-25T16:00:00Z')
  expect(localStorage.length).toBe(0)
  expect(sessionStorage.length).toBe(0)
})

test('forgets the session of the administrator', () => {
  beginSession(payload)
  clearSession(false)
  expect(sessionState.status).toBe('anonymous')
  expect(sessionState.csrfToken).toBeNull()
  expect(sessionState.expiresAt).toBeNull()
})

test('restores the session of the administrator', async () => {
  const status = await restoreSession(async () => payload)
  expect(status).toBe('authenticated')
  expect(sessionState.csrfToken).toBe('csrf-token')
})

test('reports a deployment without an administrator', async () => {
  const status = await restoreSession(async () => {
    throw new ApiError(401, 'not_initialized', '管理员尚未初始化')
  })
  expect(status).toBe('uninitialized')
  expect(sessionState.status).toBe('uninitialized')
  expect(sessionState.csrfToken).toBeNull()
})

test('reports an administrator without a session', async () => {
  const status = await restoreSession(async () => {
    throw new ApiError(401, 'unauthenticated', '请先登录')
  })
  expect(status).toBe('anonymous')
  expect(sessionState.status).toBe('anonymous')
})

test('tells the application about a session that ended on its own', () => {
  const ended = vi.fn()
  const stop = onSessionEnded(ended)
  beginSession(payload)
  clearSession()
  expect(ended).toHaveBeenCalledTimes(1)
  stop()
  beginSession(payload)
  clearSession()
  expect(ended).toHaveBeenCalledTimes(1)
})

test('does not report a session that never existed', () => {
  const ended = vi.fn()
  onSessionEnded(ended)
  clearSession()
  expect(ended).not.toHaveBeenCalled()
})

test('sends the CSRF token with a modifying request', async () => {
  const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
    answer(201, { token: 'enrollment', expires_at: 'later' }),
  )
  vi.stubGlobal('fetch', fetchMock)
  beginSession(payload)

  await api('/api/v1/agent-tokens', { method: 'POST' })

  const init = fetchMock.mock.calls[0][1]
  expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('csrf-token')
  expect(init?.credentials).toBe('same-origin')
})

test('sends no CSRF token with a request that reads', async () => {
  const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
    answer(200, { nodes: [] }),
  )
  vi.stubGlobal('fetch', fetchMock)
  beginSession(payload)

  await api('/api/v1/nodes')

  const init = fetchMock.mock.calls[0][1]
  expect(new Headers(init?.headers).has('X-CSRF-Token')).toBe(false)
})

test('clears the session when the API refuses the request', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => answer(401, { code: 'unauthenticated', message: '请先登录' })),
  )
  beginSession(payload)

  await expect(api('/api/v1/nodes')).rejects.toBeInstanceOf(ApiError)
  expect(sessionState.status).toBe('anonymous')
  expect(sessionState.csrfToken).toBeNull()
})

test('reports the reason of a failed request', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      answer(400, {
        code: 'validation_failed',
        message: '提交的密码不符合要求',
        fields: { new_password: '密码长度需为 12 到 128 个字符' },
      }),
    ),
  )

  const failure = await api('/api/v1/auth/password', { method: 'POST' }).catch(
    (error: unknown) => error,
  )
  expect(failure).toBeInstanceOf(ApiError)
  const error = failure as ApiError
  expect(error.code).toBe('validation_failed')
  expect(error.message).toBe('提交的密码不符合要求')
  expect(error.fields).toEqual({ new_password: '密码长度需为 12 到 128 个字符' })
})

test('reports a control端 that cannot be reached without a technical detail', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => {
      throw new TypeError('Failed to fetch')
    }),
  )

  const failure = await api('/api/v1/nodes').catch((error: unknown) => error)
  expect(failure).toBeInstanceOf(ApiError)
  expect((failure as ApiError).message).toBe('无法连接服务，请稍后重试')
})
