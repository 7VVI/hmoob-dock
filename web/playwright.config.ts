import { defineConfig, devices } from '@playwright/test'

// baseURLFrom reads the address of the development server of the frontend.
//
// The tests drive the frontend against a mocked API, so the development
// server of the frontend is all they need. The address can be changed with
// the environment variable HMOOB_E2E_BASE_URL, so the tests can also run
// against a server that serves HTTPS.
export function baseURLFrom(env: NodeJS.ProcessEnv = process.env): string {
  return env.HMOOB_E2E_BASE_URL ?? 'http://localhost:5173'
}

// ignoreHTTPSErrorsFor reports whether the server of the given address
// answers with a certificate of its own.
//
// A development server with a certificate of its own is not part of the
// trust store of the system, so the tests accept its certificate.
export function ignoreHTTPSErrorsFor(baseURL: string): boolean {
  return baseURL.startsWith('https:')
}

// reuseExistingServerFrom reports whether a server that is already running
// may be used.
//
// A server that is already running belongs to another state of the frontend,
// so it is only used when the developer says so with the environment
// variable HMOOB_E2E_REUSE_SERVER.
export function reuseExistingServerFrom(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.HMOOB_E2E_REUSE_SERVER === '1'
}

const baseURL = baseURLFrom()
const ignoreHTTPSErrors = ignoreHTTPSErrorsFor(baseURL)

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL,
    ignoreHTTPSErrors,
    trace: 'off',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  // The tests drive the frontend against a mocked API, so the development
  // server of the frontend is all they need. A server that serves HTTPS
  // needs the certificate of the development environment, which the two
  // variables HMOOB_DEV_CERT and HMOOB_DEV_KEY of the environment hold.
  webServer: {
    command: 'npm run dev',
    url: baseURL,
    ignoreHTTPSErrors,
    reuseExistingServer: reuseExistingServerFrom(),
    timeout: 60_000,
  },
})
