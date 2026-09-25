import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时将 /api 代理到 Go 后端（harness serve -addr :8080）
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
