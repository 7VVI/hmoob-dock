import { afterEach, expect, test, vi } from 'vitest'
import {
  baseURLFrom,
  ignoreHTTPSErrorsFor,
  reuseExistingServerFrom,
} from './playwright.config'

afterEach(() => {
  vi.unstubAllEnvs()
  vi.resetModules()
})

test('serves the tests from the address of the environment', () => {
  expect(baseURLFrom({})).toBe('http://localhost:5173')
  expect(baseURLFrom({ HMOOB_E2E_BASE_URL: 'https://localhost:5173' })).toBe(
    'https://localhost:5173',
  )
})

test('accepts the certificate of a development server of its own', () => {
  expect(ignoreHTTPSErrorsFor('http://localhost:5173')).toBe(false)
  expect(ignoreHTTPSErrorsFor('https://localhost:5173')).toBe(true)
})

test('uses a server that is already running only when the developer says so', () => {
  expect(reuseExistingServerFrom({})).toBe(false)
  expect(reuseExistingServerFrom({ HMOOB_E2E_REUSE_SERVER: '1' })).toBe(true)
})

test('gives the configuration the address of the environment', async () => {
  vi.stubEnv('HMOOB_E2E_BASE_URL', 'https://localhost:5173')
  vi.resetModules()

  const config = (await import('./playwright.config')).default
  const server = Array.isArray(config.webServer) ? config.webServer[0] : config.webServer

  expect(config.use?.baseURL).toBe('https://localhost:5173')
  expect(config.use?.ignoreHTTPSErrors).toBe(true)
  expect(server?.url).toBe('https://localhost:5173')
  expect(server?.ignoreHTTPSErrors).toBe(true)
  expect(server?.reuseExistingServer).toBe(false)
})

test('gives the configuration the permission to use a running server', async () => {
  vi.stubEnv('HMOOB_E2E_REUSE_SERVER', '1')
  vi.resetModules()

  const config = (await import('./playwright.config')).default
  const server = Array.isArray(config.webServer) ? config.webServer[0] : config.webServer

  expect(server?.reuseExistingServer).toBe(true)
})
