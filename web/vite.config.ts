import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const target = process.env.OC_API_TARGET || 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  base: '/',
  server: {
    port: 3000,
    proxy: {
      '/auth': target,
      '/api': { target, ws: true },
      '/webhooks': target,
    },
  },
})
