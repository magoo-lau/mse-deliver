<template>
  <div class="login-wrap">
    <div class="login-card">
      <div class="login-title">deliver-console</div>
      <div class="login-sub">多机安装控制台（控制面/节点代理模型，agent 出站连接）</div>
      <el-form @submit.prevent>
        <el-form-item>
          <el-input v-model="password" type="password" show-password placeholder="admin 密码（首启见启动控制台）" size="large" @keyup.enter="doLogin" />
        </el-form-item>
        <el-button type="primary" size="large" style="width: 100%" :loading="loading" @click="doLogin">登录</el-button>
      </el-form>
      <div class="login-tip">忘记密码：控制台机执行 deliver-console -reset-pass 重置</div>
    </div>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { authed } from '../nav'
import { api, setConsoleToken } from '../api'

const password = ref('')
const loading = ref(false)

async function doLogin() {
  if (!password.value) return
  loading.value = true
  try {
    const res = await api.post('/api/login', { password: password.value })
    setConsoleToken(res.token)
    authed.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-wrap { height: 100vh; display: flex; align-items: center; justify-content: center; background: var(--el-fill-color-darker); }
.login-card { width: 380px; padding: 36px 32px; background: var(--el-bg-color); border-radius: 8px; border: 1px solid var(--el-border-color); }
.login-title { font-size: 20px; font-weight: 600; text-align: center; color: var(--el-text-color-primary); }
.login-sub { font-size: 12px; text-align: center; color: var(--el-text-color-secondary); margin: 6px 0 24px; }
.login-tip { margin-top: 14px; font-size: 11px; color: var(--el-text-color-placeholder); text-align: center; }
</style>
