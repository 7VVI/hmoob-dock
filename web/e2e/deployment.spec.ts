import { expect, test, type Page, type Route } from '@playwright/test'

// The credentials of the mocked deployment.
const PASSWORD = 'a-long-admin-password'
const CSRF_TOKEN = 'csrf-token'
const EXPIRES_AT = '2026-10-25T16:00:00Z'

// The YAML of an application that the administrator edits.
const CONFIG = 'services:\n  web:\n    image: nginx:1.27\n'

// The state of a deployment task.
type TaskState =
  | 'queued'
  | 'preflight'
  | 'running'
  | 'verifying'
  | 'succeeded'
  | 'failed'
  | 'uncertain'

// An application of the mocked deployment.
interface MockApp {
  id: string
  name: string
  mode: 'compose' | 'swarm'
  target_id: string
  namespace: string
  latest_version_id: string
  successful_version_id: string
  config: { yaml: string; variables: Record<string, string> }
}

// A version of the history of an application.
interface MockVersion {
  id: string
  app_id: string
  created_at: string
  config: { yaml: string; variables: Record<string, string> }
}

// A deployment of the mocked deployment.
interface MockDeployment {
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
  manual_resolution: { seq: number; reason: string; pending: boolean } | null
  created_at: string
  updated_at: string
  finished_at: string | null
}

// The state of the mocked deployment.
interface MockState {
  initialized: boolean
  authenticated: boolean
  applications: MockApp[]
  versions: MockVersion[]
  deployments: Record<string, MockDeployment>
  // The keys of the submissions the page sent.
  submissions: string[]
  // The number of times the page read a deployment.
  reads: number
  // The number of times the page opened the stream of the events.
  streams: number
  // The services the preview removes.
  removedServices: string[]
  // The outcome of the next deployment.
  outcome: {
    state: TaskState
    message: string
    partial: boolean
    rolledBack: boolean
  }
  // The page is out of date: the application has a newer version.
  stale: boolean
  // The next preview is refused.
  previewRefused: boolean
}

// A deployment without an administrator.
function emptyState(): MockState {
  return {
    initialized: false,
    authenticated: false,
    applications: [],
    versions: [],
    deployments: {},
    submissions: [],
    reads: 0,
    streams: 0,
    removedServices: [],
    outcome: { state: 'succeeded', message: '服务 web 已健康', partial: false, rolledBack: false },
    stale: false,
    previewRefused: false,
  }
}

// A deployment with one application and one finished deployment.
function runningState(): MockState {
  const state = emptyState()
  state.initialized = true
  state.applications = [{
    id: 'app-1',
    name: '网站',
    mode: 'compose',
    target_id: 'node-1',
    namespace: 'site',
    latest_version_id: 'version-1',
    successful_version_id: 'version-1',
    config: { yaml: CONFIG, variables: {} },
  }]
  state.versions = [{
    id: 'version-1',
    app_id: 'app-1',
    created_at: '2026-10-25T08:00:00Z',
    config: { yaml: CONFIG, variables: {} },
  }]
  state.deployments = {
    'task-1': {
      id: 'task-1',
      app_id: 'app-1',
      version_id: 'version-1',
      agent_id: 'node-1',
      state: 'succeeded',
      last_seq: 3,
      result_code: 'compose_ready',
      result_message: '服务 web 已健康',
      rolled_back: false,
      partial_update: false,
      dispatch_started_at: '2026-10-25T08:00:01Z',
      manual_resolution: null,
      created_at: '2026-10-25T08:00:00Z',
      updated_at: '2026-10-25T08:00:05Z',
      finished_at: '2026-10-25T08:00:05Z',
    },
  }
  return state
}

// digestOf is the digest of a configuration.
//
// The control端 hashes the configuration, so two configurations that differ
// have two digests. The mock hashes the same way, so the page cannot send a
// digest of another configuration.
function digestOf(yaml: string, variables: Record<string, string>): string {
  const payload = `${yaml}\n${JSON.stringify(variables)}`
  let hash = 0
  for (let index = 0; index < payload.length; index += 1) {
    hash = (hash * 31 + payload.charCodeAt(index)) >>> 0
  }
  return `digest-${hash.toString(16)}`
}

