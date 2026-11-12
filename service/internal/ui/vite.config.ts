import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev proxy target: the local dummy backend. Override for remote-dev
// against a deployed smoke stand by setting VITE_DEV_PROXY_TARGET.
const DEFAULT_DEV_PROXY_TARGET = 'http://localhost:8080'
const proxyTarget = process.env.VITE_DEV_PROXY_TARGET ?? DEFAULT_DEV_PROXY_TARGET

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: proxyTarget, changeOrigin: true },
      '/healthz': { target: proxyTarget, changeOrigin: true },
    },
  },
})
