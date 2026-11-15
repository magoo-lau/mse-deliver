import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import '@vue-flow/minimap/dist/style.css'
import * as Icons from '@element-plus/icons-vue'
import App from './App.vue'

const app = createApp(App)
// 紧凑风格：全局 small（与仓库前端基线一致的密度优先口径）
app.use(ElementPlus, { size: 'small', locale: zhCn })
for (const [name, comp] of Object.entries(Icons)) app.component(name, comp)
app.mount('#app')
