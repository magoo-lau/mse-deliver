<template>
  <Login v-if="!authed" />
  <el-container v-else class="layout">
    <el-aside width="200px" class="aside">
      <div class="brand">
        <span class="brand-main">deliver-studio</span>
        <span class="brand-sub">通用交付工作台</span>
      </div>
      <el-menu :default-active="activeView" @select="onSelect" class="menu">
        <el-menu-item-group title="工作台">
          <el-menu-item index="Home"><el-icon><HomeFilled /></el-icon>首页</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="交付">
          <el-menu-item index="Suites"><el-icon><Box /></el-icon>套件打包</el-menu-item>
          <el-menu-item index="Deliveries"><el-icon><Promotion /></el-icon>交付包</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="定义">
          <el-menu-item index="Apps"><el-icon><Monitor /></el-icon>应用定义</el-menu-item>
          <el-menu-item index="Components"><el-icon><Grid /></el-icon>组件目录</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="资源">
          <el-menu-item index="Materials"><el-icon><Files /></el-icon>制品库</el-menu-item>
          <el-menu-item index="Images"><el-icon><Coin /></el-icon>镜像仓</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="帮助">
          <el-menu-item index="Guide"><el-icon><Reading /></el-icon>使用指南</el-menu-item>
        </el-menu-item-group>
      </el-menu>
      <div class="aside-foot">
        <span>admin</span>
        <span class="foot-link" @click="passDialog = true">改密码</span>
        <span class="foot-link" @click="doLogout">退出</span>
      </div>
      <div class="aside-note">打包需本机 docker（镜像构建/导出）</div>
    </el-aside>
    <el-main class="main">
      <component :is="views[activeView]" />
    </el-main>
  </el-container>

  <el-dialog v-model="passDialog" title="修改管理员密码" width="420px">
    <el-form label-width="90px">
      <el-form-item label="当前密码">
        <el-input v-model="passForm.oldPassword" type="password" show-password />
      </el-form-item>
      <el-form-item label="新密码">
        <el-input v-model="passForm.newPassword" type="password" show-password placeholder="至少 6 位" />
      </el-form-item>
      <el-form-item label="确认新密码">
        <el-input v-model="passForm.confirm" type="password" show-password @keyup.enter="doChangePass" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="passDialog = false">取消</el-button>
      <el-button type="primary" :loading="changing" @click="doChangePass">确认修改</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { onMounted, reactive, ref, shallowRef } from 'vue'
import { ElMessage } from 'element-plus'
import Home from './views/Home.vue'
import Suites from './views/Suites.vue'
import Deliveries from './views/Deliveries.vue'
import Apps from './views/Apps.vue'
import Components from './views/Components.vue'
import Materials from './views/Materials.vue'
import Images from './views/Images.vue'
import Guide from './views/Guide.vue'
import Login from './views/Login.vue'
import { activeView, authed } from './nav'
import { api, setStudioToken, studioToken } from './api'

const views = shallowRef({
  Home, Suites, Deliveries, Apps, Components, Materials, Images, Guide
})
function onSelect(key) {
  activeView.value = key
}

// 启动探活：已有 token 则免登录直入（过期 token 由首个 API 401 兜底回登录页）
onMounted(async () => {
  if (!studioToken()) return
  try {
    const out = await api.get('/api/whoami')
    authed.value = !!out.loggedIn
  } catch { /* 401 已由 api 层处理回登录页 */ }
})

async function doLogout() {
  try { await api.post('/api/logout') } catch { /* 会话失效亦可退出 */ }
  setStudioToken('')
  authed.value = false
}

const passDialog = ref(false)
const changing = ref(false)
const passForm = reactive({ oldPassword: '', newPassword: '', confirm: '' })

async function doChangePass() {
  if (!passForm.oldPassword || !passForm.newPassword) {
    ElMessage.warning('请填写当前密码与新密码')
    return
  }
  if (passForm.newPassword !== passForm.confirm) {
    ElMessage.warning('两次新密码不一致')
    return
  }
  changing.value = true
  try {
    await api.put('/api/settings/password', {
      oldPassword: passForm.oldPassword,
      newPassword: passForm.newPassword
    })
    passDialog.value = false
    ElMessage.success('密码已修改，全部会话已失效，请重新登录')
    setStudioToken('')
    authed.value = false
    Object.assign(passForm, { oldPassword: '', newPassword: '', confirm: '' })
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    changing.value = false
  }
}
</script>

<style>
html, body, #app { height: 100%; margin: 0; }
.layout { height: 100%; }
.aside { border-right: 1px solid var(--el-border-color-light); display: flex; flex-direction: column; background: var(--el-bg-color); }
.brand { padding: 14px 16px 10px; }
.brand-main { font-weight: 600; font-size: 15px; display: block; }
.brand-sub { color: var(--el-text-color-secondary); font-size: 12px; }
.menu { border-right: none; flex: 1; }
.aside-foot { padding: 8px 16px 2px; color: var(--el-text-color-secondary); font-size: 12px; display: flex; gap: 10px; align-items: center; }
.foot-link { color: var(--el-color-primary); cursor: pointer; }
.foot-link:hover { opacity: .8; }
.aside-note { padding: 2px 16px 10px; color: var(--el-text-color-placeholder); font-size: 12px; }
.main { background: var(--el-fill-color-lighter); padding: 12px; }
.page-card + .page-card { margin-top: 10px; }
.mono { font-family: Consolas, Monaco, monospace; font-size: 12px; }
</style>
