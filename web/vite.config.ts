import {defineConfig} from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import {fileURLToPath, URL} from 'node:url'

export default defineConfig({
  base: '/app/',
  plugins: [react(), tailwindcss()],
  resolve: {alias: {'@': fileURLToPath(new URL('./src', import.meta.url))}},
  build: {
    outDir: '../internal/transport/spa/assets', emptyOutDir: true,
    sourcemap: false, target: 'es2022',
  },
  server: {proxy: {'/api': 'http://127.0.0.1:8480', '/ui': 'http://127.0.0.1:8480'}},
  test: {environment: 'jsdom', setupFiles: ['./src/test-setup.ts'], exclude: ['tests/e2e/**', 'node_modules/**']},
})
