import { expect, test, type Page, type Route } from '@playwright/test'

// The credentials of the mocked deployment.
const PASSWORD = 'a-long-admin-password'
const BOOTSTRAP_TOKEN = 'bootstrap-token'
const CSRF_TOKEN = 'csrf-token'
const EXPIRES_AT = '2026-10-25T16:00:00Z'

// A node of the mocked deployment.
interface NodeSummary {
  id: string
  hostname: string
  connected: boolean
  container_count: number
  last_seen: string | null
}

// The state of the mocked deployment.
interface Deployment {
  initialized: boolean
  authenticated: boolean
  nodes: NodeSummary[]
  revoked: string[]
  // The next revocation of a node fails.
  revokeFails: boolean
}

// A deployment without an administrator.
function emptyDeployment(): Deployment {
  return { initialized: false, authenticated: false, nodes: [], revoked: [], revokeFails: false }
}

// A deployment with an administrator and two nodes.
function runningDeployment(): Deployment {
  return {
    initialized: true,
    authenticated: false,
    nodes: [
      {
        id: 'node-1',
        hostname: 'worker-1',
        connected: true,
        container_count: 3,
        last_seen: EXPIRES_AT,
      },
      {
        id: 'node-2',
        hostname: 'worker-2-with-a-very-long-name',
        connected: false,
        container_count: 0,
        last_seen: null,
      },
    ],
    revoked: [],
    revokeFails: false,
  }
}

// installApi answers the requests of the frontend with the API of the
// control端.
//
// The answers hold the same codes, the same messages and the same fields as
// the real API, so the frontend is tested against the contract of the
// control端 and not against an invention of the test.
async function installApi(page: Page, state: Deployment) {
  await page.route('**/api/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const method = request.method()

    const answer = (status: number, body?: unknown) =>
      route.fulfill({
        status,
        contentType: 'application/json',
        body: body === undefined ? '' : JSON.stringify(body),
      })
    const body = () => JSON.parse(request.postData() ?? '{}') as Record<string, string>

    if (method === 'GET' && path === '/api/v1/auth/session') {
      if (!state.initialized) {
        return answer(401, { code: 'not_initialized', message: '管理员尚未初始化' })
      }
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'POST' && path === '/api/v1/auth/initialize') {
      if (state.initialized) {
        return answer(409, { code: 'already_initialized', message: '管理员已经初始化' })
      }
      if (body().bootstrap_token !== BOOTSTRAP_TOKEN) {
        return answer(401, { code: 'invalid_bootstrap', message: '初始化凭据无效或已过期' })
      }
      state.initialized = true
      return answer(200, { initialized: true })
    }

    if (method === 'POST' && path === '/api/v1/auth/login') {
      if (!state.initialized || body().password !== PASSWORD) {
        return answer(401, { code: 'invalid_credentials', message: '登录凭据无效' })
      }
      state.authenticated = true
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'POST' && path === '/api/v1/auth/logout') {
      state.authenticated = false
      return answer(204)
    }

    if (method === 'POST' && path === '/api/v1/auth/password') {
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      if (body().current_password !== PASSWORD) {
        return answer(401, { code: 'invalid_credentials', message: '当前密码不正确' })
      }
      state.authenticated = false
      return answer(204)
    }

    if (method === 'GET' && path === '/api/v1/nodes') {
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      return answer(200, { nodes: state.nodes })
    }

    if (method === 'POST' && path === '/api/v1/agent-tokens') {
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      return answer(201, { token: 'enrollment-token', expires_at: EXPIRES_AT })
    }

    if (method === 'POST' && path.endsWith('/revoke')) {
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      const id = path.split('/').at(-2) ?? ''
      if (state.revokeFails) {
        state.revokeFails = false
        return answer(500, { code: 'internal_error', message: '撤销节点失败' })
      }
      state.revoked.push(id)
      state.nodes = state.nodes.filter((node) => node.id !== id)
      return answer(204)
    }

    return answer(404, { code: 'not_found', message: '未找到请求的资源' })
  })
}

// logIn signs the administrator in through the login page.
async function logIn(page: Page) {
  await page.goto('/login')
  await page.getByLabel('管理员密码', { exact: true }).fill(PASSWORD)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL(/\/$/)
}

test('creates the administrator and leads to the login', async ({ page }) => {
  const state = emptyDeployment()
  await installApi(page, state)

  await page.goto('/')
  await expect(page).toHaveURL(/\/initialize$/)

  await page.getByLabel('初始化凭据', { exact: true }).fill(BOOTSTRAP_TOKEN)
  await page.getByLabel('管理员密码', { exact: true }).fill(PASSWORD)
  await page.getByLabel('确认密码', { exact: true }).fill(PASSWORD)
  await page.getByRole('button', { name: '创建管理员' }).click()

  await expect(page).toHaveURL(/\/login$/)
  expect(state.initialized).toBe(true)
})

