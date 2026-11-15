// 视图导航（无路由 SPA）：模块级响应式当前页；authed = 登录态（App.vue 切登录页/主界面）
import { ref } from 'vue'

export const activeView = ref('Hosts')

export const authed = ref(false)

// 部署包详情「去编排」带入的预选包 ID：Design 挂载时消费并清零（跳过选包步；包已删则留在选包步）
export const designPkgId = ref(0)

export function gotoView(name) {
  activeView.value = name
}
