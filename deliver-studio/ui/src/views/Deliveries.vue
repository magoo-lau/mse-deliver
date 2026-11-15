<template>
  <div>
    <!-- 交付附件（签发方上传/直推；套件 + 附件 → 最终交付包） -->
    <el-card class="page-card" shadow="never">
      <template #header>
        <div class="card-head">
          <span>交付附件（签发方产出：license + 租户数据 + 租户变量——选附件「出交付包」即选租户）</span>
          <span>
            <input ref="deliveryInput" type="file" accept=".zip" @change="uploadDelivery" />
            <el-button icon="Refresh" style="margin-left: 8px" @click="loadDeliveries">刷新</el-button>
          </span>
        </div>
      </template>
      <el-table :data="deliveries" border size="small" v-loading="deliveriesLoading">
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="tenantCode" label="租户编码" width="120" />
        <el-table-column prop="tenantName" label="租户名称" min-width="140" show-overflow-tooltip />
        <el-table-column label="类型" width="90" align="center">
          <template #default="{ row }">
            <el-tag size="small" :type="row.packageType === 'UPGRADE' ? 'warning' : 'primary'">
              {{ row.packageType === 'UPGRADE' ? '升级' : '全量' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="licenseId" label="licenseId" width="110" show-overflow-tooltip />
        <el-table-column prop="modules" label="模块集" min-width="140" show-overflow-tooltip />
        <el-table-column label="大小" width="100">
          <template #default="{ row }">{{ fmtSize(row.size) }}</template>
        </el-table-column>
        <el-table-column label="来源" width="70" align="center">
          <template #default="{ row }">
            <el-tag size="small" :type="row.origin === 'push' ? 'success' : 'info'">{{ row.origin === 'push' ? '直推' : '上传' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="createdAt" label="签发/上传时间" width="160" />
        <el-table-column label="操作" width="70" fixed="right">
          <template #default="{ row }">
            <el-button link type="danger" @click="removeDelivery(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- 交付包记录（kind=final；交 deliver-console 安装的正式产物） -->
    <el-card class="page-card" shadow="never">
      <template #header>
        <div class="card-head">
          <span>交付包记录</span>
          <span>
            <el-button type="primary" icon="Promotion" @click="openDeliver()">出交付包</el-button>
            <el-button icon="Refresh" @click="loadPkgs">刷新</el-button>
          </span>
        </div>
      </template>
      <template v-if="finals.length">
        <el-table :data="finals" border size="small">
          <el-table-column prop="id" label="#" width="60" />
          <el-table-column prop="name" label="包名" min-width="130" />
          <el-table-column prop="version" label="版本" width="80" />
          <el-table-column label="租户" width="110" align="center">
            <template #default="{ row }">
              <el-tag type="success" size="small">{{ deliveryTenant(row) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="appCode" label="应用" width="130" />
          <el-table-column label="形态" width="70" align="center">
            <template #default="{ row }">
              <el-tag v-if="row.standalone" type="success" size="small">单机</el-tag>
              <span v-else style="color: var(--el-text-color-secondary)">console</span>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="80">
            <template #default="{ row }">
              <el-tag :type="row.status === 'ok' ? 'success' : row.status === 'failed' ? 'danger' : 'warning'" size="small">
                {{ { ok: '完成', failed: '失败', running: '进行中' }[row.status] || row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="stage" label="阶段" width="120" />
          <el-table-column prop="createdAt" label="发起时间" width="160" />
          <el-table-column label="操作" width="180" fixed="right">
            <template #default="{ row }">
              <el-button v-if="row.status === 'ok'" type="primary" size="small" @click="download(row)">下载交付包</el-button>
              <el-button link type="primary" @click="showLog(row)">日志</el-button>
            </template>
          </el-table-column>
        </el-table>
      </template>
      <el-empty v-else description="暂无交付包——先在『套件打包』页完成打包，再点套件行「出交付包」（或本页上方「出交付包」按钮）选租户附件产出" :image-size="80" />
    </el-card>

    <!-- 出交付包（套件 + 附件 → 最终交付包） -->
    <el-dialog v-model="deliverDlg" title="叠加交付附件，产出最终交付包" width="560px">
      <el-form label-width="90px">
        <el-form-item label="套件*">
          <el-select v-model="deliverSuiteSel" filterable placeholder="选择已完成的套件" style="width: 100%">
            <el-option v-for="r in okKits" :key="r.id" :label="`#${r.id} ${r.name} ${r.version}`" :value="r.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="交付附件*">
          <el-select v-model="deliverDeliveryId" filterable placeholder="选择附件（= 选租户）" style="width: 100%">
            <el-option v-for="d in deliveries" :key="d.id"
              :label="`#${d.id} ${d.tenantCode}（${d.tenantName || '-'}·${d.packageType === 'UPGRADE' ? '升级' : '全量'}·${(d.createdAt || '').slice(5, 16)}）`" :value="d.id" />
          </el-select>
        </el-form-item>
      </el-form>
      <el-alert v-if="!okKits.length" type="warning" :closable="false" style="margin-top: 4px"
        title="暂无已完成套件——请先到『套件打包』页出包" />
      <el-alert v-else-if="!deliveries.length" type="warning" :closable="false" style="margin-top: 4px"
        title="交付附件为空——请先上传/推送签发方附件包" />
      <el-alert type="info" :closable="false" style="margin-top: 10px"
        title="重打包 = 套件内容原样（manifest 硬校验）+ 附件落位 delivery/ 与 extras/ + 重建外层清单；产出包交 deliver-console 安装。" />
      <template #footer>
        <el-button @click="deliverDlg = false">取消</el-button>
        <el-button type="primary" :loading="deliverSubmitting" @click="submitDeliver">开始重打包</el-button>
      </template>
    </el-dialog>

    <PackageLogDrawer v-model:open="logOpen" :rec="logRec" @refreshed="loadPkgs" />
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, downloadViaTicket, fmtSize } from '../api'
import PackageLogDrawer from '../components/PackageLogDrawer.vue'
import { deliverSuiteId } from '../nav'

const pkgs = ref([])
const deliveries = ref([])
const deliveriesLoading = ref(false)
const deliveryInput = ref(null)
const deliverDlg = ref(false)
const deliverSuiteSel = ref(null)
const deliverDeliveryId = ref(null)
const deliverSubmitting = ref(false)
const logOpen = ref(false)
const logRec = ref(null)

const finals = computed(() => pkgs.value.filter((r) => r.kind === 'final'))
const okKits = computed(() => pkgs.value.filter((r) => r.kind !== 'final' && r.status === 'ok'))

// ---- 交付附件 ----
async function loadDeliveries() {
  deliveriesLoading.value = true
  try {
    deliveries.value = (await api.get('/api/deliveries')) || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    deliveriesLoading.value = false
  }
}
async function uploadDelivery(ev) {
  const file = ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  const fd = new FormData()
  fd.append('file', file)
  fd.append('origin', 'upload')
  try {
    const rec = await api.upload('/api/deliveries', fd)
    ElMessage.success(`附件已入库（租户 ${rec.tenantCode}）`)
    await loadDeliveries()
  } catch (e) {
    ElMessage.error(e.message)
  }
}
async function removeDelivery(row) {
  await ElMessageBox.confirm(`删除交付附件 #${row.id}（租户 ${row.tenantCode}）？已产出的交付包不受影响。`, '确认', { type: 'warning' })
  await api.del(`/api/deliveries/${row.id}`)
  ElMessage.success('已删除')
  await loadDeliveries()
}

// ---- 交付包记录与出交付包 ----
async function loadPkgs() {
  try {
    pkgs.value = (await api.get('/api/packages')) || []
  } catch (e) {
    ElMessage.error(e.message)
  }
}
function deliveryTenant(row) {
  const d = deliveries.value.find((x) => x.id === row.deliveryId)
  return d ? d.tenantCode : '?'
}
async function download(row) {
  // GB 级交付包：一次性下载票换浏览器原生下载器直 GET（blob 全量进内存会表现为点击无反应）
  try {
    await downloadViaTicket(`/api/packages/${row.id}/download`)
  } catch (e) {
    ElMessage.error(e.message)
  }
}
function showLog(row) {
  logRec.value = row
  logOpen.value = true
}
// 弹窗默认预选：最新已完成套件 + 最新附件（多数「刚打包/刚签发完就出包」场景直接确认）
function latestOkKitId() {
  return okKits.value.length ? Math.max(...okKits.value.map((r) => r.id)) : null
}
function latestDeliveryId() {
  return deliveries.value.length ? Math.max(...deliveries.value.map((d) => d.id)) : null
}
function openDeliver(suiteRow) {
  deliverDlg.value = true
  deliverSuiteSel.value = suiteRow?.id ?? latestOkKitId()
  deliverDeliveryId.value = latestDeliveryId()
}
async function submitDeliver() {
  if (!deliverSuiteSel.value) return ElMessage.warning('请选择套件')
  if (!deliverDeliveryId.value) return ElMessage.warning('请选择交付附件')
  deliverSubmitting.value = true
  try {
    const rec = await api.post(`/api/packages/${deliverSuiteSel.value}/deliver`, { deliveryId: deliverDeliveryId.value })
    ElMessage.success(`重打包任务 #${rec.id} 已提交`)
    deliverDlg.value = false
    await loadPkgs()
    logRec.value = rec
    logOpen.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    deliverSubmitting.value = false
  }
}

onMounted(async () => {
  await Promise.all([loadPkgs(), loadDeliveries()])
  // 套件打包页「出交付包」跳入：预选该套件直接开弹窗（套件被删/未完成则不弹，留在本页）
  if (deliverSuiteId.value) {
    const row = okKits.value.find((r) => r.id === deliverSuiteId.value)
    deliverSuiteId.value = 0
    if (row) openDeliver(row)
    else ElMessage.warning('未找到该套件记录（须为已完成状态）')
  }
})
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
</style>
