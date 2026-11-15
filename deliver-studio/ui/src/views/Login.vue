<template>
  <div class="login-wrap">
    <el-card shadow="never" class="login-card">
      <div class="login-title">deliver-studio</div>
      <div class="login-sub">通用交付工作台 · 登录</div>
      <el-form label-position="top" @submit.prevent="doLogin">
        <el-form-item label="账号">
          <el-input v-model="user" placeholder="admin" :disabled="loading" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input v-model="pass" type="password" show-password placeholder="管理员密码"
                    :disabled="loading" @keyup.enter="doLogin" />
        </el-form-item>
        <el-button type="primary" class="login-btn" :loading="loading" @click="doLogin">登 录</el-button>
      </el-form>
      <div class="login-tip">
        首次启动的随机密码打印在启动控制台；忘记密码可用 <span class="mono">-reset-pass</span>
        参数重启工作台重新生成（旧密码与会话全部失效）。
      </div>
    </el-card>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api, setStudioToken } from '../api'
import { authed } from '../nav'

const user = ref('admin')
const pass = ref('')
const loading = ref(false)

async function doLogin() {
  if (!pass.value) {
    ElMessage.warning('请输入密码')
    return
  }
  loading.value = true
  try {
    const out = await api.post('/api/login', { user: user.value.trim() || 'admin', password: pass.value })
    setStudioToken(out.token)
    authed.value = true
    pass.value = ''
    ElMessage.success('登录成功')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-wrap {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--el-fill-color-lighter);
}
.login-card { width: 360px; }
.login-title { font-size: 18px; font-weight: 600; text-align: center; }
.login-sub { color: var(--el-text-color-secondary); font-size: 12px; text-align: center; margin: 2px 0 14px; }
.login-btn { width: 100%; margin-top: 4px; }
.login-tip { color: var(--el-text-color-secondary); font-size: 12px; line-height: 1.7; margin-top: 12px; }
.mono { font-family: Consolas, Monaco, monospace; }
</style>
