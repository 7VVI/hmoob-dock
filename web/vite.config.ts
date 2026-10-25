import { readFileSync } from 'node:fs'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// The address of the control端 of the development environment. It can be
// changed with the environment variable HMOOB_CONTROL_URL.
const controlURL = process.env.HMOOB_CONTROL_URL ?? 'http://127.0.0.1:8080'

// The certificate of the development server. Without the two variables the
// server answers with plain HTTP, so a developer can start it without a
// certificate at all.
const certificate = process.env.HMOOB_DEV_CERT
const certificateKey = process.env.HMOOB_DEV_KEY

export default defineConfig({
  plugins: [vue()],
  server: {
    host: 'localhost',
    port: 5173,
    strictPort: true,
    https:
      certificate && certificateKey
        ? { cert: readFileSync(certificate), key: readFileSync(certificateKey) }
        : undefined,
    proxy: {
      '/api': {
        target: controlURL,
        // The control端 compares the host of the request with the host of
        // the origin of the browser, so the original Host header has to
        // survive the proxy.
        changeOrigin: false,
        // The development certificate of the control端 is not part of the
        // trust store of the system.
        secure: false,
      },
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts', '*.test.ts'],
    restoreMocks: true,
    unstubGlobals: true,
  },
})
