<template>
  <div>
    <!-- 打包向导 -->
    <el-card class="page-card" shadow="never" v-loading="metaLoading">
      <template #header><span>打包向导（应用单元 + 依赖组件 → 离线部署包，四步出包）</span></template>
      <el-steps :active="step" simple style="margin-bottom: 16px">
        <el-step title="① 应用与组件" />
        <el-step title="② 镜像来源" />
        <el-step title="③ 打包时变量" />
        <el-step title="④ 预览与打包" />
      </el-steps>

      <!-- ① 应用与组件 -->
      <div v-if="step === 0">
        <el-form inline>
          <el-form-item label="应用*">
            <el-select v-model="form.appCode" style="width: 220px" placeholder="选择应用定义" @change="onAppChange">
              <el-option v-for="a in apps" :key="a.code" :label="`${a.name || a.code}（${a.code}）`" :value="a.code" />
            </el-select>
          </el-form-item>
          <el-form-item label="包名*"><el-input v-model="form.name" placeholder="如 tenant-a" style="width: 160px" /></el-form-item>
          <el-form-item label="版本"><el-input v-model="form.version" style="width: 100px" /></el-form-item>
        </el-form>
        <el-form inline>
          <el-form-item>
            <el-checkbox v-model="form.standalone">
              单机部署——随包附带 install.sh（单机全 Docker 首装；升级/回退/多机仍用 deliver-console）
            </el-checkbox>
          </el-form-item>
        </el-form>
        <template v-if="currentApp">
          <el-divider content-position="left">服务单元（{{ currentApp.units.length }} 个——镜像来源在下一步选）</el-divider>
          <div class="unit-summary">
            <span v-for="u in currentApp.units" :key="u.code" class="unit-chip" :style="{ borderColor: roleColor(u.role) }">
              <el-icon><component :is="roleIcon(u.role)" /></el-icon>{{ u.code }}<i v-if="u.entrypoint" class="entry-flag">入口</i>
            </span>
          </div>
          <el-divider content-position="left">依赖组件（{{ form.componentCodes.length }} / {{ components.length }}）</el-divider>
          <el-empty v-if="!components.length" description="组件目录为空（可跳过——纯应用交付）" :image-size="60" />
          <el-checkbox-group v-model="form.componentCodes">
            <template v-for="cat in grouped" :key="cat.key">
              <div class="group-label">{{ cat.label }}</div>
              <el-checkbox v-for="c in cat.items" :key="c.code" :value="c.code">
                {{ c.code }}<span class="comp-name">{{ c.name }}</span>
              </el-checkbox>
            </template>
          </el-checkbox-group>
        </template>
      </div>

      <!-- ② 单元制品 -->
      <div v-if="step === 1 && currentApp">
        <el-alert type="info" :closable="false" style="margin-bottom: 12px"
          title="每个单元选择本次打包的镜像来源：应用包构建（上传的应用包 + 单元 Dockerfile）/ 镜像包导入 / 镜像地址导出——可临时改，不回写应用定义。" />
        <div v-for="p in unitPicks" :key="p.code" class="pick-card">
          <div class="pick-head">
            <el-icon :color="roleColor(p.unit.role)"><component :is="roleIcon(p.unit.role)" /></el-icon>
            <b>{{ p.unit.name || p.code }}</b>
            <el-tag size="small" type="info" class="mono">{{ p.code }}</el-tag>
            <el-tag v-if="p.unit.entrypoint" size="small" type="success">入口</el-tag>
            <span v-if="p.unit.port" class="pick-port">端口 {{ p.unit.port }}</span>
          </div>
          <el-radio-group v-model="p.source" class="pick-source">
            <el-radio-button value="artifact">应用包构建</el-radio-button>
            <el-radio-button value="image_tar">镜像包导入</el-radio-button>
            <el-radio-button value="image_ref">镜像地址导出</el-radio-button>
          </el-radio-group>
          <div class="pick-body">
            <template v-if="p.source === 'artifact'">
              <el-select v-model="p.materialId" clearable filterable placeholder="选择应用包（后端=jar/fat-jar；前端=UI zip）" style="width: 420px">
                <el-option v-for="m in artifactMaterials" :key="m.id" :label="materialLabel(m)" :value="m.id" />
              </el-select>
              <div class="pick-hint">构建镜像：{{ p.unit.imageRepo }}:{{ form.version || '1.0.0' }}（单元 Dockerfile 模板现场构建；zip 自动解压为上下文——仅适配 COPY . 形态的前端单元）</div>
            </template>
            <template v-else-if="p.source === 'image_tar'">
              <el-select v-model="p.materialId" clearable filterable placeholder="选择镜像包（docker save 产物）" style="width: 420px">
                <el-option v-for="m in imageTarMaterials" :key="m.id" :label="materialLabel(m)" :value="m.id" />
              </el-select>
              <div class="pick-hint">镜像包直接进包不构建；镜像取 tar 文件名</div>
            </template>
            <template v-else>
              <el-input v-model="p.imageRef" class="mono" placeholder="镜像地址 repo/name:tag（如 mse/app:1.0.0）" style="width: 380px" />
              <div class="pick-hint">按「本机 docker → 镜像仓 → 报缺」解析导出</div>
            </template>
          </div>
          <el-checkbox v-if="p.source === 'artifact'" v-model="p.withArtifact" class="pick-artifact">
            制品随包附带（artifacts/）——供 deliver-console 以 systemd 裸机部署本单元
          </el-checkbox>
          <div v-if="pickError(p)" class="pick-error">{{ pickError(p) }}</div>
        </div>
        <el-divider content-position="left">附加文件（extras/，如 db/schema.sql——由组件编排自挂载）</el-divider>
        <el-select v-model="form.extraMaterialIds" multiple collapse-tags clearable placeholder="可选" style="min-width: 320px">
          <el-option v-for="m in extraMaterials" :key="m.id" :label="materialLabel(m)" :value="m.id" />
        </el-select>
      </div>

      <!-- ③ 打包时变量 -->
      <div v-if="step === 2">
        <template v-if="buildVars.length">
          <el-alert type="info" :closable="false" style="margin-bottom: 12px"
            title="这些变量在打包时赋值并直接烘进镜像与编排（安装时不可改）；留空则使用应用定义里的默认值。" />
          <el-form label-width="240px" style="max-width: 640px">
            <el-form-item v-for="v in buildVars" :key="v.name" :label="`${v.name}（${v.label || '打包时变量'}）`">
              <el-input v-model="form.buildVars[v.name]" :placeholder="v.value || '必填'" :show-password="!!v.sensitive" />
            </el-form-item>
          </el-form>
        </template>
        <el-empty v-else description="没有打包时变量——直接下一步（安装时变量由 deliver-console 安装表单填写）" :image-size="70" />
      </div>

      <!-- ④ 预览与打包 -->
      <div v-if="step === 3 && currentApp">
        <el-row :gutter="10">
          <el-col :span="12">
            <div class="preview-label">部署拓扑</div>
            <OrchestrationCanvas :units="currentApp.units" :components="components" :selected-codes="form.componentCodes" :editable="false" />
          </el-col>
          <el-col :span="12">
            <div class="preview-label">部署包结构预览</div>
            <PkgTreePreview :units="previewUnits" :components="selectedComponents" :extras="extrasForPreview" :version="form.version || '1.0.0'" :standalone="form.standalone" />
          </el-col>
        </el-row>
        <div class="preview-label" style="margin-top: 10px">变量总览（{{ summaryVars.length }} 项）</div>
        <VarTable :rows="summaryVars" />
        <div class="submit-bar">
          <div v-if="submitError" class="pick-error">{{ submitError }}</div>
          <el-button type="primary" size="large" icon="Box" :loading="submitting" @click="submit">开始打包</el-button>
        </div>
      </div>

      <!-- 步导航 -->
      <div class="step-bar">
        <el-button :disabled="step === 0" @click="step--">上一步</el-button>
        <el-button v-if="step < 3" type="primary" @click="next">下一步</el-button>
      </div>
    </el-card>

    <!-- 套件记录（kind=kit；叠加租户附件出交付包在「交付包」页） -->
    <el-card class="page-card" shadow="never">
      <template #header>
        <div class="card-head">
          <span>套件记录</span>
          <el-button icon="Refresh" @click="loadPkgs">刷新</el-button>
        </div>
      </template>
      <el-table :data="kits" border size="small">
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="name" label="包名" min-width="130" />
        <el-table-column prop="version" label="版本" width="80" />
        <el-table-column prop="appCode" label="应用" width="130" />
        <el-table-column prop="components" label="组件" min-width="140" show-overflow-tooltip />
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
        <el-table-column label="操作" width="230" fixed="right">
          <template #default="{ row }">
            <!-- 交付场景主操作：套件不含租户变量/license，交 console 安装须先「出交付包」 -->
            <el-button v-if="row.status === 'ok'" type="primary" size="small" @click="deliverTo(row)">出交付包</el-button>
            <el-tooltip v-if="row.status === 'ok'" placement="top"
              content="套件不含租户变量/license，交 console 安装须先「出交付包」；套件直接安装仅适用于测试环境">
              <el-button link type="primary" @click="download(row)">下载套件</el-button>
            </el-tooltip>
            <el-button link type="primary" @click="showLog(row)">日志</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <PackageLogDrawer v-model:open="logOpen" :rec="logRec" @refreshed="loadPkgs" />
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api, downloadViaTicket, fmtSize } from '../api'
import { ROLE_META, CATEGORY_META, BUILTIN_VARS, isArtifactKind } from '../lib'
import OrchestrationCanvas from '../components/OrchestrationCanvas.vue'
import PkgTreePreview from '../components/PkgTreePreview.vue'
import VarTable from '../components/VarTable.vue'
import PackageLogDrawer from '../components/PackageLogDrawer.vue'
import { gotoView, deliverSuiteId } from '../nav'

