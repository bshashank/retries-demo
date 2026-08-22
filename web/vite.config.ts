import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The production bundle is embedded into the Go binary and served from the
// binary's own file handler, so every asset reference must be relative.
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    // `npm run dev` talks to the Go server running on :8080.
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        // SSE must not be buffered by the proxy.
        ws: false,
      },
    },
  },
})
