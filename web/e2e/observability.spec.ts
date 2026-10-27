import { expect, test, type Page, type Route } from '@playwright/test'

// The credentials of the mocked deployment.
const PASSWORD = 'a-long-admin-password'
const CSRF_TOKEN = 'csrf-token'
const EXPIRES_AT = '2026-10-25T16:00:00Z'

// The state of the mocked deployment.
interface MockState {
  rules: Array<{
    id: string
    kind: string
    scope_id: string
    metric: string
    threshold: number
    for_seconds: number
    enabled: boolean
  }>
  events: Array<{
    id: string
    rule_id: string
    object_id: string
    state: string
    summary: string
    started_at: string
    resolved_at: string | null
    acknowledged_at: string | null
  }>
  acknowledged: string[]
  created: Array<Record<string, unknown>>
  authenticated: boolean
}

// emptyState is a deployment with one rule and one open event.
function runningState(): MockState {
  return {
    rules: [
      {
        id: 'rule-1',
        kind: 'resource',
        scope_id: 'c1',
        metric: 'cpu_percent',
        threshold: 80,
        for_seconds: 30,
        enabled: true,
      },
    ],
    events: [
      {
        id: 'event-1',
        rule_id: 'rule-1',
        object_id: 'c1',
        state: 'active',
        summary: '对象 c1 的 cpu_percent 持续超过阈值 80',
        started_at: '2026-10-25T08:00:00Z',
        resolved_at: null,
        acknowledged_at: null,
      },
    ],
    acknowledged: [],
    created: [],
    authenticated: false,
  }
}

// installApi answers the API of the mocked deployment.
async function installApi(page: Page, state: MockState) {
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

    if (method === 'GET' && path === '/api/v1/auth/session') {
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'POST' && path === '/api/v1/auth/login') {
      const body = JSON.parse(request.postData() ?? '{}') as { password?: string }
      if (body.password !== PASSWORD) {
        return answer(401, { code: 'invalid_credentials', message: '登录密码不正确' })
      }
      state.authenticated = true
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'GET' && path === '/api/v1/nodes') {
      return answer(200, {
        nodes: [
          { id: 'node-1', hostname: 'worker-1', connected: true, container_count: 1, last_seen: EXPIRES_AT },
        ],
      })
    }

    if (method === 'GET' && path === '/api/v1/alert-rules') {
      return answer(200, { rules: state.rules })
    }

    if (method === 'POST' && path === '/api/v1/alert-rules') {
      const body = JSON.parse(request.postData() ?? '{}')
      state.created.push(body)
      const rule = {
        id: `rule-${state.rules.length + 1}`,
        kind: body.kind,
        scope_id: body.scope_id,
        metric: body.metric ?? '',
        threshold: body.threshold ?? 0,
        for_seconds: body.for_seconds ?? 0,
        enabled: body.enabled ?? true,
      }
      state.rules.push(rule)
      return answer(201, rule)
    }

    if (method === 'GET' && path === '/api/v1/alerts') {
      const events = state.events.map((event) =>
        state.acknowledged.includes(event.id)
          ? { ...event, acknowledged_at: '2026-10-25T09:00:00Z' }
          : event,
      )
      return answer(200, { alerts: events })
    }

    if (method === 'POST' && path.startsWith('/api/v1/alerts/') && path.endsWith('/acknowledge')) {
      const id = path.split('/')[4]
      state.acknowledged.push(id)
      return answer(200, { status: 'acknowledged' })
    }

    if (method === 'GET' && path === '/api/v1/webhook') {
      return answer(200, {
        webhook: {
          id: 'default',
          configured: false,
          url_masked: '',
          allowed_private_cidrs: [],
          enabled: false,
          updated_at: null,
        },
        notifications: { pending: 0, delivered: 0, failed: 0, skipped: 0, last_delivered_at: null },
      })
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

test('lists the alerts and acknowledges one', async ({ page }) => {
  const state = runningState()
  await installApi(page, state)
  await logIn(page)

  await page.getByRole('link', { name: '告警', exact: true }).click()
  await expect(page).toHaveURL(/\/alerts$/)

  // The open event and its summary are visible.
  await expect(page.getByText('对象 c1 的 cpu_percent 持续超过阈值 80')).toBeVisible()
  await expect(page.getByText('容器负载')).toBeVisible()

  await page.getByRole('button', { name: '确认', exact: true }).click()

  // The event leaves the anomalies and reaches the acknowledged list.
  await page.getByRole('button', { name: /已确认/ }).click()
  await expect(page.getByText('对象 c1 的 cpu_percent 持续超过阈值 80')).toBeVisible()
  expect(state.acknowledged).toEqual(['event-1'])
})

test('creates a rule with an explicit threshold', async ({ page }) => {
  const state = runningState()
  await installApi(page, state)
  await logIn(page)
  await page.goto('/alerts')

  await page.getByRole('button', { name: '新建规则' }).click()
  await page.getByLabel('监视对象', { exact: true }).fill('c2')
  await page.getByLabel('阈值', { exact: true }).fill('90')
  await page.getByLabel('持续时间（秒）', { exact: true }).fill('60')
  await page.getByRole('button', { name: '保存', exact: true }).click()

  await expect(page.getByText('c2')).toBeVisible()
  expect(state.created).toHaveLength(1)
  expect(state.created[0]).toMatchObject({
    kind: 'resource',
    scope_id: 'c2',
    metric: 'cpu_percent',
    threshold: 90,
    for_seconds: 60,
  })
})

test('never says that a host is down', async ({ page }) => {
  await installApi(page, runningState())
  await logIn(page)
  await page.goto('/alerts')

  // A node or a container is never reported as a host that went down: the
  // platform only knows what the agent reports.
  await expect(page.getByText('服务器宕机')).toHaveCount(0)
})
