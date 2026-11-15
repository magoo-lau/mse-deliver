<template>
  <div class="page">
    <div class="page-head">
      <h2>安装记录</h2>
      <div class="row-gap">
        <el-switch v-model="autoRefresh" active-text="自动刷新" />
        <el-button @click="load" :loading="loading">刷新</el-button>
      </div>
    </div>

    <el-card shadow="never" class="card">
      <el-table :data="sessions" size="small" @row-click="openDetail" highlight-current-row>
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="app" label="应用" min-width="130" />
        <el-table-column prop="version" label="版本" width="110" />
        <el-table-column label="模式" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="row.mode === 'incremental' ? 'warning' : 'primary'" effect="plain">{{ row.mode === 'incremental' ? '增量' : '全新' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag size="small" :type="statusType(row.status)" effect="dark">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="summary" label="摘要" min-width="220" show-overflow-tooltip />
        <el-table-column label="开始时间" width="165">
          <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-drawer v-model="detailOpen" size="80%" :title="detailTitle" @opened="startPolling" @closed="stopPolling">
      <template v-if="d">
        <el-alert v-if="d.session.status === 'done'" type="success" :closable="false" class="banner"
                  :title="'安装完成 —— 入口 ' + (d.entryUrl || '(入口服务未分配)')" />
        <el-alert v-else-if="d.session.status === 'failed'" type="error" :closable="false" class="banner"
                  :title="d.session.summary || '存在失败节点'" />
        <el-alert v-else-if="d.session.status === 'aborted'" type="warning" :closable="false" class="banner"
                  :title="d.session.summary || '安装已中止'" />
        <div v-for="w in d.warnings || []" :key="w" class="warn-line">⚠ {{ w }}</div>

        <div class="audit-bar">
          <el-tooltip v-if="d.session.pkgSha" :content="d.session.pkgSha" placement="top">
            <span class="mono audit-sha">包指纹 {{ d.session.pkgSha.slice(0, 12) }}…（版本 {{ d.session.version }}）</span>
          </el-tooltip>
          <span v-else class="audit-sha">版本 {{ d.session.version }}（旧会话无包指纹快照）</span>
          <div class="row-gap">
            <el-button v-if="d.session.status === 'running'" size="small" type="danger" plain @click="abort">
              中止安装
            </el-button>
            <el-button v-if="canExport" size="small" type="primary" plain @click="exportOrch">导出编排</el-button>
            <el-button v-if="canRollback" size="small" type="warning" plain @click="rollback">
              从此会话回退（介质回放）
            </el-button>
          </div>
        </div>

        <el-table :data="d.nodes" size="small" :row-class-name="rowClass">
          <el-table-column prop="key" label="节点" min-width="150" />
          <el-table-column label="类型" width="80">
            <template #default="{ row }">{{ kindLabel(row.kind) }}</template>
          </el-table-column>
          <el-table-column prop="title" label="说明" min-width="230" show-overflow-tooltip />
          <el-table-column label="状态" width="105">
            <template #default="{ row }">
              <el-tag size="small" :type="statusType(row.status)" effect="dark">{{ statusText(row.status) }}</el-tag>
              <div v-if="durOf(row)" class="dur">已 {{ durOf(row) }}</div>
            </template>
          </el-table-column>
          <el-table-column prop="message" label="消息" min-width="180" show-overflow-tooltip />
          <el-table-column label="日志" width="80">
            <template #default="{ row }">
              <el-button v-if="row.logCount > 0" link type="primary" @click.stop="openLogs(row)">查看</el-button>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="150" fixed="right">
            <template #default="{ row }">
              <template v-if="row.status === 'failed' || row.status === 'ok' || row.status === 'skipped'">
                <el-button link type="primary" @click.stop="act(row, 'retry')">重试</el-button>
              </template>
              <template v-if="row.status === 'failed' || row.status === 'pending'">
                <el-button link type="warning" @click.stop="act(row, 'skip')">跳过</el-button>
              </template>
              <template v-if="row.kind === 'gate' && row.status === 'pending'">
                <el-button link type="success" @click.stop="act(row, 'gate')">确认</el-button>
              </template>
            </template>
          </el-table-column>
        </el-table>
      </template>
    </el-drawer>

    <el-drawer v-model="logsOpen" size="60%" :title="'日志：' + logNode">
      <div ref="logBox" class="log-box">
        <div v-for="l in logLines" :key="l.id" class="log-line" :class="{ sys: l.node === '__' }">
          <span class="log-node">[{{ l.node }}]</span> {{ l.line }}
        </div>
      </div>
    </el-drawer>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtTime, downloadWithToken } from '../api'

const sessions = ref([])
const loading = ref(false)
const detailOpen = ref(false)
const logsOpen = ref(false)
const d = ref(null)
const detailTitle = ref('')
const autoRefresh = ref(true)
const logLines = ref([])
const logNode = ref('')
const logBox = ref(null)
let pollTimer = null
let logAfter = 0
let activeSessionId = 0

function statusType(st) {
  return { done: 'success', ok: 'success', running: 'primary', dispatched: 'primary', pending: 'info', failed: 'danger', skipped: 'warning', aborted: 'warning' }[st] || 'info'
}
function statusText(st) {
  return { done: '完成', ok: '成功', running: '执行中', dispatched: '已派发', pending: '待执行', failed: '失败', skipped: '已跳过', aborted: '已中止' }[st] || st
}
// 已派发/执行中节点的持续时长（随 2.5s 轮询刷新）——长时间无反馈时一眼看出卡了多久
function durOf(row) {
  if (!row.startedAt || (row.status !== 'dispatched' && row.status !== 'running')) return ''
  const s = Math.max(0, Math.floor((Date.now() / 1000 - row.startedAt)))
  if (s >= 3600) return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分'
  if (s >= 60) return Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒'
  return s + ' 秒'
}
function kindLabel(kind) {
  return { docker: '装 Docker', load: '装载', compose: '编排', systemd: '裸机', cmd: '命令', wait: '等待', gate: '门禁', plugin: '插件' }[kind] || kind
}
function rowClass({ row }) {
  return row.status === 'failed' ? 'row-failed' : ''
}

// 介质回退（ADR-6）：仅成功会话且有分配快照；回退=旧包+旧方案重放增量安装，库结构/数据不回滚
const canRollback = computed(() => {
  const s = d.value?.session
  if (!s || s.status !== 'done' || !s.placement) return false
  try { return Object.keys(JSON.parse(s.placement)).length > 0 } catch { return false }
})

// 导出编排：有分配快照的会话均可导出（架构分发到其他环境，敏感变量置空）
const canExport = computed(() => {
  const s = d.value?.session
  if (!s || !s.placement) return false
  try {
    const pl = JSON.parse(s.placement)
    return pl && Object.keys(pl.host || {}).length > 0
  } catch { return false }
})

function exportOrch() {
  const s = d.value.session
  downloadWithToken(`/api/sessions/${s.id}/export`, `${s.app}-${s.version}-orchestration.json`)
}

async function rollback() {
  const s = d.value.session
  try {
    await ElMessageBox.confirm(
      '回退 = 用本会话的部署包 + 分配 + 变量重放一次增量安装（差异比对自动整组重建旧镜像/旧编排）。库结构与数据不会回滚（账本维持新版结构，降级介质可能不兼容）——应急手段，恢复后请尽快重新升级。',
      `回退到 #${s.id}（${s.app} ${s.version}）`,
      { confirmButtonText: '开始回退', cancelButtonText: '取消', type: 'warning' }
    )
  } catch { return }
  try {
    const res = await api.post('/api/sessions', {
      packageId: s.packageId,
      placement: JSON.parse(s.placement || '{}'),
      mode: 'incremental'
    })
    ElMessage.success(`回退会话 #${res.id} 已启动`)
    activeSessionId = res.id
    detailTitle.value = `#${res.id} ${s.app} ${s.version}（回退）`
    await refreshDetail()
  } catch (e) {
    ElMessage.error('回退失败：' + e.message + '（包已删除时请先重传该版本部署包）')
  }
}

async function load() {
  loading.value = true
  try {
    const res = await api.get('/api/sessions')
    sessions.value = res.sessions || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function openDetail(row) {
  activeSessionId = row.id
  detailTitle.value = '#' + row.id + ' ' + row.app + ' ' + row.version
  await refreshDetail()
  detailOpen.value = true
}

async function refreshDetail() {
  if (!activeSessionId) return
  try {
    d.value = await api.get('/api/sessions/' + activeSessionId)
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function startPolling() {
  stopPolling()
  pollTimer = setInterval(async () => {
    if (detailOpen.value) await refreshDetail()
    if (logsOpen.value) await loadLogs(false)
    if (autoRefresh.value) await load()
  }, 2500)
}
function stopPolling() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
}

async function act(row, action) {
  try {
    await api.post(`/api/sessions/${activeSessionId}/nodes/${encodeURIComponent(row.key)}/${action}`, {})
    ElMessage.success('已提交')
    await refreshDetail()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// 中止安装：剩余节点全部终止（在途命令经上报通道杀进程组），已起容器保留在主机上——
// 重新发起安装会幂等收敛，无需清理
async function abort() {
  const s = d.value.session
  try {
    await ElMessageBox.confirm(
      '将立即终止全部未完成节点（正在执行的命令会被中断杀进程组；已拉起的容器保留在主机上，重新发起安装可幂等收敛）。确定中止本次安装？',
      `中止安装 #${s.id}（${s.app} ${s.version}）`,
      { confirmButtonText: '中止安装', cancelButtonText: '取消', type: 'warning' }
    )
  } catch { return }
  try {
    await api.post(`/api/sessions/${s.id}/abort`, {})
    ElMessage.success('中止指令已下发')
    await refreshDetail()
  } catch (e) {
    ElMessage.error('中止失败：' + e.message)
  }
}

async function openLogs(row) {
  logNode.value = row.key
  logAfter = 0
  logLines.value = []
  logsOpen.value = true
  await loadLogs(true)
}

async function loadLogs(reset) {
  if (reset) logAfter = 0
  try {
    const res = await api.get(`/api/sessions/${activeSessionId}/logs?after=${logAfter}`)
    const lines = res.lines || []
    if (lines.length) {
      logAfter = lines[lines.length - 1].id
      logLines.value = reset ? lines : logLines.value.concat(lines)
      await nextTick(() => {
        if (logBox.value) logBox.value.scrollTop = logBox.value.scrollHeight
      })
    }
  } catch (e) {
    if (reset) ElMessage.error(e.message)
  }
}

onMounted(async () => {
  await load()
  startPolling()
})
onUnmounted(stopPolling)
</script>

<style scoped>
.page-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.page-head h2 { margin: 0; font-size: 17px; display: inline; }
.row-gap { display: flex; gap: 12px; align-items: center; }
.card { margin-bottom: 14px; }
.banner { margin-bottom: 10px; }
.audit-bar { display: flex; align-items: center; justify-content: space-between; gap: 10px;
  margin-bottom: 10px; padding: 6px 10px; background: var(--el-fill-color-light);
  border-radius: 4px; font-size: 12px; color: var(--el-text-color-secondary); }
.mono { font-family: Consolas, Monaco, monospace; }
.warn-line { font-size: 12px; color: var(--el-color-warning); margin-bottom: 4px; }
.log-box { background: var(--el-fill-color-darker); border-radius: 6px; padding: 10px; height: 100%; overflow: auto; font-family: Consolas, Monaco, monospace; font-size: 12px; }
.log-line { color: var(--el-text-color-regular); line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.log-node { color: var(--el-color-primary); margin-right: 4px; }
:deep(.row-failed) { background: var(--el-color-danger-light-9); }
.dur { font-size: 11px; color: var(--el-text-color-secondary); margin-top: 2px; }
</style>
