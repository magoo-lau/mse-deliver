<template>
  <el-drawer :model-value="open" @update:model-value="$emit('update:open', $event)" size="62%"
             :title="'执行记录：' + (host?.name || '')">
    <!-- row-key：2.5s 轮询整体替换数据时按 id 保留展开态（否则展开行会被收起） -->
    <el-table :data="sessions" size="small" v-loading="loading" row-key="id">
      <el-table-column type="expand">
        <template #default="{ row }">
          <div class="node-list">
            <div v-if="!row.nodes?.length" class="node-empty">（本主机无节点）</div>
            <div v-for="n in row.nodes" :key="n.key" class="node-item"
                 :class="{ active: activeNode && activeNode.sessionId === row.id && activeNode.key === n.key }"
                 @click="openNode(row, n)">
              <el-tag size="small" :type="statusType(n.status)" effect="dark">{{ statusText(n.status) }}</el-tag>
              <span class="node-kind">{{ kindLabel(n.kind) }}</span>
              <span class="node-title">{{ n.title }}</span>
              <span class="node-logs">{{ n.logCount }} 行日志</span>
            </div>
          </div>
        </template>
      </el-table-column>
      <el-table-column prop="id" label="#" width="60" />
      <el-table-column label="应用" min-width="130">
        <template #default="{ row }">{{ row.app }}<span v-if="row.version" class="ver"> {{ row.version }}</span></template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="statusType(row.status)" effect="dark">{{ statusText(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="summary" label="摘要" min-width="170" show-overflow-tooltip />
      <el-table-column label="开始时间" width="150">
        <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
      </el-table-column>
    </el-table>

    <div v-if="activeNode" class="log-wrap">
      <div class="log-head">
        <span>日志：#{{ activeNode.sessionId }} / {{ activeNode.key }}<span v-if="activeNode.title">（{{ activeNode.title }}）</span></span>
        <el-button link size="small" @click="closeLogs">收起</el-button>
      </div>
      <div ref="logBox" class="log-box">
        <div v-if="!logLines.length" class="log-empty">暂无日志</div>
        <div v-for="l in logLines" :key="l.id" class="log-line"><span class="log-node">[{{ l.node }}]</span> {{ l.line }}</div>
      </div>
    </div>
  </el-drawer>
</template>

<script setup>
import { ref, watch, nextTick, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import { api, fmtTime } from '../api'

const props = defineProps({ open: Boolean, host: { type: Object, default: null } })
defineEmits(['update:open'])

const sessions = ref([])
const loading = ref(false)
const activeNode = ref(null) // {sessionId, key, title}
const logLines = ref([])
const logBox = ref(null)
let pollTimer = null
let logAfter = 0

function statusType(st) {
  return { done: 'success', ok: 'success', running: 'primary', dispatched: 'primary',
    pending: 'info', failed: 'danger', skipped: 'warning' }[st] || 'info'
}
function statusText(st) {
  return { done: '完成', ok: '成功', running: '执行中', dispatched: '已派发', pending: '待执行',
    failed: '失败', skipped: '已跳过' }[st] || st
}
function kindLabel(kind) {
  return { docker: '装 Docker', load: '装载', compose: '编排', systemd: '裸机', cmd: '命令',
    wait: '等待', gate: '门禁', plugin: '插件', toolkit: '装 compose' }[kind] || kind
}

watch(() => props.open, v => {
  if (v) {
    activeNode.value = null
    logLines.value = []
    load()
    startPoll()
  } else {
    stopPoll()
  }
})

async function load(silent) {
  if (!silent) loading.value = true
  try {
    const res = await api.get(`/api/hosts/${props.host.id}/sessions`)
    sessions.value = res.sessions || []
  } catch (e) {
    if (!silent) ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function startPoll() {
  stopPoll()
  pollTimer = setInterval(async () => {
    await load(true)
    if (activeNode.value) await loadLogs(false)
    // 全部终态即停轮询（重开抽屉恢复刷新）
    if (!sessions.value.some(s => s.status === 'running')) stopPoll()
  }, 2500)
}
function stopPoll() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
}

function openNode(row, n) {
  if (activeNode.value && activeNode.value.sessionId === row.id && activeNode.value.key === n.key) return
  activeNode.value = { sessionId: row.id, key: n.key, title: n.title }
  logAfter = 0
  logLines.value = []
  loadLogs(true)
}

function closeLogs() {
  activeNode.value = null
  logLines.value = []
}

async function loadLogs(reset) {
  if (!activeNode.value) return
  if (reset) logAfter = 0
  try {
    const { sessionId, key } = activeNode.value
    const res = await api.get(`/api/sessions/${sessionId}/logs?node=${encodeURIComponent(key)}&after=${logAfter}`)
    const lines = res.lines || []
    if (lines.length) {
      logAfter = lines[lines.length - 1].id
      logLines.value = reset ? lines : logLines.value.concat(lines)
      await nextTick(() => {
        if (logBox.value) logBox.value.scrollTop = logBox.value.scrollHeight
      })
    }
  } catch { /* 静默，下轮重试 */ }
}

onUnmounted(stopPoll)
</script>

<style scoped>
.ver { color: var(--el-text-color-secondary); font-size: 11px; }
.node-list { padding: 4px 10px; display: flex; flex-direction: column; gap: 4px; }
.node-empty { font-size: 11px; color: var(--el-text-color-placeholder); }
.node-item { display: flex; align-items: center; gap: 8px; padding: 4px 8px; border-radius: 4px;
  cursor: pointer; font-size: 12px; }
.node-item:hover { background: var(--el-fill-color-light); }
.node-item.active { background: var(--el-fill-color); }
.node-kind { color: var(--el-text-color-secondary); flex-shrink: 0; }
.node-title { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.node-logs { color: var(--el-text-color-placeholder); font-size: 11px; flex-shrink: 0; }
.log-wrap { margin-top: 12px; }
.log-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 6px;
  font-size: 12px; color: var(--el-text-color-secondary); }
.log-box { background: var(--el-fill-color-darker); border-radius: 6px; padding: 10px; height: 300px;
  overflow: auto; font-family: Consolas, Monaco, monospace; font-size: 12px; }
.log-line { color: var(--el-text-color-regular); line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.log-node { color: var(--el-color-primary); margin-right: 4px; }
.log-empty { color: var(--el-text-color-placeholder); }
</style>
