import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    allowedHosts: true,
    proxy: {
      '/admin': { target: 'http://localhost:8080', changeOrigin: true },
      '/auth':  { target: 'http://localhost:8080', changeOrigin: true },
      '/v1':    { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
})
