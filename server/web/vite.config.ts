import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 构建产物直接输出到 Go 内嵌目录；开发时 /api 代理到本地 panel（自签 HTTPS）。
// 不改写 Host（changeOrigin 默认 false），后端同源校验比较的是浏览器看到的 localhost:5173。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../boxctl/internal/webui/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': { target: 'https://127.0.0.1:18443', secure: false, ws: true },
    },
  },
})
