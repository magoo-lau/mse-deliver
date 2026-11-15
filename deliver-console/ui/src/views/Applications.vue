<template>
  <div class="page">
    <div class="page-head">
      <h2>应用中心</h2>
      <span class="hint">已装应用的运行与升级视图（状态真源=安装清单 inventory）：升级 / 覆盖重装 / 对账 / 回退 / 卸载</span>
      <div class="head-actions">
        <el-button size="small" @click="loadApps">刷新</el-button>
      </div>
    </div>

    <!-- 应用列表 -->
    <el-card v-if="!current" shadow="never" class="card">
      <el-table :data="apps" size="small" @row-click="openApp" highlight-current-row>
        <el-table-column prop="app" label="应用" min-width="160">
          <template #default="{ row }"><b>{{ row.app }}</b></template>
        </el-table-column>
        <el-table-column prop="version" label="已装版本" width="120" />
        <el-table-column prop="services" label="服务" width="70" />
        <el-table-column prop="hosts" label="主机" width="70" />
        <el-table-column label="最近会话" width="130">
          <template #default="{ row }">
            <el-tag size="small" :type="statusType(row.lastStatus)" effect="plain">{{ statusLabel(row.lastStatus) }}</el-tag>
            <el-tag v-if="row.running" size="small" type="success" effect="dark" style="margin-left: 4px">执行中</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="lastSummary" label="结果摘要" min-width="240" show-overflow-tooltip />
        <el-table-column label="" width="100" fixed="right">
          <template #default><el-button link type="primary">详情 →</el-button></template>
        </el-table-column>
      </el-table>
      <el-empty v-if="!apps.length" description="暂无已装应用——去「编排安装」完成首次安装" />
    </el-card>

    <!-- 应用详情 -->
    <template v-else>
      <el-card shadow="never" class="card">
        <template #header>
          <div class="row-between">
            <div class="row-gap">
              <b>{{ current }}</b>
              <el-tag v-if="detail" size="small" effect="plain">{{ detail.services?.length || 0 }} 服务</el-tag>
              <el-button size="small" @click="back">← 返回列表</el-button>
            </div>
            <div class="row-gap">
              <el-button size="small" type="primary" plain @click="goUpgrade">升级 / 覆盖重装</el-button>
              <el-button size="small" :loading="reconciling" @click="reconcile">对账检查</el-button>
              <el-button size="small" type="warning" plain @click="openRollback">回退…</el-button>
              <el-button size="small" type="danger" plain @click="openUninstall">卸载…</el-button>
            </div>
          </div>
        </template>

        <el-alert v-if="drift && drift.drifted" type="warning" :closable="false" class="mb8"
                  :title="`检测到漂移：${driftCount('missing')} 缺失 / ${driftCount('unexpected')} 多余 / ${driftCount('imageMismatch')} 镜像不符——可在编排安装中以「覆盖重装」或「升级（仅变更）」一键收敛`" />
        <el-alert v-else-if="drift" type="success" :closable="false" class="mb8"
                  :title="`对账一致（${drift.items?.length || 0} 个服务实况与清单相符；档案时点 ${fmtTime(driftAt(drift))}）`" />

        <el-table :data="mergedRows" size="small">
          <el-table-column prop="service" label="服务" min-width="150" />
          <el-table-column prop="host" label="主机" width="120" />
          <el-table-column prop="kind" label="形态" width="90" />
          <el-table-column prop="image" label="镜像" min-width="200" show-overflow-tooltip />
          <el-table-column label="实况" width="130">
            <template #default="{ row }">
              <el-tag size="small" effect="plain" :type="driftTagType(row.drift)">{{ driftTag(row.drift) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="实况明细" min-width="200">
            <template #default="{ row }">
              <span v-if="row.driftNote" class="drift-note">{{ row.driftNote }}</span>
              <span v-else>—</span>
            </template>
          </el-table-column>
        </el-table>
      </el-card>

      <!-- 可升级包 + 历史会话 -->
      <el-row :gutter="12">
        <el-col :span="10">
          <el-card shadow="never" class="card">
            <template #header><b>部署包（升级用）</b></template>
            <el-table :data="detail?.packages || []" size="small">
              <el-table-column prop="version" label="版本" width="120" />
              <el-table-column label="上传时间" width="170">
                <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
              </el-table-column>
              <el-table-column label="" width="110" fixed="right">
                <template #default="{ row }">
                  <el-button link type="primary" @click="goUpgradePkg(row)">去编排</el-button>
                </template>
              </el-table-column>
            </el-table>
          </el-card>
        </el-col>
        <el-col :span="14">
          <el-card shadow="never" class="card">
            <template #header><b>近期会话（回退用：仅成功会话可回退）</b></template>
            <el-table :data="(detail?.sessions || []).slice(0, 8)" size="small">
              <el-table-column prop="id" label="#" width="60" />
              <el-table-column prop="version" label="版本" width="110" />
              <el-table-column prop="mode" label="模式" width="110" />
              <el-table-column label="状态" width="90">
                <template #default="{ row }">
                  <el-tag size="small" :type="statusType(row.status)" effect="plain">{{ statusLabel(row.status) }}</el-tag>
                </template>
              </el-table-column>
              <el-table-column label="时间" width="160">
                <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
              </el-table-column>
              <el-table-column label="" width="80" fixed="right">
                <template #default="{ row }">
                  <el-button v-if="row.status === 'done'" link type="warning" @click="doRollback(row)">回退</el-button>
                </template>
              </el-table-column>
            </el-table>
          </el-card>
        </el-col>
      </el-row>
    </template>

    <!-- 卸载确认（输入应用名二次确认） -->
    <el-dialog v-model="uninstallDlg" title="卸载应用" width="520px">
      <el-alert type="warning" :closable="false" class="mb8"
                :title="`将移除 ${current} 的全部容器与 compose 网络；数据目录 ./data 恒保留（需彻底清数据请登主机手工处理）。此操作生成清理会话，可中途中止。`" />
      <el-form label-width="120px" @submit.prevent>
        <el-form-item label="输入应用名确认">
          <el-input v-model="uninstallConfirm" :placeholder="current" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="uninstallDlg = false">取消</el-button>
        <el-button type="danger" :disabled="uninstallConfirm !== current" @click="doUninstall">确认卸载</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtTime } from '../api'
import { gotoView, designPkgId } from '../nav'

const apps = ref([])
const current = ref('')
const detail = ref(null)
const drift = ref(null)
const reconciling = ref(false)
const uninstallDlg = ref(false)
const uninstallConfirm = ref('')

onMounted(loadApps)

async function loadApps() {
  try {
    const res = await api.get('/api/apps')
    apps.value = res.apps || []
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function openApp(row) {
  current.value = row.app
  await loadDetail()
}

async function loadDetail() {
  try {
    detail.value = await api.get('/api/apps/' + encodeURIComponent(current.value))
    drift.value = detail.value.drift || null
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function back() {
  current.value = ''
  detail.value = null
  drift.value = null
  loadApps()
}

// 清单行 × 漂移实况合并视图（服务为主键：漂移独有条目〔unexpected〕也入表）
const mergedRows = computed(() => {
  const rows = (detail.value?.services || []).map((s) => ({ ...s, drift: 'none', driftNote: '' }))
  const bySvc = new Map(rows.map((r) => [r.service, r]))
  for (const it of drift.value?.items || []) {
    if (it.status === 'unexpected') {
      rows.push({ service: it.service, host: it.host, kind: '容器', image: it.actual || '', drift: 'unexpected', driftNote: `${it.container || ''} 不在安装清单（手动起的容器/残留）` })
      continue
    }
    const row = bySvc.get(it.service)
    if (!row) continue
    row.drift = it.status
    if (it.status === 'missing') row.driftNote = '清单在册但实况无此容器（被删/未起来）'
    else if (it.status === 'imageMismatch') row.driftNote = `镜像不符：清单 ${it.expected || '?'} ↔ 实况 ${it.actual || '?'}`
    else if (it.status === 'noArchive') row.driftNote = '主机容器档案缺失（agent 离线/心跳未达/无 docker）——无法判定'
    else row.driftNote = it.archiveStale ? '实况与清单相符（档案时点较早，仅供参考）' : ''
  }
  return rows
})

function driftCount(status) {
  return (drift.value?.items || []).filter((i) => i.status === status).length
}
function driftAt(d) {
  // checkedAt 为 RFC3339 字符串，转 unix 秒供 fmtTime
  const t = Date.parse(d.checkedAt)
  return isNaN(t) ? 0 : Math.floor(t / 1000)
}
function driftTag(st) {
  return { ok: '正常', missing: '缺失', unexpected: '多余', imageMismatch: '镜像不符', noArchive: '无档案', none: '—' }[st] || st
}
function driftTagType(st) {
  return { ok: 'success', missing: 'danger', unexpected: 'warning', imageMismatch: 'warning', noArchive: 'info', none: 'info' }[st] || 'info'
}

function statusType(s) {
  return { done: 'success', running: 'primary', failed: 'danger', aborted: 'warning', uninstall: 'info' }[s] || 'info'
}
function statusLabel(s) {
  return { done: '成功', running: '执行中', failed: '失败', aborted: '已中止', uninstall: '卸载' }[s] || s || '—'
}

// ---- 对账 ----
async function reconcile() {
  reconciling.value = true
  try {
    drift.value = await api.post(`/api/apps/${encodeURIComponent(current.value)}/reconcile`)
    if (!drift.value.drifted) ElMessage.success('对账一致：实况与安装清单相符')
    else ElMessage.warning('检测到漂移——见实况列明细')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    reconciling.value = false
  }
}

// ---- 升级 / 覆盖重装（跳编排向导：预选最新包，模式与 diff 在向导内确认） ----
function goUpgrade() {
  goUpgradePkg((detail.value?.packages || [])[0])
}
function goUpgradePkg(row) {
  if (!row) {
    ElMessage.warning('本应用无已上传的部署包——请先在「部署包」页上传新包')
    return
  }
  designPkgId.value = row.id
  gotoView('Design')
}

// ---- 回退（历史成功会话重放=介质回退；库与数据不回滚，apply-forward） ----
function openRollback() { /* 会话表内行内「回退」按钮 */ }
async function doRollback(row) {
  try {
    await ElMessageBox.confirm(
      `按会话 #${row.id}（版本 ${row.version}）的分配与变量重放安装——介质回退到该版本，库结构与数据不回滚（apply-forward）。继续？`,
      '回退确认', { confirmButtonText: '回退', cancelButtonText: '取消', type: 'warning' }
    )
  } catch { return }
  try {
    const res = await api.post(`/api/apps/${encodeURIComponent(current.value)}/rollback`, { sessionId: row.id })
    ElMessage.success(`回退会话 #${res.id} 已创建——进度见「安装记录」`)
    gotoView('Sessions')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// ---- 卸载 ----
function openUninstall() {
  uninstallConfirm.value = ''
  uninstallDlg.value = true
}
async function doUninstall() {
  try {
    const res = await api.post(`/api/apps/${encodeURIComponent(current.value)}/uninstall`, { confirm: uninstallConfirm.value })
    uninstallDlg.value = false
    ElMessage.success(`卸载会话 #${res.id} 已创建（${res.nodes} 台主机）——进度见「安装记录」`)
    gotoView('Sessions')
  } catch (e) {
    ElMessage.error(e.message)
  }
}
</script>

<style scoped>
.page { display: flex; flex-direction: column; gap: 12px; }
.page-head { display: flex; align-items: center; gap: 12px; }
.page-head h2 { margin: 0; font-size: 17px; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); }
.head-actions { margin-left: auto; }
.card { background: var(--el-bg-color); }
.row-between { display: flex; justify-content: space-between; align-items: center; }
.row-gap { display: flex; gap: 8px; align-items: center; }
.mb8 { margin-bottom: 8px; }
.drift-note { font-size: 12px; color: var(--el-text-color-secondary); }
</style>
