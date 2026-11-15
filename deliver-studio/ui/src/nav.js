// 视图导航（无路由 SPA）：模块级响应式当前页，各视图经 gotoView 跳转（首页快速入口 → 打包向导等）；
// authed = 登录态（App.vue 据此切登录页/主界面；api.js 401 时置 false）
import { ref } from 'vue'

export const activeView = ref('Home')

export const authed = ref(false)

// 套件打包页「出交付包」带入的预选套件 ID：交付包页挂载时消费并清零（直接开弹窗预选该套件）
export const deliverSuiteId = ref(0)

export function gotoView(name) {
  activeView.value = name
}
