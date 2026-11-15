<template>
  <div>
    <!-- 统计 -->
    <el-row :gutter="10">
      <el-col :span="6" v-for="s in statsCards" :key="s.label">
        <el-card class="page-card stat-card" shadow="never">
          <div class="stat-num">{{ s.num }}</div>
          <div class="stat-label">{{ s.label }}</div>
        </el-card>
      </el-col>
    </el-row>

    <!-- 快速开始：三个入口 -->
    <el-row :gutter="10" class="quick-row">
      <el-col :span="8">
        <el-card class="page-card quick-card" shadow="hover" @click="gotoView('Suites')">
          <div class="quick-body">
            <el-icon class="quick-icon" :size="30"><Box /></el-icon>
            <div>
              <div class="quick-title">打一个套件</div>
              <div class="quick-desc">四步向导出套件；交付场景再叠加租户附件「出交付包」产出最终交付包</div>
            </div>
          </div>
        </el-card>
      </el-col>
      <el-col :span="8">
        <el-card class="page-card quick-card" shadow="hover" @click="gotoView('Apps')">
          <div class="quick-body">
            <el-icon class="quick-icon" :size="30"><CirclePlus /></el-icon>
            <div>
              <div class="quick-title">定义一个新应用</div>
              <div class="quick-desc">向导起步：基本信息 → 按模板加服务单元 → 依赖组件 → 变量初始化</div>
            </div>
          </div>
        </el-card>
      </el-col>
      <el-col :span="8">
        <el-card class="page-card quick-card" shadow="hover" @click="dataDialog = true">
          <div class="quick-body">
            <el-icon class="quick-icon" :size="30"><Import /></el-icon>
            <div>
              <div class="quick-title">载入 / 迁移数据</div>
              <div class="quick-desc">一键载入示例预设，或导入导出工作台数据（声明 / 含制品与镜像包的完整包）</div>
            </div>
          </div>
        </el-card>
      </el-col>
    </el-row>

    <!-- 示例预设 -->
    <el-card class="page-card" shadow="never" v-if="presets.length">
      <template #header><span>示例预设（一键载入，幂等可重复执行）</span></template>
      <div class="preset-row">
        <div v-for="p in presets" :key="p.name" class="preset-item">
          <div>
            <b>{{ p.name }}</b>
            <span class="preset-title">{{ p.title }}</span>
          </div>
          <el-button type="primary" size="small" :loading="applying === p.name" @click="applyPreset(p)">一键载入</el-button>
        </div>
      </div>
    </el-card>

    <!-- 最近打包 -->
    <el-card class="page-card" shadow="never">
      <template #header>
        <div class="card-head">
          <span>最近打包</span>
          <el-button link type="primary" @click="gotoView('Suites')">全部记录 →</el-button>
        </div>
      </template>
      <el-table :data="recentPkgs" border size="small">
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="name" label="包名" min-width="120" />
        <el-table-column prop="version" label="版本" width="80" />
        <el-table-column prop="appCode" label="应用" width="130" />
        <el-table-column label="状态" width="80">
          <template #default="{ row }">
            <el-tag size="small" :type="row.status === 'ok' ? 'success' : row.status === 'failed' ? 'danger' : 'warning'">
              {{ { ok: '完成', failed: '失败', running: '进行中' }[row.status] || row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="createdAt" label="发起时间" width="160" />
      </el-table>
      <div v-if="!recentPkgs.length" class="empty-tip">还没有打包记录——点上方「打一个交付包」开始</div>
    </el-card>

    <!-- 数据导入导出对话框 -->
    <el-dialog v-model="dataDialog" title="载入 / 迁移数据" width="620px">
      <el-divider content-position="left">内置示例预设</el-divider>
      <div v-for="p in presets" :key="p.name" class="dlg-row">
        <span>{{ p.name }}（{{ p.title }}）</span>
        <el-button size="small" type="primary" :loading="applying === p.name" @click="applyPreset(p)">载入</el-button>
      </div>
      <el-divider content-position="left">导出（备份 / 迁移到另一台机器）</el-divider>
      <div class="dlg-row">
        <el-checkbox v-model="expMaterials">包含制品文件</el-checkbox>
        <el-checkbox v-model="expImages">包含镜像包（镜像仓 tar）</el-checkbox>
        <span class="dlg-hint">不勾 = 轻量 JSON；任一勾 = zip 完整包</span>
        <el-button size="small" :loading="exporting" @click="doExport">导出</el-button>
      </div>
      <el-divider content-position="left">导入（.json 声明 或 .zip 完整包）</el-divider>
      <div class="dlg-row">
        <el-radio-group v-model="impReplace">
          <el-radio :value="false">合并（按编码覆盖）</el-radio>
          <el-radio :value="true">替换（先清空应用与组件）</el-radio>
        </el-radio-group>
        <el-button size="small" type="primary" :loading="importing" @click="impInput?.click()">选择文件</el-button>
        <input ref="impInput" type="file" accept=".json,.zip" style="display: none" @change="doImport" />
      </div>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api, downloadFile } from '../api'
import { gotoView } from '../nav'

const counts = ref({ apps: 0, components: 0, materials: 0, packages: 0 })
const presets = ref([])
const applying = ref('')
const recentPkgs = ref([])
const dataDialog = ref(false)
const expMaterials = ref(false)
const expImages = ref(false)
const exporting = ref(false)
const impReplace = ref(false)
const importing = ref(false)
const impInput = ref(null)

const statsCards = computed(() => [
  { label: '应用定义', num: counts.value.apps },
  { label: '组件', num: counts.value.components },
  { label: '制品', num: counts.value.materials },
  { label: '打包记录', num: counts.value.packages }
])

async function loadAll() {
  try {
    const [apps, comps, mats, pkgs, ps] = await Promise.all([
      api.get('/api/apps'), api.get('/api/components'), api.get('/api/materials'),
      api.get('/api/packages'), api.get('/api/presets')
    ])
    counts.value = { apps: (apps || []).length, components: (comps || []).length,
      materials: (mats || []).length, packages: (pkgs || []).length }
    presets.value = ps || []
    recentPkgs.value = (pkgs || []).slice(0, 5)
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function applyPreset(p) {
  applying.value = p.name
  try {
    const res = await api.post(`/api/presets/${p.name}/apply`)
    ElMessage.success(`已载入：应用 ${res.apps} · 组件 ${res.components}`)
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    applying.value = ''
  }
}

async function doExport() {
  exporting.value = true
  try {
    const q = []
    if (expMaterials.value) q.push('materials=1')
    if (expImages.value) q.push('images=1')
    await downloadFile('/api/export' + (q.length ? '?' + q.join('&') : ''), 'deliver-studio-export.json')
    ElMessage.success('已导出')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    exporting.value = false
  }
}
async function doImport(ev) {
  const file = ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  const fd = new FormData()
  fd.append('file', file)
  importing.value = true
  try {
    const res = await api.upload(`/api/import?replace=${impReplace.value ? 1 : 0}`, fd)
    ElMessage.success(`导入完成：应用 ${res.apps} · 组件 ${res.components} · 制品 ${res.materials} · 镜像 ${res.images}`)
    dataDialog.value = false
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    importing.value = false
  }
}
onMounted(loadAll)
</script>

<style scoped>
.stat-card { text-align: center; }
.stat-num { font-size: 26px; font-weight: 700; color: var(--el-color-primary); }
.stat-label { color: var(--el-text-color-secondary); font-size: 13px; }
.quick-card { cursor: pointer; }
.quick-body { display: flex; gap: 14px; align-items: center; }
.quick-icon { color: var(--el-color-primary); flex: none; }
.quick-title { font-size: 15px; font-weight: 600; }
.quick-desc { color: var(--el-text-color-secondary); font-size: 12px; margin-top: 4px; }
.preset-row { display: flex; flex-direction: column; gap: 8px; }
.preset-item { display: flex; justify-content: space-between; align-items: center; padding: 6px 10px;
  border: 1px solid var(--el-border-color-lighter); border-radius: 6px; }
.preset-title { color: var(--el-text-color-secondary); margin-left: 10px; font-size: 12px; }
.card-head { display: flex; justify-content: space-between; align-items: center; }
.empty-tip { color: var(--el-text-color-secondary); text-align: center; padding: 18px; font-size: 12px; }
.dlg-row { display: flex; align-items: center; gap: 12px; padding: 4px 0; flex-wrap: wrap; }
.dlg-hint { color: var(--el-text-color-secondary); font-size: 12px; }
</style>
