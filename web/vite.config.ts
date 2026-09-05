import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { svelteTesting } from '@testing-library/svelte/vite'

// Build into dist/ (embedded by the Go binary). Dev server proxies API calls
// to the local daemon so `npm run dev` + `make dev` work side by side.
export default defineConfig({
  plugins: [svelte(), svelteTesting()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/healthz': 'http://127.0.0.1:8080',
    },
  },
  test: { environment: 'jsdom' },
})
