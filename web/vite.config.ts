import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// The AgentMux server address. In development the frontend runs on its own
// port and proxies /api to the Go server, so the browser sees one origin and
// no CORS preflight is involved. In production the Go server serves this
// build directly and the proxy is unused.
const AGENTMUX_SERVER = process.env.AGENTMUX_SERVER ?? 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: AGENTMUX_SERVER,
        changeOrigin: false,
        // The terminal endpoint is a WebSocket under the same prefix. Without
        // this the dev server answers the upgrade request as an ordinary HTTP
        // request and the browser's socket never opens - which looks exactly
        // like a server that is not running, and sends anybody debugging it to
        // the wrong process.
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    // The Go server serves this directory; source maps are not shipped.
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
