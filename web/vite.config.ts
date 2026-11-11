import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Wails 桌面模式：前端静态编译后由 Wails AssetServer 直接服务，
// 后端经 window.go 绑定方法直连，开发期无需 /api 代理。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
  },
})
