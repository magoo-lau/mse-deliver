<template>
  <div class="page">
    <div class="page-head">
      <h2>主机池</h2>
      <span class="hint">目标机只需拷入 agent 二进制执行 install；此后 agent 出站长轮询，无需直登——行内「执行」打开主机终端（左侧快捷命令 + 右侧命令行实时交互，勾选多台可批量执行）、「日志」看该主机执行历史；Docker/Compose 安装介质在「安装源」页维护</span>
    </div>

    <el-card shadow="never" class="card">
      <template #header><b>接入新主机</b></template>
      <el-form label-width="70px" inline>
        <el-form-item label="引导令牌">
          <el-input :model-value="enrollToken" readonly style="width: 260px">
            <template #append>
              <el-button @click="copyText(enrollToken)">复制</el-button>
            </template>
          </el-input>
          <el-button type="warning" plain @click="rotate">轮换令牌</el-button>
        </el-form-item>
      </el-form>
      <div class="steps">
        <div class="step"><b>① 下载 agent</b>（传输到目标机）：
          <el-link type="primary" @click="dl('linux')">deliver-agent-linux</el-link>
          <el-link type="primary" @click="dl('windows.exe')" style="margin-left: 8px">deliver-agent-windows.exe</el-link>
        </div>
        <div class="step"><b>② 目标机执行</b>（root，注册并自装系统服务）：
          <code class="code">chmod +x deliver-agent-linux &amp;&amp; ./deliver-agent-linux install -server {{ serverAddr }} -token {{ enrollToken || '<引导令牌>' }}</code>
        </div>
        <div class="step"><b>③ 回本页刷新</b>——主机出现即在线（能力档案自动上报：docker/compose/systemd/java/资源）。</div>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header>
        <div class="row-between">
          <b>主机（{{ hosts.length }}）</b>
          <div class="head-actions">
            <span class="auto-up-label">自动升级旧版 agent</span>
            <el-switch v-model="autoUpgrade" @change="saveAutoUpgrade" />
            <el-button type="primary" plain :disabled="!selRows.length" @click="openBatchExec">
              批量执行{{ selRows.length ? `（${selRows.length}）` : '' }}
            </el-button>
            <el-button @click="load()" :loading="loading">刷新</el-button>
          </div>
        </div>
      </template>
      <el-table :data="hosts" size="small" @selection-change="onSelChange">
        <el-table-column type="selection" width="40" :selectable="row => !row.revoked" />
        <el-table-column label="主机" min-width="170">
          <template #default="{ row }">
            <div class="host-name">
              <span>{{ row.name }}</span>
              <el-tag v-if="row.local" size="small" type="info">本机节点</el-tag>
              <el-tag v-if="row.revoked" size="small" type="danger">已吊销</el-tag>
            </div>
            <div class="host-addr">{{ row.addr || '-' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="80">
          <template #default="{ row }">
            <el-tag :type="row.online ? 'success' : 'info'" size="small" effect="dark">{{ row.online ? '在线' : '离线' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="能力" min-width="260">
          <template #default="{ row }">
            <div class="caps">
              <el-tag size="small" :type="row.info.docker ? 'success' : 'info'" effect="plain">docker{{ row.info.dockerVersion ? ' ' + row.info.dockerVersion : '' }}</el-tag>
              <el-tag v-if="row.info.docker" size="small" :type="row.info.compose ? 'success' : 'info'" effect="plain">compose{{ row.info.composeVersion ? ' ' + row.info.composeVersion : '' }}</el-tag>
              <el-tooltip v-if="row.info.os !== 'linux' && !row.info.docker" placement="top"
                          content="Windows 暂不支持自动安装 Docker/docker-compose——请人工预装 Docker Desktop（桌面版自带 compose 插件）并确保守护进程运行">
                <el-tag size="small" type="warning" effect="plain">需预装 Docker Desktop</el-tag>
              </el-tooltip>
              <el-tag size="small" :type="row.info.systemd ? 'success' : 'info'" effect="plain">systemd</el-tag>
              <el-tag v-if="row.info.javaVersion" size="small" type="success" effect="plain">java {{ row.info.javaVersion }}</el-tag>
              <el-tag size="small" effect="plain">{{ row.info.os }}/{{ row.info.arch }}</el-tag>
            </div>
            <div class="host-addr">{{ row.info.cpuCores }} 核 · {{ memOf(row) }} · 剩余 {{ row.info.diskFreeGB || '?' }}G</div>
            <div v-if="heldPorts(row).length" class="host-ports">
              <el-tooltip v-for="c in heldPorts(row)" :key="c.name" placement="top"
                          :content="'端口 ' + (c.hostPorts || []).join('/') + ' ← ' + c.name + (c.image ? '（' + c.image + '）' : '') + (c.project ? ' · 项目 ' + c.project : '')">
                <el-tag size="small" type="info" effect="plain" class="port-tag">{{ (c.hostPorts || []).join('/') }} ← {{ c.name }}</el-tag>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
        <el-table-column min-width="210">
          <template #header>
            <span>agent 版本
              <el-tooltip placement="top"
                          :content="'console 内嵌 agent 版本：' + (agentStamp || '未知（console 未注入版本戳）') + '。点「升级」由 console 远程推送内嵌版本：agent 在任务间隙自升级并重启（≤25 秒生效，主机须在线）。'">
                <el-icon class="ver-help"><QuestionFilled /></el-icon>
              </el-tooltip>
            </span>
          </template>
          <template #default="{ row }">
            <span v-if="row.local">—</span>
            <el-tooltip v-else-if="agentNeedsUpgrade(row)" placement="top"
                        content="与 console 内嵌版本不一致——旧 agent 不识别新任务类型（如安装 Docker/Compose）、介质下载会 400。点「升级」由 console 远程推送：agent 在任务间隙自升级并重启（≤25 秒生效，主机须在线）；极旧 agent（不含自升级能力）不响应升级指令，请从本页重新下载安装一次">
              <span class="ver-cell">
                <el-tag size="small" type="danger" effect="dark">
                  {{ row.info.agentVersion ? row.info.agentVersion + ' ≠ ' + agentStamp : '未知（旧版 agent）' }}
                </el-tag>
                <el-button link type="primary" size="small" @click="upgradeAgent(row)">升级</el-button>
              </span>
            </el-tooltip>
            <span v-else class="ver-cell">
              <el-tag size="small" :type="row.info.agentVersion ? 'success' : 'danger'" effect="plain">
                {{ row.info.agentVersion || '未知（旧版 agent）' }}
              </el-tag>
              <el-button v-if="!row.revoked" link type="primary" size="small" @click="upgradeAgent(row)">升级</el-button>
            </span>
          </template>
        </el-table-column>
        <el-table-column label="访问地址（跨机端点/入口用）" min-width="150">
          <template #default="{ row }">
            <el-input v-model="row.addr" size="small" style="width: 140px" @change="saveAddr(row)" />
          </template>
        </el-table-column>
        <el-table-column label="最近心跳" width="150">
          <template #default="{ row }">{{ row.online ? '刚刚' : fmtTime(row.lastSeen) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="330" fixed="right">
          <template #default="{ row }">
            <el-button v-if="!row.revoked" link type="primary" @click="openTerm(row)">执行</el-button>
            <el-button link @click="openHostLogs(row)">日志</el-button>
            <el-button v-if="!row.revoked && row.info.os === 'linux' && !row.info.docker"
                       link type="success" @click="openInstall(row, 'docker')">装 Docker</el-button>
            <el-button v-if="!row.revoked && row.info.os === 'linux' && row.info.docker"
                       link type="primary" @click="openInstall(row, 'docker')">Docker 换版</el-button>
            <el-button v-if="!row.revoked && row.info.os === 'linux' && row.info.docker && !row.info.compose"
                       link type="success" @click="openInstall(row, 'compose')">装 Compose</el-button>
            <el-button v-if="!row.revoked && row.info.os === 'linux' && row.info.docker && row.info.compose"
                       link type="primary" @click="openInstall(row, 'compose')">Compose 换版</el-button>
            <el-button v-if="!row.local && !row.revoked" link type="danger" @click="revoke(row)">吊销</el-button>
            <el-button v-if="!row.local" link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="dlg.visible" :title="dlg.title" width="480px">
      <el-form label-width="90px">
        <el-form-item label="当前">
          <span>{{ dlg.currentLine }}</span>
        </el-form-item>
        <el-form-item label="目标版本">
          <el-select v-model="dlg.version" style="width: 100%">
            <el-option v-if="dlg.kind === 'docker'" value="" label="在线最新（get 脚本直装）" />
            <el-option v-else value="" :label="'缺省（最新介质' + (cs.version ? ' / 在线 ' + cs.version : '') + '）'" />
            <el-option v-for="v in dlg.versions" :key="v" :value="v" :label="v" />
          </el-select>
          <div v-if="dlg.kind === 'docker'" class="media-tip">指定版本需已上传对应离线介质（在线脚本只能装最新）</div>
        </el-form-item>
        <el-form-item label="强制重装">
          <el-switch v-model="dlg.force" />
          <span class="media-tip" style="margin-left: 8px">已装同版本仍重装</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlg.visible = false">取消</el-button>
        <el-button type="primary" :loading="dlg.submitting" @click="submitInstall">开始安装</el-button>
      </template>
    </el-dialog>

    <HostExecDialog v-model:open="execOpen" :hosts="execHosts" />
    <HostTerminal v-model:open="termOpen" :host="termHost" :agent-stamp="agentStamp" />
    <HostLogDrawer v-model:open="logOpen" :host="logHost" />
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtTime, downloadWithToken } from '../api'
import { gotoView } from '../nav'
import HostExecDialog from '../components/HostExecDialog.vue'
import HostTerminal from '../components/HostTerminal.vue'
import HostLogDrawer from '../components/HostLogDrawer.vue'

const hosts = ref([])
const enrollToken = ref('')
const loading = ref(false)
const serverAddr = ref(location.origin)
const agentStamp = ref('') // console 内嵌 agent 版本戳（版本列比对）
const autoUpgrade = ref(false) // agent 自动升级开关（settings 持久化，缺省关=仅手动按钮）
let refreshTimer = null

// 主机运维：单行「执行」= 终端式实时交互（HostTerminal）、多选「批量执行」= 一次下发弹窗（HostExecDialog）、
// 单行「日志」= 执行历史抽屉（HostLogDrawer）
const selRows = ref([])
const execOpen = ref(false)
const execHosts = ref([])
const termOpen = ref(false)
const termHost = ref(null)
const logOpen = ref(false)
const logHost = ref(null)

function onSelChange(rows) {
  selRows.value = rows
}
function openTerm(row) {
  termHost.value = row
  termOpen.value = true
}
function openBatchExec() {
  if (!selRows.value.length) return
  execHosts.value = selRows.value.slice()
  execOpen.value = true
}
function openHostLogs(row) {
  logHost.value = row
  logOpen.value = true
}

function agentNeedsUpgrade(row) {
  // 仅决定版本列红标警示（版本未知=极旧 agent 未上报视同需升级）；「升级」按钮已常显，点击由后端校验（离线/版本不低于内嵌均明确报错）
  return !!agentStamp.value && !row.local && !row.revoked && row.info.agentVersion !== agentStamp.value
}

// 安装介质清单（静默加载，仅供「装 Docker / 装 Compose」弹窗的版本下拉；管理入口在「安装源」页）
const ds = reactive({ media: {} })
const cs = reactive({ version: '', media: {} })

async function loadDS() {
  try {
    ds.media = (await api.get('/api/docker-source')).media || {}
  } catch { /* 下拉退化缺省项，不阻塞页面 */ }
}

async function loadCS() {
  try {
    const res = await api.get('/api/compose-source')
    cs.version = res.version || ''
    cs.media = res.media || {}
  } catch { /* 同上 */ }
}

// 一键安装/换版弹窗（docker / compose 共用；版本下拉取自该主机平台已传介质）
const dlg = reactive({ visible: false, kind: 'docker', row: null, version: '', force: false, versions: [], title: '', currentLine: '', submitting: false })

function openInstall(row, kind) {
  const slot = (row.info.os || 'linux') + '-' + (row.info.arch || 'amd64')
  const media = kind === 'docker' ? (ds.media || {})[slot] : (cs.media || {})[slot]
  dlg.kind = kind
  dlg.row = row
  dlg.version = ''
  dlg.force = false
  dlg.submitting = false
  dlg.versions = media ? Object.keys(media) : []
  dlg.title = kind === 'docker'
    ? (row.info.docker ? `Docker 换版（${row.name}）` : `安装 Docker（${row.name}）`)
    : (row.info.compose ? `docker compose 换版（${row.name}）` : `安装 docker compose（${row.name}）`)
  dlg.currentLine = kind === 'docker'
    ? (row.info.docker ? `docker ${row.info.dockerVersion || '?'}` : '未安装 docker')
    : (row.info.compose ? `docker compose ${row.info.composeVersion || '?'}` : '未安装 compose 插件')
  dlg.visible = true
}

async function submitInstall() {
  const row = dlg.row
  if (!row) return
  dlg.submitting = true
  const ep = dlg.kind === 'docker' ? 'install-docker' : 'install-compose'
  try {
    const res = await api.post(`/api/hosts/${row.id}/${ep}`, { version: dlg.version, force: dlg.force })
    ElMessage.success('安装会话 #' + res.id + ' 已启动')
    dlg.visible = false
    gotoView('Sessions')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    dlg.submitting = false
  }
}

async function load(silent) {
  if (!silent) loading.value = true
  try {
    const res = await api.get('/api/hosts')
    hosts.value = res.hosts || []
    enrollToken.value = res.enrollToken || ''
    agentStamp.value = res.agentStamp || ''
  } catch (e) {
    if (!silent) ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

// 手动下发升级指令：置 pending 标记，agent 下一轮轮询（≤25s）任务间隙自升级重启
async function upgradeAgent(row) {
  try {
    const res = await api.post(`/api/hosts/${row.id}/upgrade-agent`, {})
    ElMessage.success(res.message || '升级指令已下发')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function loadAutoUpgrade() {
  try {
    const res = await api.get('/api/settings/agent-upgrade')
    autoUpgrade.value = !!res.auto
  } catch { /* 开关读取失败不阻塞页面 */ }
}

async function saveAutoUpgrade() {
  try {
    await api.post('/api/settings/agent-upgrade', { auto: autoUpgrade.value })
    ElMessage.success(autoUpgrade.value ? '已开启：旧版在线 agent 将在任务间隙自动升级' : '已关闭自动升级（保留手动按钮）')
  } catch (e) {
    ElMessage.error(e.message)
    autoUpgrade.value = !autoUpgrade.value
  }
}

function memOf(row) {
  const mb = row.info.memMB
  if (!mb) return '?'
  return mb >= 1024 ? (mb / 1024).toFixed(0) + 'G' : mb + 'M'
}
// 已发布宿主端口的运行容器（agent 心跳端口档案——分配阶段预检与主机池巡览共用）
function heldPorts(row) {
  return (row.info?.containers || []).filter((c) => (c.hostPorts || []).length)
}

async function saveAddr(row) {
  try {
    await api.put(`/api/hosts/${row.id}/update`, { addr: row.addr, labels: row.labels || '' })
    ElMessage.success('已保存')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function rotate() {
  try {
    await ElMessageBox.confirm('轮换后旧引导令牌立即失效（已注册 agent 不受影响），确定？', '轮换引导令牌', { type: 'warning' })
    const res = await api.post('/api/hosts/enroll-rotate')
    enrollToken.value = res.enrollToken
    ElMessage.success('已轮换，请用新令牌接入')
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

async function revoke(row) {
  try {
    await ElMessageBox.confirm(`吊销后 ${row.name} 的 agent 令牌立即失效（该机任务停止分发），确定？`, '吊销主机', { type: 'warning' })
    await api.post(`/api/hosts/${row.id}/revoke`, {})
    await load()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`删除主机记录 ${row.name}（agent 侧令牌同时失效）？`, '删除主机', { type: 'warning' })
    await api.del(`/api/hosts/${row.id}`)
    await load()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

function dl(name) {
  const plat = name === 'windows.exe' ? 'windows' : 'linux'
  downloadWithToken(`/download/agent/${plat}`, `deliver-agent-${name}`)
}

function copyText(text) {
  navigator.clipboard?.writeText(text).then(() => ElMessage.success('已复制')).catch(() => {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
    ElMessage.success('已复制')
  })
}

onMounted(() => {
  load()
  loadDS()
  loadCS()
  loadAutoUpgrade()
  refreshTimer = setInterval(() => load(true), 15000) // 静默刷新：升级指令下发后版本翻绿可直接看到
})
onUnmounted(() => clearInterval(refreshTimer))
</script>

<style scoped>
.page-head { display: flex; align-items: baseline; gap: 12px; margin-bottom: 12px; }
.page-head h2 { margin: 0; font-size: 17px; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); }
.card { margin-bottom: 14px; }
.media-tip { color: var(--el-text-color-placeholder); font-size: 11px; }
.steps { display: flex; flex-direction: column; gap: 6px; font-size: 12px; color: var(--el-text-color-regular); }
.step .code { display: inline-block; padding: 2px 6px; background: var(--el-fill-color); border-radius: 3px; color: var(--el-color-primary); font-size: 12px; }
.host-name { display: flex; gap: 6px; align-items: center; font-weight: 500; }
.ver-cell { display: inline-flex; gap: 6px; align-items: center; }
.ver-help { vertical-align: -2px; cursor: help; color: var(--el-text-color-secondary); }
.head-actions { display: flex; align-items: center; gap: 10px; }
.auto-up-label { font-size: 12px; color: var(--el-text-color-secondary); }
.host-addr { font-size: 11px; color: var(--el-text-color-secondary); margin-top: 2px; }
.host-ports { display: flex; gap: 4px; flex-wrap: wrap; margin-top: 3px; }
.port-tag { max-width: 100%; }
.caps { display: flex; gap: 4px; flex-wrap: wrap; }
.row-between { display: flex; justify-content: space-between; align-items: center; }
</style>
