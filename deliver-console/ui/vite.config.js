import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// dev 代理到本机控制台后端（缺省 7870）
export default defineConfig({
  plugins: [vue()],
  server: { port: 5181, proxy: { '/api': 'http://127.0.0.1:7870', '/download': 'http://127.0.0.1:7870' } }
})
