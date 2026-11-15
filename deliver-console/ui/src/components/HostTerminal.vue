<template>
  <el-drawer :model-value="open" @update:model-value="$emit('update:open', $event)" size="78%"
             :title="'主机终端：' + (host?.name || '')" @opened="focusInput">
    <div class="term-layout">
      <!-- 左：快捷命令 -->
      <div class="side">
        <div class="side-head">快捷命令</div>
        <div class="side-list">
          <el-tooltip v-for="p in presetList" :key="p.id" :content="p.script" placement="right" :show-after="300">
            <div class="side-item" @click="submitText(p.script)">
              <span class="side-name">{{ p.name }}</span>
              <el-icon v-if="p.id.startsWith('c')" class="side-del" title="删除" @click.stop="delPreset(p)"><Close /></el-icon>
            </div>
          </el-tooltip>
          <div v-if="!presetList.length" class="side-empty">暂无适用快捷命令</div>
        </div>
        <div class="side-foot">
          <el-button size="small" :disabled="!draft.trim()" @click="savePreset">保存输入为快捷</el-button>
        </div>
      </div>

      <!-- 右：终端 -->
      <div class="term">
        <div class="term-bar">
          <span class="host-chip">
            {{ host?.name }} · {{ host?.info?.os }}/{{ host?.info?.arch }}
            <el-tag size="small" :type="host?.online ? 'success' : 'info'" effect="dark">{{ host?.online ? '在线' : '离线' }}</el-tag>
            <el-tag v-if="sid" size="small" effect="plain">会话 #{{ sid }}</el-tag>
            <el-tag v-if="cwd" size="small" type="success" effect="plain" class="cwd-tag">{{ cwd }}</el-tag>
            <el-tooltip v-if="staleAgent" placement="bottom"
                        content="agent 版本低于 console 内嵌版本：远程中断与实时心跳不可用（命令会跑到超时才结束）——建议先在主机池升级 agent">
              <el-tag size="small" type="warning" effect="plain">旧版 agent</el-tag>
            </el-tooltip>
            <el-tag v-if="closed" size="small" type="info" effect="dark">已结束</el-tag>
          </span>
          <div class="bar-actions">
            <el-button v-if="running" size="small" type="danger" plain @click="stop">停止（Ctrl+C）</el-button>
            <el-button v-if="cmds.length" link size="small" @click="clearScreen">清屏</el-button>
          </div>
        </div>

        <div ref="outBox" class="term-out" @scroll="onScroll">
          <div v-if="!cmds.length" class="term-empty">
            输入命令回车执行，或点击左侧快捷命令（点击即执行）——Linux 走 sh -c、Windows 走 PowerShell；<br>
            一次执行一条（执行中输入将排队依次执行）；整个终端窗口记为一条执行记录，关闭即收尾并中断在跑命令。
          </div>
          <div v-for="c in cmds" :key="c.key" class="cmd-block">
            <div class="cmd-line"><span class="prompt">$</span> {{ c.text }}</div>
            <div v-for="l in c.lines" :key="l.id" class="out-line">{{ l.line }}</div>
            <div v-if="c.error" class="st-line err">✗ 未执行：{{ c.error }}</div>
            <div v-else class="st-line" :class="{ err: c.status === 'failed' }">
              <template v-if="c.status === 'ok'">✓ 完成<template v-if="durOf(c)"> · {{ durOf(c) }}</template></template>
              <template v-else-if="c.status === 'failed' && c.interrupted">✋ 已中断<template v-if="durOf(c)"> · {{ durOf(c) }}</template></template>
              <template v-else-if="c.status === 'failed'">✗ 失败<template v-if="durOf(c)"> · {{ durOf(c) }}</template><template v-if="c.message"> —— {{ c.message }}</template></template>
              <template v-else-if="c.status === 'running'">▸ 执行中…</template>
              <template v-else-if="c.status === 'dispatched'">▸ 排队中（等待 agent 领取）…</template>
              <template v-else>▸ 等待调度…</template>
            </div>
          </div>
        </div>

        <div v-if="pending.length" class="pending-line">
          <span class="pending-lbl">排队（{{ pending.length }}）：</span>
          <el-tag v-for="(p, i) in pending" :key="i" size="small" effect="plain" closable class="pending-tag"
                  @close="pending.splice(i, 1)">{{ firstLine(p) }}</el-tag>
        </div>

        <div class="term-input">
          <span class="prompt">$</span>
          <el-input ref="draftInput" v-model="draft" type="textarea" :autosize="{ minRows: 1, maxRows: 5 }"
                    resize="none" :placeholder="running ? '执行中——输入将排队；点右侧「停止」或按 Ctrl+C 中断当前命令' : '输入命令回车执行（Shift+Enter 换行多行脚本）'"
                    @keydown.enter.exact.prevent="submitDraft"
                    @keydown.ctrl.c.prevent="ctrlC" />
          <el-button v-if="running" type="danger" class="stop-btn" @click="stop">■ 停止</el-button>
        </div>
      </div>
    </div>
  </el-drawer>
