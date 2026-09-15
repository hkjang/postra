import {defineConfig} from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import {fileURLToPath, URL} from 'node:url'

// Keep the browser-visible Host so exact-origin login/session CSRF validation
// works in development too. No proxy or Node server is included in releases.
const backend = process.env.POSTRA_DEV_API_TARGET || 'http://127.0.0.1:8480'
const browserProxy = {target: backend, changeOrigin: false}

export default defineConfig({
  base: '/app/',
  publicDir: '../assets',
  plugins: [react(), tailwindcss()],
  resolve: {alias: {'@': fileURLToPath(new URL('./src', import.meta.url))}},
  build: {
    outDir: '../internal/transport/spa/assets', emptyOutDir: true,
    sourcemap: false, target: 'es2022',
  },
  server: {proxy: {'/api': browserProxy, '/auth': browserProxy, '/tracking': browserProxy, '/momento': browserProxy}},
  test: {environment: 'jsdom', setupFiles: ['./src/test-setup.ts'], exclude: ['tests/e2e/**', 'node_modules/**']},
})