const apps = ref([])
const components = ref([])
const materials = ref([])
const pkgs = ref([])
const metaLoading = ref(false)
const submitting = ref(false)
const step = ref(0)
const logOpen = ref(false)
const logRec = ref(null)

const form = ref({ name: '', version: '1.0.0', appCode: '', componentCodes: [], extraMaterialIds: [], buildVars: {}, standalone: false })
const unitPicks = ref([]) // [{code, unit, source, materialId, imageRef}]

const currentApp = computed(() => apps.value.find((a) => a.code === form.value.appCode))
const artifactMaterials = computed(() => materials.value.filter((m) => isArtifactKind(m.kind)))
const imageTarMaterials = computed(() => materials.value.filter((m) => m.kind === 'image_tar'))
const extraMaterials = computed(() => materials.value.filter((m) => m.kind === 'extra'))
const selectedComponents = computed(() => components.value.filter((c) => form.value.componentCodes.includes(c.code)))
const buildVars = computed(() => (currentApp.value?.variables || []).filter((v) => v.when === 'build'))
const grouped = computed(() =>
  Object.entries(CATEGORY_META).map(([key, label]) => ({ key, label, items: components.value.filter((c) => c.category === key) }))
    .filter((g) => g.items.length)
)
const kits = computed(() => pkgs.value.filter((r) => r.kind !== 'final'))
const roleIcon = (r) => (ROLE_META[r] || ROLE_META.custom).icon
const roleColor = (r) => (ROLE_META[r] || ROLE_META.custom).color
// 制品下拉 label：同名 jar 跨版本共存（服务端按类型+文件名+应用+版本保留最新一条），必须带应用与版本才能分辨
const materialLabel = (m) => `${m.fileName}（${[m.appCode, m.version, fmtSize(m.size)].filter(Boolean).join(' · ')}）`