</template>

<script setup>
import { ref, reactive, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Close } from '@element-plus/icons-vue'
import { api } from '../api'

const props = defineProps({
  open: Boolean,
  host: { type: Object, default: null },
  agentStamp: { type: String, default: '' }
})
defineEmits(['update:open'])

const draft = ref('')
const draftInput = ref(null)
const presets = reactive({ builtin: [], custom: [] })
const cmds = ref([]) // {key, text, status, message, startedAt, finishedAt, error, interrupted, lines[], after}
const pending = ref([]) // 执行中输入的排队命令（前端 FIFO，前一条终态自动发下一条）
const sid = ref(0)
const closed = ref(false)
const cwd = ref('') // 会话记忆的工作目录（cd 跨命令生效；state 轮询同步）
const outBox = ref(null)
let pollTimer = null
let stickBottom = true

const presetList = computed(() => {
  const os = props.host?.info?.os || 'linux'
  return [...presets.builtin, ...presets.custom].filter(p => !p.os || p.os === os)
})

// 当前在跑的命令（running/dispatched/pending）——同一时刻至多一条（真 shell 串行语义）
const activeCmd = computed(() => cmds.value.find(c => !c.error &&
  (c.status === 'running' || c.status === 'dispatched' || c.status === 'pending')) || null)
const running = computed(() => !!activeCmd.value && !closed.value)

// 旧 agent 不解析 report 响应 cancel 字段、无周期心跳——终端可跑但中断不生效
const staleAgent = computed(() => {
  if (!props.agentStamp || !props.host || props.host.local) return false
  return props.host.info?.agentVersion !== props.agentStamp
})

watch(() => props.open, v => {
  if (v) {
    openTerminal()
  } else {
    closeTerminal()
  }
})
watch(() => props.host?.id, () => {
  // 切换主机：结束当前终端会话并重置（防串台）
  if (sid.value) closeTerminal()
  resetLocal()
})

function resetLocal() {
  cmds.value = []
  pending.value = []
  closed.value = false
  cwd.value = ''
  stickBottom = true
  stopPoll()
}

// openTerminal 开抽屉=开终端会话（一个窗口一条执行记录）
async function openTerminal() {
  resetLocal()
  loadPresets()
  if (!props.host) return
  try {
    const res = await api.post('/api/terminal/open', { hostId: props.host.id })
    sid.value = res.id
    startPoll()
  } catch (e) {
    ElMessage.error(e.message)
  }
  nextTick(focusInput)
}

// closeTerminal 关抽屉=收尾会话（后端中断在跑命令、未终态落 failed、会话终态留档）
function closeTerminal() {
  stopPoll()
  if (sid.value) {
    api.post(`/api/terminal/${sid.value}/close`, {}).catch(() => { /* 尽力而为；心跳超时由后端清扫兜底 */ })
  }
  sid.value = 0
}

function focusInput() {
  draftInput.value?.focus?.()
}

async function loadPresets() {
  try {
    const res = await api.get('/api/exec-presets')
    presets.builtin = res.builtin || []
    presets.custom = res.custom || []
  } catch { /* 快捷脚本加载失败不阻塞手输执行 */ }
}