test('keeps the input and shows the reason of a refused initialization', async ({ page }) => {
  await installApi(page, emptyDeployment())
  await page.goto('/initialize')

  await page.getByLabel('初始化凭据', { exact: true }).fill('wrong-token')
  await page.getByLabel('管理员密码', { exact: true }).fill(PASSWORD)
  await page.getByLabel('确认密码', { exact: true }).fill(PASSWORD)
  await page.getByRole('button', { name: '创建管理员' }).click()

  await expect(page.getByRole('alert')).toContainText('初始化凭据无效或已过期')
  await expect(page.getByLabel('初始化凭据', { exact: true })).toHaveValue('wrong-token')
  await expect(page).toHaveURL(/\/initialize$/)
})

test('logs the administrator in and shows the nodes of the deployment', async ({ page }) => {
  await installApi(page, runningDeployment())

  await logIn(page)

  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toBeVisible()
  await expect(page.getByText('已连接', { exact: true })).toBeVisible()
  await expect(page.getByText('连接中断', { exact: true })).toBeVisible()
  await expect(page.getByText('服务器宕机')).toHaveCount(0)
  await expect(page.getByText('共 2 个节点。')).toBeVisible()
})

test('refuses a wrong password and keeps the input', async ({ page }) => {
  await installApi(page, runningDeployment())
  await page.goto('/login')

  await page.getByLabel('管理员密码', { exact: true }).fill('wrong-password')
  await page.getByRole('button', { name: '登录', exact: true }).click()

  await expect(page.getByRole('alert')).toContainText('登录凭据无效')
  await expect(page.getByLabel('管理员密码', { exact: true })).toHaveValue('wrong-password')
  await expect(page).toHaveURL(/\/login$/)
})

test('sends the administrator back to the login when the session ends', async ({ page }) => {
  const state = runningDeployment()
  await installApi(page, state)
  await logIn(page)

  // The session of the administrator expires while the page is open.
  state.authenticated = false
  await page.getByRole('button', { name: '刷新' }).click()

  await expect(page).toHaveURL(/\/login$/)
})

test('shows the enrollment token once and keeps it out of the storage', async ({ page }) => {
  await installApi(page, runningDeployment())
  await logIn(page)
  await page.goto('/settings')

  await page.getByRole('button', { name: '生成接入令牌' }).click()
  await expect(page.getByText('enrollment-token')).toBeVisible()
  expect(await page.evaluate(() => localStorage.length)).toBe(0)
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0)

  await page.reload()
  await expect(page.getByRole('button', { name: '生成接入令牌' })).toBeVisible()
  await expect(page.getByText('enrollment-token')).toHaveCount(0)
})

test('revokes a node only after a confirmation', async ({ page }) => {
  const state = runningDeployment()
  await installApi(page, state)
  await logIn(page)
  await page.goto('/settings')

  await page.getByRole('button', { name: '撤销' }).first().click()
  await expect(page.getByText('确认撤销？')).toBeVisible()
  await page.getByRole('button', { name: '取消' }).click()
  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toBeVisible()
  expect(state.revoked).toHaveLength(0)

  await page.getByRole('button', { name: '撤销' }).first().click()
  await page.getByRole('button', { name: '确认' }).click()

  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toHaveCount(0)
  await expect(page.getByText('已撤销该节点的接入凭据，它不会再连接。')).toBeVisible()
  expect(state.revoked).toEqual(['node-1'])
})

test('reports a failed revocation and lets the administrator try again', async ({ page }) => {
  const state = runningDeployment()
  state.revokeFails = true
  await installApi(page, state)
  await logIn(page)
  await page.goto('/settings')

  await page.getByRole('button', { name: '撤销' }).first().click()
  await page.getByRole('button', { name: '确认' }).click()

  await expect(page.getByRole('alert')).toContainText('撤销节点失败')
  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toBeVisible()

  await page.getByRole('button', { name: '确认' }).click()
  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toHaveCount(0)
})

test('returns to the login after a password change', async ({ page }) => {
  await installApi(page, runningDeployment())
  await logIn(page)
  await page.goto('/settings')

  await page.getByLabel('当前密码', { exact: true }).fill(PASSWORD)
  await page.getByLabel('新密码', { exact: true }).fill('another-long-admin-password')
  await page.getByLabel('确认新密码', { exact: true }).fill('another-long-admin-password')
  await page.getByRole('button', { name: '修改密码' }).click()

  await expect(page).toHaveURL(/\/login\?notice=password-changed$/)
  await expect(page.getByText('密码已修改，请使用新密码登录。')).toBeVisible()
})

test('signs the administrator out', async ({ page }) => {
  const state = runningDeployment()
  await installApi(page, state)
  await logIn(page)

  await page.getByRole('button', { name: '退出登录' }).click()

  await expect(page).toHaveURL(/\/login$/)
  expect(state.authenticated).toBe(false)
})

test('logs the administrator in with the keyboard alone', async ({ page }) => {
  await installApi(page, runningDeployment())
  await page.goto('/login')

  await page.getByLabel('管理员密码', { exact: true }).focus()
  await page.keyboard.type(PASSWORD)
  await page.keyboard.press('Enter')

  await expect(page).toHaveURL(/\/$/)
  await expect(page.getByRole('rowheader', { name: 'worker-1' })).toBeVisible()
})
