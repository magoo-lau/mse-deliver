<template>
  <el-dialog :model-value="open" @update:model-value="$emit('update:open', $event)"
             :title="dlgTitle" width="680px" :close-on-click-modal="false" append-to-body>
    <!-- 表单模式 -->
    <template v-if="mode === 'form'">
      <div class="line">
        <span class="lbl">目标主机</span>
        <el-tag v-for="h in hosts" :key="h.id" size="small" effect="plain" class="host-tag">
          {{ h.name }}（{{ h.info?.os || '?' }}）
        </el-tag>
      </div>
      <div class="line">
        <span class="lbl">快捷脚本</span>
        <el-tooltip v-for="p in presetList" :key="p.id" :content="p.script" placement="top">
          <el-tag size="small" type="primary" effect="plain" class="preset-tag"
                  :closable="p.id.startsWith('c')" @click="applyPreset(p)" @close="delPreset(p)">{{ p.name }}</el-tag>
        </el-tooltip>
        <span v-if="!presetList.length" class="sub">（所选主机平台暂无适用快捷脚本）</span>
      </div>
      <el-input v-model="script" type="textarea" :rows="7"
                placeholder="Linux 走 sh -c、Windows 走 PowerShell——点击上方快捷脚本可回填后修改" />
      <div class="line opt">
        <span class="lbl">超时（秒）</span>
        <el-input-number v-model="timeoutSec" :min="30" :max="3600" :step="30" />
        <span class="lbl remark-lbl">备注</span>
        <el-input v-model="remark" style="width: 250px" placeholder="留空取脚本首行" maxlength="60" />
      </div>
    </template>

    <!-- 输出模式：逐主机会话状态 + 点选看流式输出 -->
    <template v-else>
      <el-table :data="results" size="small" highlight-current-row @row-click="selectHost">
        <el-table-column prop="hostName" label="主机" min-width="150" />
        <el-table-column label="会话" width="70">
          <template #default="{ row }">{{ row.sessionId ? '#' + row.sessionId : '—' }}</template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.error" size="small" type="danger" effect="dark">未执行</el-tag>
            <el-tag v-else size="small" :type="statusType(row.status)" effect="dark">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="error" label="失败原因" min-width="180" show-overflow-tooltip />
      </el-table>
      <div ref="logBox" class="log-box">
        <div v-if="!activeSessionId" class="log-empty">点击上方主机行查看该主机执行输出（执行中自动追加）</div>
        <div v-for="l in logLines" :key="l.id" class="log-line"><span class="log-node">[{{ l.node }}]</span> {{ l.line }}</div>
      </div>
    </template>

    <template #footer>
      <template v-if="mode === 'form'">
        <el-button :disabled="!script.trim()" @click="savePreset">存为快捷</el-button>
        <el-button @click="$emit('update:open', false)">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">执行</el-button>
      </template>
      <template v-else>
        <el-button @click="backToForm">再次执行</el-button>
        <el-button type="primary" @click="$emit('update:open', false)">关闭</el-button>
      </template>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, computed, watch, nextTick, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'

const props = defineProps({ open: Boolean, hosts: { type: Array, default: () => [] } })
defineEmits(['update:open'])

const mode = ref('form') // form=编辑脚本 | output=执行结果
const script = ref('')
const remark = ref('')
const timeoutSec = ref(600)
const submitting = ref(false)
const presets = reactive({ builtin: [], custom: [] })
const results = ref([]) // [{hostId, hostName, sessionId, error, status}]
const activeHostId = ref(0)
const logLines = ref([])
const logBox = ref(null)
let pollTimer = null
let logAfter = 0

const dlgTitle = computed(() => props.hosts.length > 1
  ? `批量执行命令（${props.hosts.length} 台）`
  : `执行命令（${props.hosts[0]?.name || ''}）`)

// 快捷脚本按目标主机 OS 过滤：混选平台只留全平台项
const osSet = computed(() => new Set(props.hosts.map(h => h.info?.os || 'linux')))
const presetList = computed(() => {
  const all = [...presets.builtin, ...presets.custom]
  if (osSet.value.size > 1) return all.filter(p => !p.os)
  const os = [...osSet.value][0] || 'linux'
  return all.filter(p => !p.os || p.os === os)
})