// 预览用：单元解析出的镜像形态
const previewUnits = computed(() => unitPicks.value.map((p) => {
  const u = { ...p.unit, source: p.source, imageRef: p.imageRef }
  if (p.source === 'image_tar' && p.materialId) {
    const m = materials.value.find((x) => x.id === p.materialId)
    if (m) u.imageRepo = m.fileName.replace(/\.tar$/, '')
  }
  return u
}))
const extrasForPreview = computed(() =>
  form.value.extraMaterialIds.map((id) => materials.value.find((m) => m.id === id)?.fileName).filter(Boolean))

// 变量总览：应用安装时/打包时 + 组件端点/凭据 + 单元内置
const summaryVars = computed(() => {
  const rows = []
  for (const v of currentApp.value?.variables || []) {
    rows.push({ name: v.name, label: v.label, when: v.when, sourceLabel: whenSourceLabel(v), origin: '应用自定义', originType: 'app', sensitive: v.sensitive })
  }
  for (const c of selectedComponents.value) {
    for (const ep of c.endpoints || []) {
      rows.push({ name: ep.placeholder, label: ep.label || `服务 ${ep.service}:${ep.port}`, when: 'install',
        sourceLabel: `console 回填 ${ep.from || 'url'}`, origin: `组件 ${c.code}`, originType: 'component' })
    }
    for (const cr of c.credentials || []) {
      rows.push({ name: cr.placeholder, label: cr.label || (cr.env ? `.env ${cr.env}` : ''), when: 'install',
        sourceLabel: { ask: '安装时询问', random: '随机生成', const: '固定值' }[cr.generate || 'const'], origin: `组件 ${c.code}`, originType: 'component' })
    }
  }
  for (const b of BUILTIN_VARS) {
    rows.push({ ...b, sourceLabel: '打包引擎自动', origin: '单元内置', originType: 'builtin' })
  }
  return rows
})
function whenSourceLabel(v) {
  if (v.when === 'build') return '打包向导填值'
  return { input: '安装时询问（有默认）', const: '固定值', random: '随机生成' }[v.source || 'input']
}

