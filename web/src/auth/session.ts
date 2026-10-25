// The session of the administrator.
//
// The session itself lives in a cookie of the browser that the scripts of
// the page cannot read. The CSRF token is the only part of the session that
// the frontend has to know, and it stays in the memory of the page: it never
// reaches the storage of the browser.
import { reactive } from 'vue'
import { ApiError, type SessionPayload } from '../api/types'

// SessionStatus describes what the page knows about the session.
export type SessionStatus =
  // The state was not read yet.
  | 'unknown'
  // The deployment has no administrator yet.
  | 'uninitialized'
  // The administrator exists, the page has no session.
  | 'anonymous'
  // The page holds a session of the administrator.
  | 'authenticated'

// SessionState is the state of the session of the administrator.
export interface SessionState {
  // The status of the session.
  status: SessionStatus
  // The CSRF token of the session, or null while there is no session.
  csrfToken: string | null
  // The time the session expires, or null while there is no session.
  expiresAt: string | null
}

// The state of the session of the current page.
export const sessionState = reactive<SessionState>({
  status: 'unknown',
  csrfToken: null,
  expiresAt: null,
})

// A listener is called when a session of the administrator ends.
type SessionEndedListener = () => void

const listeners = new Set<SessionEndedListener>()

// onSessionEnded registers a listener for a session that ends on its own.
// It returns the function that removes the listener again.
export function onSessionEnded(listener: SessionEndedListener): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// beginSession remembers the session of a successful login.
export function beginSession(payload: SessionPayload): void {
  sessionState.status = 'authenticated'
  sessionState.csrfToken = payload.csrf_token
  sessionState.expiresAt = payload.expires_at ?? null
}

// clearSession forgets the session of the administrator.
//
// A session that ends while the page is open is reported to the listeners,
// so the application can send the administrator back to the login. A session
// that never existed is not reported.
export function clearSession(notify = true): void {
  const hadSession = sessionState.status === 'authenticated'
  sessionState.status = 'anonymous'
  sessionState.csrfToken = null
  sessionState.expiresAt = null
  if (!notify || !hadSession) {
    return
  }
  for (const listener of [...listeners]) {
    listener()
  }
}

// restoreSession reads the state of the session from the API.
//
// The loader is a parameter, so the module does not depend on the client and
// its behaviour can be tested without a network.
export async function restoreSession(
  load: () => Promise<SessionPayload>,
): Promise<SessionStatus> {
  try {
    const payload = await load()
    beginSession(payload)
    return 'authenticated'
  } catch (error) {
    clearSession(false)
    if (error instanceof ApiError && error.code === 'not_initialized') {
      sessionState.status = 'uninitialized'
      return 'uninitialized'
    }
    return 'anonymous'
  }
}