// referencesOf returns the names of the variables a document refers to.
function referencesOf(yaml: string): string[] {
  const names = new Set<string>()
  const pattern = /\$\{?([_a-zA-Z][_a-zA-Z0-9]*)\}?/g
  let match = pattern.exec(yaml)
  while (match !== null) {
    names.add(match[1])
    match = pattern.exec(yaml)
  }
  return [...names]
}

// privilegedServicesOf returns the services of a document that run
// privileged.
//
// The control端 writes one warning per privileged service, so the mock reads
// the names of the document instead of naming a service of its own.
function privilegedServicesOf(yaml: string): string[] {
  const services: string[] = []
  let inServices = false
  let current = ''
  let privileged = false
  const flush = () => {
    if (current !== '' && privileged) {
      services.push(current)
    }
  }
  for (const raw of yaml.split('\n')) {
    const line = raw.replace(/\r$/, '')
    const text = line.trim()
    if (text === '' || text.startsWith('#')) {
      continue
    }
    const indent = line.length - line.trimStart().length
    if (indent === 0) {
      flush()
      current = ''
      privileged = false
      inServices = text === 'services:'
      continue
    }
    if (!inServices) {
      continue
    }
    if (indent === 2 && text.endsWith(':')) {
      flush()
      current = text.slice(0, -1).replace(/^["']|["']$/g, '')
      privileged = false
      continue
    }
    if (indent >= 4 && current !== '' && /^privileged:\s*true$/.test(text)) {
      privileged = true
    }
  }
  flush()
  return services
}

// installApi answers the requests of the frontend with the API of the
// control端.
//
// The answers hold the same codes, the same messages and the same fields as
// the real API, so the frontend is tested against the contract of the
// control端 and not against an invention of the test.
async function installApi(page: Page, state: MockState) {
  await page.route('**/api/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const method = request.method()
    const parts = path.split('/').filter((part) => part !== '')

    const answer = (status: number, body?: unknown) =>
      route.fulfill({
        status,
        contentType: 'application/json',
        body: body === undefined ? '' : JSON.stringify(body),
      })

    if (method === 'GET' && path === '/api/v1/auth/session') {
      if (!state.initialized) {
        return answer(401, { code: 'not_initialized', message: '管理员尚未初始化' })
      }
      if (!state.authenticated) {
        return answer(401, { code: 'unauthenticated', message: '请先登录' })
      }
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'POST' && path === '/api/v1/auth/login') {
      if (!state.initialized) {
        return answer(401, { code: 'not_initialized', message: '管理员尚未初始化' })
      }
      const body = JSON.parse(request.postData() ?? '{}') as { password?: string }
      if (body.password !== PASSWORD) {
        return answer(401, { code: 'invalid_credentials', message: '登录凭据无效' })
      }
      state.authenticated = true
      return answer(200, { csrf_token: CSRF_TOKEN, expires_at: EXPIRES_AT })
    }

    if (method === 'GET' && path === '/api/v1/nodes') {
      return answer(200, { nodes: [
        { id: 'node-1', hostname: 'worker-1', connected: true, container_count: 2, last_seen: EXPIRES_AT },
      ] })
    }

    if (method === 'GET' && path === '/api/v1/events') {
      // The stream of the events ends right away, so the browser has to
      // open it again. The page reads the state of the deployment again
      // instead of sending it a second time.
      state.streams += 1
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: 'retry: 300\n\n',
      })
    }

    if (method === 'GET' && path === '/api/v1/applications') {
      return answer(200, { applications: state.applications })
    }

    if (method === 'POST' && path === '/api/v1/applications') {
      const body = JSON.parse(request.postData() ?? '{}') as Partial<MockApp> & { adopt?: boolean }
      const created: MockApp = {
        id: `app-${state.applications.length + 1}`,
        name: body.name ?? '',
        mode: body.mode === 'swarm' ? 'swarm' : 'compose',
        target_id: body.target_id ?? '',
        namespace: body.namespace ?? '',
        latest_version_id: '',
        successful_version_id: '',
        config: { yaml: '', variables: {} },
      }
      state.applications = [...state.applications, created]
      return answer(201, created)
    }

    if (parts[2] === 'applications' && parts.length === 4) {
      const app = state.applications.find((item) => item.id === parts[3])
      if (!app) {
        return answer(404, { code: 'app_not_found', message: '未找到该应用' })
      }
      if (method === 'GET') {
        return answer(200, app)
      }
      return answer(405, { code: 'method_not_allowed', message: '该端点只接受 GET 请求' })
    }

    if (parts[2] === 'applications' && parts[4] === 'preview' && method === 'POST') {
      const app = state.applications.find((item) => item.id === parts[3])
      if (!app) {
        return answer(404, { code: 'app_not_found', message: '未找到该应用' })
      }
      const body = JSON.parse(request.postData() ?? '{}') as {
        base_version_id?: string
        config?: { yaml?: string; variables?: Record<string, string> }
      }
      if (state.stale || (body.base_version_id ?? '') !== app.latest_version_id) {
        return answer(409, { code: 'conflict', message: '部署请求与应用的当前状态不一致，请重新预览' })
      }
      const yaml = body.config?.yaml ?? ''
      const variables = body.config?.variables ?? {}
      if (state.previewRefused || yaml.trim() === '' || !yaml.includes('services:')) {
        return answer(400, {
          code: 'invalid_config',
          message: '配置的格式不合法',
          fields: [{ path: 'services', message: '配置必须定义 services', line: 1 }],
        })
      }
      const risks: string[] = privilegedServicesOf(yaml).map(
        (name) => `服务 ${name} 以特权模式运行`,
      )
      const notices: string[] = []
      for (const name of referencesOf(yaml)) {
        if (!(name in variables)) {
          notices.push(`变量 ${name} 没有提供，将按空字符串处理`)
        }
      }
      const warnings = [...risks, ...notices]
      return answer(200, {
        digest: digestOf(yaml, variables),
        removed_services: state.removedServices,
        warnings,
        risk_warnings: risks,
        inventory_revision: 'revision-1',
        requires_confirmation: risks.length > 0 || state.removedServices.length > 0,
      })
    }

    if (parts[2] === 'applications' && parts[4] === 'deployments' && method === 'POST') {
      const app = state.applications.find((item) => item.id === parts[3])
      if (!app) {
        return answer(404, { code: 'app_not_found', message: '未找到该应用' })
      }
      const body = JSON.parse(request.postData() ?? '{}') as {
        idempotency_key?: string
        base_version_id?: string
        preview_digest?: string
        inventory_revision?: string
        config?: { yaml?: string; variables?: Record<string, string> }
        confirmed_removal?: { digest?: string; services?: string[]; inventory_revision?: string }
        accept_warnings?: boolean
      }
      const key = body.idempotency_key ?? ''
      state.submissions.push(key)
      // The key of a request that was answered already answers with the
      // task of the first request.
      const known = Object.values(state.deployments).find((task) => task.app_id === app.id)
      if (key !== '' && known && known.id === `task-of-${key}`) {
        return answer(201, known)
      }
      const yaml = body.config?.yaml ?? ''
      const variables = body.config?.variables ?? {}
      if (!body.preview_digest) {
        return answer(409, { code: 'conflict', message: '部署请求与应用的当前状态不一致，请重新预览' })
      }
      if (body.preview_digest !== digestOf(yaml, variables)) {
        return answer(409, { code: 'conflict', message: '部署请求与应用的当前状态不一致，请重新预览' })
      }
      if (state.stale || (body.base_version_id ?? '') !== app.latest_version_id) {
        return answer(409, { code: 'conflict', message: '部署请求与应用的当前状态不一致，请重新预览' })
      }
      if (state.removedServices.length > 0) {
        const confirmation = body.confirmed_removal ?? {}
        if (
          (confirmation.services ?? []).join(',') !== state.removedServices.join(',') ||
          confirmation.digest !== body.preview_digest ||
          confirmation.inventory_revision !== body.inventory_revision
        ) {
          return answer(422, {
            code: 'confirmation_required',
            message: '该部署需要管理员明确确认后才能执行',
          })
        }
      }
      const version: MockVersion = {
        id: `version-${state.versions.length + 1}`,
        app_id: app.id,
        created_at: '2026-10-25T09:00:00Z',
        config: { yaml, variables },
      }
      state.versions = [version, ...state.versions]
      app.latest_version_id = version.id
      app.config = { yaml, variables }
      const task: MockDeployment = {
        id: `task-of-${key}`,
        app_id: app.id,
        version_id: version.id,
        agent_id: app.target_id,
        state: state.outcome.state,
        last_seq: 1,
        result_code: 'compose_ready',
        result_message: state.outcome.message,
        rolled_back: state.outcome.rolledBack,
        partial_update: state.outcome.partial,
        dispatch_started_at: '2026-10-25T09:00:01Z',
        manual_resolution: null,
        created_at: '2026-10-25T09:00:00Z',
        updated_at: '2026-10-25T09:00:01Z',
        finished_at: '2026-10-25T09:00:01Z',
      }
      state.deployments[task.id] = task
      return answer(201, {
        id: task.id,
        app_id: task.app_id,
        version_id: task.version_id,
        agent_id: task.agent_id,
        state: task.state,
        last_seq: task.last_seq,
        digest: body.preview_digest,
      })
    }

    if (parts[2] === 'applications' && parts[4] === 'versions' && parts.length === 5 && method === 'GET') {
      // The control端 lists the newest version first.
      const versions = state.versions
        .filter((version) => version.app_id === parts[3])
        .sort((left, right) =>
          left.created_at === right.created_at
            ? right.id.localeCompare(left.id)
            : right.created_at.localeCompare(left.created_at),
        )
      return answer(200, {
        versions: versions.map((version) => ({ id: version.id, created_at: version.created_at })),
        total: versions.length,
        limit: 20,
        offset: 0,
      })
    }

    if (parts[2] === 'applications' && parts[4] === 'versions' && parts.length === 6 && method === 'GET') {
      const version = state.versions.find((item) => item.app_id === parts[3] && item.id === parts[5])
      if (!version) {
        return answer(404, { code: 'version_not_found', message: '未找到该版本' })
      }
      return answer(200, version)
    }

    if (parts[2] === 'deployments' && parts.length === 4) {
      const task = state.deployments[parts[3]]
      if (!task) {
        return answer(404, { code: 'deployment_not_found', message: '未找到该部署任务' })
      }
      if (method === 'GET') {
        state.reads += 1
        return answer(200, task)
      }
      return answer(405, { code: 'method_not_allowed', message: '该端点只接受 GET 请求' })
    }

    if (parts[2] === 'deployments' && parts[4] === 'output' && method === 'GET') {
      const task = state.deployments[parts[3]]
      if (!task) {
        return answer(404, { code: 'deployment_not_found', message: '未找到该部署任务' })
      }
      const text = 'Creating network site_done\n'
      return answer(200, {
        task_id: task.id,
        fragments: [{ offset: 0, data: Buffer.from(text, 'utf-8').toString('base64'), truncated: false }],
        total_bytes: text.length,
        truncated: false,
      })
    }

    if (parts[2] === 'deployments' && parts[4] === 'reconcile' && method === 'POST') {
      const task = state.deployments[parts[3]]
      if (!task) {
        return answer(404, { code: 'deployment_not_found', message: '未找到该部署任务' })
      }
      task.state = 'failed'
      task.result_message = '服务 web 已经停止'
      task.finished_at = '2026-10-25T09:05:00Z'
      return answer(200, task)
    }

    if (parts[2] === 'deployments' && parts[4] === 'resolve' && method === 'POST') {
      const task = state.deployments[parts[3]]
      if (!task) {
        return answer(404, { code: 'deployment_not_found', message: '未找到该部署任务' })
      }
      const body = JSON.parse(request.postData() ?? '{}') as {
        expected_seq?: number
        executor_stopped?: boolean
        reason?: string
      }
      if (!body.executor_stopped || (body.reason ?? '').trim() === '') {
        return answer(400, { code: 'validation_failed', message: '提交的内容不合法' })
      }
      task.manual_resolution = {
        seq: body.expected_seq ?? task.last_seq,
        reason: body.reason ?? '',
        pending: false,
      }
      task.state = 'failed'
      task.result_code = 'manual_resolution'
      task.finished_at = '2026-10-25T09:10:00Z'
      return answer(202, task)
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

// setConfig writes a configuration into the editor of the page.
async function setConfig(page: Page, text: string) {
  const editor = page.locator('.cm-content')
  await editor.click()
  await page.keyboard.press('Control+A')
  await page.keyboard.insertText(text)
}

test('lists the applications of the deployment', async ({ page }) => {
  await installApi(page, runningState())
  await logIn(page)

  await page.getByRole('link', { name: '站点管理' }).click()
  await expect(page).toHaveURL(/\/applications$/)
  await expect(page.getByRole('rowheader', { name: '网站' })).toBeVisible()
  await expect(page.getByRole('cell', { name: 'Compose' })).toBeVisible()
  await expect(page.getByRole('cell', { name: 'site' })).toBeVisible()
})

test('creates an application and opens it', async ({ page }) => {
  const state = emptyState()
  // The administrator is initialized: the page has to show the login form,
  // not the form that creates the first administrator.
  state.initialized = true
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications')

  await expect(page.getByText('还没有应用。')).toBeVisible()
  await page.getByRole('button', { name: '新建应用' }).click()

  await page.getByLabel('名称', { exact: true }).fill('博客')
  await page.getByLabel('目标', { exact: true }).selectOption('node-1')
  await page.getByLabel('命名空间', { exact: true }).fill('blog')
  await page.getByRole('button', { name: '创建应用' }).click()

  await expect(page).toHaveURL(/\/applications\/app-1$/)
  await expect(page.getByRole('heading', { name: '博客' })).toBeVisible()
})

test('shows the impact of a configuration before it is deployed', async ({ page }) => {
  const state = runningState()
  state.removedServices = ['legacy']
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:1.27\n    privileged: true\n')
  await page.getByRole('button', { name: '预览影响' }).click()

  await expect(page.getByText('服务 web 以特权模式运行')).toBeVisible()
  await expect(page.getByText('这次部署会删除这些服务：')).toBeVisible()
  await expect(page.getByRole('listitem').filter({ hasText: 'legacy' })).toBeVisible()

  // The deployment waits for the confirmations of the administrator.
  await expect(page.getByRole('button', { name: '保存并部署' })).toBeDisabled()
  await page.getByLabel('我确认删除这些服务').check()
  await page.getByLabel('我已知晓这些风险').check()
  await expect(page.getByRole('button', { name: '保存并部署' })).toBeEnabled()
})

test('refuses an invalid configuration and keeps the input', async ({ page }) => {
  await installApi(page, runningState())
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'not a compose document\n')
  await page.getByRole('button', { name: '预览影响' }).click()

  await expect(page.getByRole('alert')).toContainText('配置的格式不合法')
  await expect(page.getByRole('alert')).toContainText('配置必须定义 services')
  await expect(page.locator('.cm-content')).toContainText('not a compose document')
})

test('warns about a variable that was not provided', async ({ page }) => {
  await installApi(page, runningState())
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:${TAG}\n')
  await page.getByRole('button', { name: '预览影响' }).click()

  await expect(page.getByText('变量 TAG 没有提供，将按空字符串处理')).toBeVisible()
})

test('does not deploy when the removal is not confirmed', async ({ page }) => {
  const state = runningState()
  state.removedServices = ['legacy']
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:1.28\n')
  await page.getByRole('button', { name: '预览影响' }).click()

  await expect(page.getByText('这次部署会删除这些服务：')).toBeVisible()
  await expect(page.getByRole('button', { name: '保存并部署' })).toBeDisabled()
  expect(state.submissions).toHaveLength(0)
})

test('sends one deployment when the button is clicked twice', async ({ page }) => {
  const state = runningState()
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:1.28\n')
  await page.getByRole('button', { name: '预览影响' }).click()
  await expect(page.getByRole('button', { name: '保存并部署' })).toBeEnabled()

  const button = page.getByRole('button', { name: '保存并部署' })
  await button.dblclick()

  await expect(page.getByText('部署已经提交。')).toBeVisible()
  expect(state.submissions).toHaveLength(1)
})

test('keeps the local edit when the page is out of date', async ({ page }) => {
  const state = runningState()
  state.stale = true
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:1.29\n')
  await page.getByRole('button', { name: '预览影响' }).click()

  await expect(page.getByRole('alert')).toContainText('本页面的编辑已保留')
  await expect(page.locator('.cm-content')).toContainText('nginx:1.29')
  expect(state.submissions).toHaveLength(0)
})

test('keeps the state of a running deployment after a reload of the page', async ({ page }) => {
  const state = runningState()
  state.deployments['task-1'].state = 'running'
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1?deployment=task-1')

  await expect(page.getByText('部署中')).toBeVisible()

  await page.reload()

  await expect(page.getByText('部署中')).toBeVisible()
  expect(state.submissions).toHaveLength(0)
})

test('reads the deployment again when the stream of the events breaks', async ({ page }) => {
  const state = runningState()
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1?deployment=task-1')

  await expect(page.getByText('部署成功')).toBeVisible()
  // The browser opens the stream again and again. The page reads the
  // deployment instead of sending it a second time.
  await expect.poll(() => state.streams, { timeout: 15_000 }).toBeGreaterThan(1)
  await expect.poll(() => state.reads, { timeout: 15_000 }).toBeGreaterThan(1)
  expect(state.submissions).toHaveLength(0)
})

test('shows a partial update', async ({ page }) => {
  const state = runningState()
  state.deployments['task-1'].state = 'failed'
  state.deployments['task-1'].partial_update = true
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1?deployment=task-1')

  await expect(page.getByText('部署失败')).toBeVisible()
  await expect(page.getByText('部分更新')).toBeVisible()
})

test('shows a rollback of the target', async ({ page }) => {
  const state = runningState()
  state.deployments['task-1'].state = 'failed'
  state.deployments['task-1'].rolled_back = true
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1?deployment=task-1')

  await expect(page.getByText('目标已回滚到本次部署之前的状态。')).toBeVisible()
})

test('shows a deployment whose outcome is unknown as待确认', async ({ page }) => {
  const state = runningState()
  state.deployments['task-1'].state = 'uncertain'
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1?deployment=task-1')

  await expect(page.getByText('状态待确认')).toBeVisible()
  await expect(page.getByText('部署成功')).toHaveCount(0)
  await expect(page.getByText('部署失败')).toHaveCount(0)

  await page.getByRole('button', { name: '人工核对并释放' }).click()
  await expect(page.getByText('平台无法证明执行端已停止。')).toBeVisible()

  await page.getByLabel('我已确认执行端不再继续操作').check()
  await page.getByLabel('释放原因', { exact: true }).fill('节点已经关机')
  await page.getByRole('button', { name: '释放', exact: true }).click()

  await expect(page.getByText('释放已经提交。')).toBeVisible()
})

test('deploys a version of the history again', async ({ page }) => {
  const state = runningState()
  state.versions.push({
    id: 'version-0',
    app_id: 'app-1',
    created_at: '2026-10-24T08:00:00Z',
    config: { yaml: 'services:\n  web:\n    image: nginx:1.25\n', variables: {} },
  })
  await installApi(page, state)
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '部署记录', exact: true }).click()
  await expect(page.getByText('重新部署该版本只会把该版本的配置再次部署到目标上，不会恢复已经删除的数据。')).toBeVisible()

  // The history lists the newest version first, so the administrator deploys
  // the older version again by its row.
  const oldVersion = page.getByRole('row').filter({ hasText: 'version-0' })
  await oldVersion.getByRole('button', { name: '重新部署该版本' }).click()

  await expect(page.locator('.cm-content')).toContainText('nginx:1.25')
  await expect(page.getByRole('button', { name: '保存并部署' })).toBeEnabled()
})

test('keeps the configuration out of the storage of the browser', async ({ page }) => {
  await installApi(page, runningState())
  await logIn(page)
  await page.goto('/applications/app-1')

  await page.getByRole('button', { name: '配置', exact: true }).click()
  await setConfig(page, 'services:\n  web:\n    image: nginx:1.30\n')

  expect(await page.evaluate(() => window.localStorage.length)).toBe(0)
  expect(await page.evaluate(() => window.sessionStorage.length)).toBe(0)
})
