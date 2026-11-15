<template>
  <Login v-if="!authed" />
  <el-container v-else class="layout">
    <el-aside width="200px" class="aside">
      <div class="brand">
        <span class="brand-main">deliver-console</span>
        <span class="brand-sub">安装控制台</span>
      </div>
      <el-menu :default-active="activeView" @select="onSelect" class="menu">
        <el-menu-item-group title="安装">
          <el-menu-item index="Hosts"><el-icon><Cpu /></el-icon>主机池</el-menu-item>
          <el-menu-item index="Sources"><el-icon><Download /></el-icon>安装源</el-menu-item>
          <el-menu-item index="Packages"><el-icon><Box /></el-icon>部署包</el-menu-item>
          <el-menu-item index="Design"><el-icon><SetUp /></el-icon>编排安装</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="运维">
          <el-menu-item index="Applications"><el-icon><Grid /></el-icon>应用中心</el-menu-item>
        </el-menu-item-group>
        <el-menu-item-group title="执行">
          <el-menu-item index="Sessions"><el-icon><VideoPlay /></el-icon>安装记录</el-menu-item>
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
      <div class="aside-note">agent 出站连接 console（无需直登目标机）</div>
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
      <el-button type="primary" @click="doChangePass">保存</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, computed } from 'vue'
import { ElMessage } from 'element-plus'
import { activeView, authed, gotoView } from './nav'
import { api, setConsoleToken, consoleToken } from './api'
import Login from './views/Login.vue'
import Hosts from './views/Hosts.vue'
import Sources from './views/Sources.vue'
import Packages from './views/Packages.vue'
import Design from './views/Design.vue'
import Applications from './views/Applications.vue'
import Sessions from './views/Sessions.vue'
import Guide from './views/Guide.vue'

const views = { Hosts, Sources, Packages, Design, Applications, Sessions, Guide }
const passDialog = ref(false)
const passForm = reactive({ oldPassword: '', newPassword: '', confirm: '' })

function onSelect(name) {
  gotoView(name)
}

async function doChangePass() {
  if (passForm.newPassword !== passForm.confirm) {
    ElMessage.warning('两次新密码不一致')
    return
  }
  try {
    await api.put('/api/settings/password', { old: passForm.oldPassword, new: passForm.newPassword })
    ElMessage.success('密码已修改')
    passDialog.value = false
    passForm.oldPassword = passForm.newPassword = passForm.confirm = ''
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function doLogout() {
  try { await api.post('/api/logout') } catch { /* 会话已失效亦可 */ }
  setConsoleToken('')
  authed.value = false
}

// 启动静默探活：已存 token 则直接进入主界面
;(async () => {
  if (consoleToken()) {
    try {
      const who = await api.get('/api/whoami')
      if (who.authed) authed.value = true
    } catch { /* token 失效停留登录页 */ }
  }
})()
</script>

<style scoped>
.layout { height: 100vh; }
.aside { display: flex; flex-direction: column; border-right: 1px solid var(--el-border-color); background: var(--el-bg-color); }
.brand { padding: 14px 16px 10px; display: flex; flex-direction: column; }
.brand-main { font-weight: 600; font-size: 15px; color: var(--el-text-color-primary); }
.brand-sub { font-size: 12px; color: var(--el-text-color-secondary); margin-top: 2px; }
.menu { flex: 1; border-right: none; overflow-y: auto; }
.aside-foot { padding: 10px 16px; display: flex; gap: 10px; align-items: center; font-size: 12px; color: var(--el-text-color-secondary); border-top: 1px solid var(--el-border-color); }
.foot-link { color: var(--el-color-primary); cursor: pointer; }
.foot-link:hover { text-decoration: underline; }
.aside-note { padding: 4px 16px 10px; font-size: 11px; color: var(--el-text-color-placeholder); }
.main { background: var(--el-fill-color-lighter); overflow: auto; padding: 14px 16px; }
</style>