async function delPreset(p) {
  try {
    await api.del('/api/exec-presets/' + encodeURIComponent(p.id))
    ElMessage.success('已删除快捷命令「' + p.name + '」')
    await loadPresets()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function savePreset() {
  let name
  try {
    const { value } = await ElMessageBox.prompt('为当前输入的脚本起个名字（保存在 console，所有浏览器可用）', '保存快捷命令',
      { inputPattern: /\S+/, inputErrorMessage: '名称不能为空' })
    name = value.trim()
  } catch { return }
  try {
    await api.post('/api/exec-presets', { name, os: props.host?.info?.os || '', script: draft.value })
    ElMessage.success('已保存为快捷命令')
    await loadPresets()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function submitDraft() {
  const text = draft.value.trim()
  if (!text) return
  draft.value = ''
  submitText(text)
}

// submitText 真 shell 语义：执行中进入前端排队，前一条终态自动依次执行
function submitText(text) {
  if (closed.value) {
    ElMessage.warning('终端已结束——重新打开主机行「执行」开新终端')
    return
  }
  if (running.value) {
    pending.value.push(text)
    return
  }
  dispatch(text)
}

// dispatch 下发一条命令（追加 cmd-N 节点并派发）
async function dispatch(text) {
  if (!sid.value) return
  const c = { key: '', text, status: 'pending', message: '', startedAt: 0, finishedAt: 0,
    error: '', interrupted: false, logCount: 0, lines: [], after: 0 }
  cmds.value.push(c)
  const rc = cmds.value[cmds.value.length - 1] // 响应式代理：字段变更即时触发渲染
  stickBottom = true
  scrollBottom()
  try {
    const res = await api.post(`/api/terminal/${sid.value}/run`, { script: text })
    rc.key = res.node
    await pollOnce()
  } catch (e) {
    rc.error = e.message
  }
}

// stop 中断当前命令并清空排队（真 shell 的 Ctrl+C）；旧 agent 无法远程中断时给出明确指引
async function stop() {
  const c = activeCmd.value
  pending.value = []
  if (!c || !c.key) return
  try {
    const res = await api.post(`/api/terminal/${sid.value}/stop`, { node: c.key })
    if (res.stale) {
      ElMessage({ type: 'warning', duration: 8000, showClose: true,
        message: res.advice || '该命令由旧版 agent 执行，无法远程中断——请先在主机池升级 agent（需当前命令结束后生效）；急停可到目标机执行 systemctl restart deliver-agent' })
    } else {
      ElMessage.success('停止指令已下发，命令将在数秒内中断')
    }
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function ctrlC() {
  if (!draft.value.trim() && running.value) stop()
}

function startPoll() {
  stopPoll()
  pollTimer = setInterval(pollOnce, 1500)
}
function stopPoll() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
}

// pollOnce 刷终端状态：会话/节点状态 + 当前命令增量日志；会话收尾即停；前一条终态自动发排队命令
async function pollOnce() {
  if (!sid.value) return
  let state
  try {
    state = await api.get(`/api/terminal/${sid.value}/state`)
  } catch { return /* 下轮重试 */ }
  const nodes = state.nodes || []
  cwd.value = state.cwd || cwd.value
  for (const c of cmds.value) {
    if (!c.key || c.error) continue
    const n = nodes.find(x => x.key === c.key)
    if (n) {
      c.status = n.status
      c.message = n.message
      c.startedAt = n.startedAt
      c.finishedAt = n.finishedAt
      c.logCount = n.logCount
      c.interrupted = n.status === 'failed' && (n.message || '').includes('人工中断')
    }
  }
  if (state.closed || state.session?.status !== 'running') {
    // 会话收尾（关闭/僵尸清扫/console 重启）：先补齐未显示日志再停轮询——最后一批输出
    // 几乎全部随终态上报落库，不补拉就「✓ 完成」却无输出
    for (const c of cmds.value) {
      if (c.key && !c.error && (c.logCount || 0) > c.lines.length) await loadCmdLogs(c)
    }
    closed.value = true
    stopPoll()
    return
  }
  const cur = activeCmd.value
  // 日志补拉以 logCount 为准（服务端权威行数）：显示行数落后就追——快速命令输出几乎全部随
  // 终态上报一次性落库、状态先翻 ok，靠「活跃才拉」或空页启发式都会漏
  const targets = cmds.value.filter(c => c.key && !c.error &&
    (c === cur || (c.logCount || 0) > c.lines.length))
  for (const c of targets) await loadCmdLogs(c)
  // 前一条终态且仍有排队 → 自动发下一条
  if (!cur && pending.value.length && !closed.value) {
    dispatch(pending.value.shift())
  }
}

async function loadCmdLogs(c) {
  try {
    const res = await api.get(`/api/sessions/${sid.value}/logs?node=${encodeURIComponent(c.key)}&after=${c.after}`)
    const lines = res.lines || []
    if (lines.length) {
      c.after = lines[lines.length - 1].id
      c.lines.push(...lines)
      if (stickBottom) scrollBottom()
    }
  } catch { /* 静默，下轮重试 */ }
}

function durOf(c) {
  if (!c.startedAt || !c.finishedAt) return ''
  const s = Math.max(0, c.finishedAt - c.startedAt)
  return s >= 60 ? Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒' : s + ' 秒'
}

function firstLine(text) {
  return (text.split('\n').map(t => t.trim()).find(t => t) || '').slice(0, 40)
}

function onScroll() {
  const el = outBox.value
  if (!el) return
  stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

function scrollBottom() {
  nextTick(() => {
    if (outBox.value) outBox.value.scrollTop = outBox.value.scrollHeight
  })
}

// clearScreen 清屏=结束当前终端并开新终端（显示与节点绑定，重开保一对一口径）
async function clearScreen() {
  closeTerminal()
  resetLocal()
  await openTerminal()
  nextTick(focusInput)
}

// 后台标签/被遮挡窗口的定时器会被浏览器节流（可达 1 次/分钟），拉开心跳与日志轮询间隔——
// 恢复可见/聚焦时立即补一轮，输出即时追上（配合服务端 90s 清扫阈值，不再误判僵尸）
function onResume() {
  if (props.open && sid.value && !closed.value && !pollTimer) {
    startPoll()
    pollOnce()
  }
}

onMounted(() => {
  document.addEventListener('visibilitychange', onResume)
  window.addEventListener('focus', onResume)
})
onUnmounted(() => {
  document.removeEventListener('visibilitychange', onResume)
  window.removeEventListener('focus', onResume)
  closeTerminal()
})
</script>

<style scoped>
.term-layout { display: flex; gap: 10px; height: 100%; }
.side { width: 200px; flex-shrink: 0; display: flex; flex-direction: column; border: 1px solid var(--el-border-color-lighter); border-radius: 6px; overflow: hidden; }
.side-head { padding: 6px 10px; font-size: 12px; color: var(--el-text-color-secondary); border-bottom: 1px solid var(--el-border-color-lighter); }
.side-list { flex: 1; overflow: auto; padding: 4px; }
.side-item { display: flex; align-items: center; justify-content: space-between; gap: 4px; padding: 5px 8px;
  border-radius: 4px; font-size: 12px; cursor: pointer; }
.side-item:hover { background: var(--el-fill-color-light); }
.side-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.side-del { flex-shrink: 0; color: var(--el-text-color-placeholder); font-size: 12px; }
.side-del:hover { color: var(--el-color-danger); }
.side-empty { padding: 10px 8px; font-size: 11px; color: var(--el-text-color-placeholder); }
.side-foot { padding: 8px; border-top: 1px solid var(--el-border-color-lighter); }
.side-foot .el-button { width: 100%; }

.term { flex: 1; display: flex; flex-direction: column; border: 1px solid var(--el-border-color-lighter); border-radius: 6px; overflow: hidden; min-width: 0; }
.term-bar { display: flex; align-items: center; justify-content: space-between; padding: 6px 10px;
  border-bottom: 1px solid var(--el-border-color-lighter); }
.host-chip { font-size: 12px; color: var(--el-text-color-secondary); display: inline-flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.cwd-tag { max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bar-actions { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.term-out { flex: 1; overflow: auto; padding: 10px 12px; background: var(--el-fill-color-darker);
  font-family: Consolas, Monaco, monospace; font-size: 12px; }
.term-empty { color: var(--el-text-color-placeholder); line-height: 2; }
.cmd-block { margin-bottom: 10px; }
.cmd-line { color: var(--el-color-success); font-weight: 600; white-space: pre-wrap; word-break: break-all; }
.out-line { color: var(--el-text-color-regular); line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.st-line { color: var(--el-text-color-secondary); margin-top: 2px; }
.st-line.err { color: var(--el-color-danger); }
.pending-line { display: flex; align-items: center; flex-wrap: wrap; gap: 4px; padding: 4px 10px;
  border-top: 1px dashed var(--el-border-color-lighter); font-size: 11px; }
.pending-lbl { color: var(--el-text-color-placeholder); }
.pending-tag { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.term-input { display: flex; align-items: flex-end; gap: 8px; padding: 8px 10px; border-top: 1px solid var(--el-border-color-lighter); }
.prompt { color: var(--el-color-success); font-family: Consolas, Monaco, monospace; font-size: 13px; font-weight: 700; line-height: 24px; }
.term-input :deep(.el-textarea__inner) { font-family: Consolas, Monaco, monospace; }
.stop-btn { flex-shrink: 0; font-weight: 600; }
</style>
