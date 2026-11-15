import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// dev 代理到本机工作台后端（缺省 7860）
export default defineConfig({
  plugins: [vue()],
  server: { port: 5180, proxy: { '/api': 'http://127.0.0.1:7860' } }
})