async function loadMeta() {
  metaLoading.value = true
  try {
    const [a, c, m] = await Promise.all([api.get('/api/apps'), api.get('/api/components'), api.get('/api/materials')])
    apps.value = a || []
    components.value = c || []
    materials.value = m || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    metaLoading.value = false
  }
}
function onAppChange(code) {
  const app = apps.value.find((a) => a.code === code)
  if (!app) return
  form.value.version = app.defaultVersion || '1.0.0'
  if (!form.value.name) form.value.name = code
  const def = app.defaultComponents || []
  form.value.componentCodes = def.length ? def.filter((c) => components.value.some((x) => x.code === c))
    : components.value.filter((c) => c.defaultChecked).map((c) => c.code)
  form.value.buildVars = {}
  for (const v of (app.variables || []).filter((x) => x.when === 'build')) {
    form.value.buildVars[v.name] = v.value || ''
  }
  unitPicks.value = (app.units || []).map((u) => ({
    code: u.code, unit: u, source: u.source || 'artifact', materialId: null, imageRef: u.imageRef || '', withArtifact: false
  }))
  step.value = 0
}
function pickError(p) {
  if (p.source === 'artifact' && !p.materialId) return '请选择应用包'
  if (p.source === 'image_tar' && !p.materialId) return '请选择镜像包'
  if (p.source === 'image_ref' && !(p.imageRef || '').trim()) return '请填写镜像地址'
  if (p.source === 'artifact' && !(p.unit.imageRepo || '').trim()) return '单元未配置镜像名（应用定义里补）'
  if (p.withArtifact && p.source !== 'artifact') return '「制品随包附带」仅在应用包来源可用'
  // zip × 整包引用互斥（引擎 buildPlan 同款硬校验的前置反馈）：zip 解压为上下文、原包不保留，
  // COPY {{ARTIFACT_FILE}} 的后端 fat-jar 模板必失败——防把 UI zip 选给后端单元（实测包 #10）
  if (p.source === 'artifact' && p.materialId) {
    const m = materials.value.find((x) => x.id === p.materialId)
    if (m && /\.zip$/i.test(m.fileName) && (p.unit.dockerfile || '').includes('{{ARTIFACT_FILE}}')) {
      return `zip 应用包（${m.fileName}）不能配给整包引用 {{ARTIFACT_FILE}} 的单元（zip 会解压为构建上下文）——请选 jar/fat-jar 应用包；UI zip 配给前端单元`
    }
  }
  return ''
}
function next() {
  if (step.value === 0) {
    if (!form.value.appCode) return ElMessage.warning('请选择应用')
    if (!form.value.name) return ElMessage.warning('请填写包名')
  }
  if (step.value === 1) {
    const errs = unitPicks.value.map(pickError).filter(Boolean)
    if (errs.length) return ElMessage.warning(errs[0])
  }
  step.value++
}
const submitError = computed(() => {
  if (!currentApp.value) return '未选择应用'
  if (form.value.standalone) {
    const cc = selectedComponents.value.find((c) => c.configCenter)
    if (cc) return `勾选「单机部署（随包 install.sh）」时不能勾选配置中心组件 ${cc.code}——请改用 deliver-console 安装`
  }
  const errs = unitPicks.value.map(pickError).filter(Boolean)
  if (errs.length) return errs[0]
  for (const v of buildVars.value) {
    if (!(form.value.buildVars[v.name] || '').trim() && !(v.value || '').trim()) return `打包时变量 ${v.name} 未赋值`
  }
  return ''
})
async function submit() {
  if (submitError.value) return ElMessage.warning(submitError.value)
  submitting.value = true
  try {
    const vars = {}
    for (const v of buildVars.value) {
      const val = (form.value.buildVars[v.name] || '').trim() || v.value
      if (val) vars[v.name] = val
    }
    const payload = {
      name: form.value.name, version: form.value.version, appCode: form.value.appCode,
      units: unitPicks.value.map((p) => ({ code: p.code, source: p.source, materialId: p.materialId || 0, imageRef: p.imageRef || '', withArtifact: !!p.withArtifact })),
      componentCodes: form.value.componentCodes, extraMaterialIds: form.value.extraMaterialIds, buildVars: vars,
      standalone: !!form.value.standalone
    }
    const rec = await api.post('/api/packages', payload)
    ElMessage.success(`打包任务 #${rec.id} 已提交`)
    form.value = { ...form.value, name: '' }
    await loadPkgs()
    logRec.value = rec
    logOpen.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    submitting.value = false
  }
}
async function loadPkgs() {
  try {
    pkgs.value = (await api.get('/api/packages')) || []
  } catch (e) {
    ElMessage.error(e.message)
  }
}
async function download(row) {
  // window.open 裸 GET 带不上 token 会被 401，必须走带鉴权的 blob 下载
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
// 套件行「出交付包」：携带预选套件跳「交付包」页直接开弹窗（选附件=选租户）
function deliverTo(row) {
  deliverSuiteId.value = row.id
  gotoView('Deliveries')
}
onMounted(() => {
  loadMeta()
  loadPkgs()
})
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
.group-label { color: var(--el-text-color-secondary); margin: 6px 0 2px; font-size: 12px; }
.comp-name { color: var(--el-text-color-secondary); margin-left: 4px; font-size: 12px; }
.unit-summary { display: flex; flex-wrap: wrap; gap: 6px; }
.unit-chip { display: inline-flex; align-items: center; gap: 3px; border: 1px solid; border-radius: 10px;
  padding: 1px 8px; font-size: 12px; font-family: Consolas, Monaco, monospace; }
.entry-flag { font-style: normal; color: var(--el-color-success); margin-left: 2px; font-size: 11px; }
.pick-card { border: 1px solid var(--el-border-color); border-radius: 8px; padding: 12px; margin-bottom: 10px; }
.pick-head { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.pick-port { color: var(--el-text-color-secondary); font-size: 12px; }
.pick-source { margin-bottom: 10px; }
.pick-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-top: 6px; }
.pick-error { color: var(--el-color-danger); font-size: 12px; margin-top: 6px; }
.preview-label { color: var(--el-text-color-secondary); font-size: 12px; margin-bottom: 6px; }
.submit-bar { display: flex; justify-content: flex-end; align-items: center; gap: 14px; margin-top: 12px; }
.step-bar { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; border-top: 1px solid var(--el-border-color-lighter); padding-top: 12px; }
</style>