const activeSessionId = computed(() => {
  const it = results.value.find(r => r.hostId === activeHostId.value)
  return it?.sessionId || 0
})

function statusType(st) {
  return { done: 'success', running: 'primary', failed: 'danger' }[st] || 'info'
}
function statusText(st) {
  return { done: '完成', running: '执行中', failed: '失败' }[st] || st
}

watch(() => props.open, v => {
  if (v) {
    mode.value = 'form' // 脚本内容保留（连续巡检方便）；结果态复位
    stopPoll()
    loadPresets()
  } else {
    stopPoll()
  }
})

async function loadPresets() {
  try {
    const res = await api.get('/api/exec-presets')
    presets.builtin = res.builtin || []
    presets.custom = res.custom || []
  } catch { /* 快捷脚本加载失败不阻塞手写执行 */ }
}

function applyPreset(p) {
  script.value = p.script
}

async function delPreset(p) {
  try {
    await api.del('/api/exec-presets/' + encodeURIComponent(p.id))
    ElMessage.success('已删除快捷脚本「' + p.name + '」')
    await loadPresets()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function savePreset() {
  let name
  try {
    const { value } = await ElMessageBox.prompt('为当前脚本起个名字（保存在 console，所有浏览器可用）', '存为快捷脚本',
      { inputValue: remark.value || '', inputPattern: /\S+/, inputErrorMessage: '名称不能为空' })
    name = value.trim()
  } catch { return }
  try {
    const os = osSet.value.size > 1 ? '' : ([...osSet.value][0] || '')
    await api.post('/api/exec-presets', { name, os, script: script.value })
    ElMessage.success('已保存为快捷脚本')
    await loadPresets()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function submit() {
  if (!script.value.trim()) {
    ElMessage.warning('请输入脚本内容')
    return
  }
  submitting.value = true
  try {
    const res = await api.post('/api/exec', {
      hostIds: props.hosts.map(h => h.id),
      script: script.value,
      timeoutSec: timeoutSec.value,
      title: remark.value.trim()
    })
    results.value = (res.items || []).map(it => ({ ...it, status: it.sessionId ? 'running' : 'failed' }))
    const first = results.value.find(it => it.sessionId)
    activeHostId.value = first ? first.hostId : 0
    logAfter = 0
    logLines.value = []
    mode.value = 'output'
    if (!first) return
    await pollOnce()
    startPoll()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    submitting.value = false
  }
}

function backToForm() {
  stopPoll()
  mode.value = 'form'
}

function startPoll() {
  stopPoll()
  pollTimer = setInterval(pollOnce, 2500)
}
function stopPoll() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
}

async function pollOnce() {
  for (const it of results.value) {
    if (!it.sessionId) continue
    try {
      const d = await api.get('/api/sessions/' + it.sessionId)
      it.status = d.session.status
    } catch { /* 下轮再刷 */ }
  }
  await loadLogs(false)
  if (results.value.every(it => !it.sessionId || it.status === 'done' || it.status === 'failed')) stopPoll()
}

function selectHost(row) {
  if (!row.sessionId || row.hostId === activeHostId.value) return
  activeHostId.value = row.hostId
  logAfter = 0
  logLines.value = []
  loadLogs(true)
}

async function loadLogs(reset) {
  if (!activeSessionId.value) return
  if (reset) logAfter = 0
  try {
    const res = await api.get(`/api/sessions/${activeSessionId.value}/logs?after=${logAfter}`)
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
.line { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.lbl { font-size: 12px; color: var(--el-text-color-secondary); flex-shrink: 0; }
.opt { margin-top: 10px; }
.remark-lbl { margin-left: 14px; }
.host-tag { margin-right: 2px; }
.preset-tag { cursor: pointer; margin-right: 2px; }
.sub { font-size: 11px; color: var(--el-text-color-placeholder); }
.log-box { margin-top: 10px; background: var(--el-fill-color-darker); border-radius: 6px; padding: 10px;
  height: 280px; overflow: auto; font-family: Consolas, Monaco, monospace; font-size: 12px; }
.log-line { color: var(--el-text-color-regular); line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.log-node { color: var(--el-color-primary); margin-right: 4px; }
.log-empty { color: var(--el-text-color-placeholder); }
</style>
