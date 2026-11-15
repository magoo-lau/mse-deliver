<template>
  <div>
    <!-- 应用列表 -->
    <el-card class="page-card" shadow="never" v-loading="loading">
      <template #header>
        <div class="card-head">
          <span>应用定义（服务单元自由组合 + 变量清单；前端、后端、任意镜像都是「单元」）</span>
          <el-button type="primary" icon="Plus" @click="openWizard">新建应用（向导）</el-button>
        </div>
      </template>
      <el-empty v-if="!apps.length" description="还没有应用——右上角「新建应用」走向导，或首页一键载入示例预设" />
      <el-row :gutter="10">
        <el-col :span="8" v-for="a in apps" :key="a.code">
          <el-card class="app-card" shadow="hover" @click="openEditor(a)">
            <div class="app-card-head">
              <b class="app-name">{{ a.name || a.code }}</b>
              <el-tag size="small" type="info" class="mono">{{ a.code }}</el-tag>
            </div>
            <div class="app-units">
              <span v-for="u in a.units" :key="u.code" class="unit-chip" :style="{ borderColor: roleColor(u.role) }">
                <el-icon><component :is="roleIcon(u.role)" /></el-icon>{{ u.code }}<i v-if="u.entrypoint" class="entry-flag">入口</i>
              </span>
            </div>
            <div class="app-meta">
              <span>变量 {{ (a.variables || []).length }} 个</span>
              <span v-if="a.defaultComponents && a.defaultComponents.length">依赖 {{ a.defaultComponents.length }} 组件</span>
              <span>默认版本 {{ a.defaultVersion || '1.0.0' }}</span>
            </div>
            <div v-if="a.remark" class="app-remark">{{ a.remark }}</div>
          </el-card>
        </el-col>
      </el-row>
    </el-card>

    <!-- 新建向导 -->
    <el-dialog v-model="wizard" title="新建应用（向导）" width="920px" top="3vh" :close-on-click-modal="false">
      <el-steps :active="wStep" simple style="margin-bottom: 14px">
        <el-step title="基本信息" />
        <el-step title="添加服务单元" />
        <el-step title="依赖组件" />
        <el-step title="变量初始化" />
      </el-steps>

      <!-- ① 基本信息 -->
      <div v-if="wStep === 0">
        <el-form label-width="110px" style="max-width: 560px">
          <el-form-item label="编码*" required>
            <el-input v-model="wForm.code" placeholder="小写字母数字短横线，如 demo-app" class="mono" />
          </el-form-item>
          <el-form-item label="名称"><el-input v-model="wForm.name" placeholder="如 演示应用" /></el-form-item>
          <el-form-item label="默认版本"><el-input v-model="wForm.defaultVersion" placeholder="1.0.0" style="width: 140px" /></el-form-item>
          <el-form-item label="备注"><el-input v-model="wForm.remark" /></el-form-item>
        </el-form>
      </div>

      <!-- ② 添加单元 -->
      <div v-if="wStep === 1">
        <div class="tpl-grid">
          <div v-for="t in templates" :key="t.key" class="tpl-card" @click="wizardAddUnit(t)">
            <div class="tpl-title"><el-icon><CirclePlus /></el-icon>{{ t.title }}</div>
            <div class="tpl-desc">{{ t.desc }}</div>
          </div>
        </div>
        <el-divider content-position="left">已添加单元（点击模板卡片继续追加；顺序即编排顺序，第一个后端单元默认为入口）</el-divider>
        <div v-if="!wUnits.length" class="empty-tip">尚未添加单元——点上方模板卡片起步，之后可以随时改</div>
        <div v-for="(u, i) in wUnits" :key="i" class="unit-row">
          <el-icon class="unit-role-icon" :color="roleColor(u.role)"><component :is="roleIcon(u.role)" /></el-icon>
          <el-input v-model="u.code" class="mono" style="width: 150px" placeholder="编码" />
          <el-input v-model="u.name" style="width: 150px" placeholder="名称" />
          <el-tag size="small" type="info">{{ roleLabel(u.role) }}</el-tag>
          <el-tag v-if="u.entrypoint" size="small" type="success">入口</el-tag>
          <el-button link type="primary" :disabled="i === 0" @click="wMove(i, -1)">↑</el-button>
          <el-button link type="primary" :disabled="i === wUnits.length - 1" @click="wMove(i, 1)">↓</el-button>
          <el-button link type="danger" @click="wUnits.splice(i, 1)">移除</el-button>
        </div>
      </div>

      <!-- ③ 依赖组件 -->
      <div v-if="wStep === 2">
        <el-alert type="info" :closable="false" title="这里选的是打包时的默认勾选（之后随时可改）；组件=第三方设施（数据库/缓存/中间件），声明来自「组件目录」。" style="margin-bottom: 10px" />
        <el-empty v-if="!components.length" description="组件目录为空——可先跳过，之后在「组件目录」维护" />
        <el-checkbox-group v-model="wComps">
          <div v-for="cat in groupedComponents" :key="cat.key" class="comp-group">
            <div class="comp-group-title">{{ cat.label }}</div>
            <el-checkbox v-for="c in cat.items" :key="c.code" :value="c.code">
              {{ c.code }}<span class="comp-name">{{ c.name }}</span>
            </el-checkbox>
          </div>
        </el-checkbox-group>
      </div>

      <!-- ④ 变量初始化 -->
      <div v-if="wStep === 3">
        <el-alert type="info" :closable="false" title="已扫描模板中的 {{X}} 变量引用，勾选即登记为变量（安装时人工填写为默认方式）；也可跳过稍后在编辑器维护。" style="margin-bottom: 10px" />
        <div v-if="!wVarCandidates.length" class="empty-tip">模板中没有发现需要登记的变量引用</div>
        <el-table v-else :data="wVarCandidates" border size="small">
          <el-table-column width="50">
            <template #default="{ row }"><el-checkbox v-model="row.checked" /></template>
          </el-table-column>
          <el-table-column label="变量引用" width="200">
            <template #default="{ row }"><span class="mono ph">{{ ph(row.name) }}</span></template>
          </el-table-column>
          <el-table-column label="说明（安装时提示）" min-width="160">
            <template #default="{ row }"><el-input v-model="row.label" placeholder="如：租户编号" /></template>
          </el-table-column>
          <el-table-column label="赋值时机" width="120">
            <template #default="{ row }">
              <el-select v-model="row.when">
                <el-option value="install" label="安装时" />
                <el-option value="build" label="打包时" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="来源" width="120">
            <template #default="{ row }">
              <el-select v-model="row.source">
                <el-option value="input" label="人工填写" />
                <el-option value="const" label="固定值" />
                <el-option value="random" label="随机生成" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="敏感" width="60" align="center">
            <template #default="{ row }"><el-switch v-model="row.sensitive" size="small" /></template>
          </el-table-column>
          <el-table-column label="值/默认值" min-width="120">
            <template #default="{ row }"><el-input v-model="row.value" :show-password="!!row.sensitive" /></template>
          </el-table-column>
        </el-table>
      </div>

      <template #footer>
        <el-button :disabled="wStep === 0" @click="wStep--">上一步</el-button>
        <el-button v-if="wStep < 3" type="primary" @click="wNext">下一步</el-button>
        <el-button v-else type="primary" :loading="creating" @click="createApp">创建并编辑</el-button>
      </template>
    </el-dialog>

    <!-- 编辑抽屉 -->
    <el-drawer v-model="editor" size="94%" :with-header="false" :close-on-click-modal="false">
      <div class="editor" v-if="editApp">
        <div class="editor-top">
          <el-input v-model="editApp.name" style="width: 240px" placeholder="应用名称" />
          <el-tag class="mono">{{ editApp.code }}</el-tag>
          <el-input v-model="editApp.defaultVersion" style="width: 110px" placeholder="默认版本" />
          <div class="editor-top-spacer" />
          <el-button :loading="saving" type="primary" icon="Check" @click="saveApp">保存</el-button>
          <el-button @click="editor = false">关闭</el-button>
        </div>
        <div class="editor-body">
          <!-- 左：单元列表 -->
          <div class="unit-list">
            <div class="unit-list-title">服务单元（{{ editApp.units.length }}）</div>
            <div
              v-for="(u, i) in editApp.units" :key="u.code + i"
              class="unit-item" :class="{ active: i === activeUnitIdx }" @click="activeUnitIdx = i"
            >
              <el-icon :color="roleColor(u.role)"><component :is="roleIcon(u.role)" /></el-icon>
              <div class="unit-item-main">
                <div class="unit-item-code">{{ u.code }}<el-tag v-if="u.entrypoint" size="small" type="success" class="unit-entry">入口</el-tag></div>
                <div class="unit-item-sub">{{ roleLabel(u.role) }} · {{ sourceLabel(u.source) }}</div>
              </div>
              <div class="unit-item-ops">
                <el-button link :disabled="i === 0" @click.stop="editMove(i, -1)">↑</el-button>
                <el-button link :disabled="i === editApp.units.length - 1" @click.stop="editMove(i, 1)">↓</el-button>
                <el-button link type="danger" :disabled="editApp.units.length <= 1" @click.stop="editRemoveUnit(i)">×</el-button>
              </div>
            </div>
            <el-button class="unit-add" icon="Plus" @click="addUnitDialog = true">添加单元</el-button>
          </div>
          <!-- 右：详情页签 -->
          <div class="unit-detail">
            <el-tabs v-model="detailTab">
              <el-tab-pane label="单元详情" name="unit">
                <el-collapse v-model="openSections">
                  <el-collapse-item name="base" title="基本信息">
                    <el-form label-width="130px" class="detail-form">
                      <el-row :gutter="10">
                        <el-col :span="8"><el-form-item label="编码*"><el-input v-model="activeUnit.code" class="mono" /></el-form-item></el-col>
                        <el-col :span="8"><el-form-item label="名称"><el-input v-model="activeUnit.name" /></el-form-item></el-col>
                        <el-col :span="8">
                          <el-form-item label="角色">
                            <el-select v-model="activeUnit.role">
                              <el-option value="backend" label="后端" />
                              <el-option value="frontend" label="前端" />
                              <el-option value="custom" label="自定义" />
                            </el-select>
                          </el-form-item>
                        </el-col>
                      </el-row>
                      <el-row :gutter="10">
                        <el-col :span="8"><el-form-item label="服务端口"><el-input v-model="activeUnit.port" placeholder="8080" /></el-form-item></el-col>
                        <el-col :span="8"><el-form-item label="健康检查路径"><el-input v-model="activeUnit.healthPath" placeholder="/actuator/health（空=跳过）" /></el-form-item></el-col>
                        <el-col :span="8">
                          <el-form-item label="主入口">
                            <el-switch :model-value="activeUnit.entrypoint" @change="setEntrypoint" />
                            <span class="form-hint">对外端口/健康检查/复用模式目标</span>
                          </el-form-item>
                        </el-col>
                      </el-row>
                      <el-form-item label="备注"><el-input v-model="activeUnit.remark" /></el-form-item>
                    </el-form>
                  </el-collapse-item>

                  <el-collapse-item name="source" title="镜像来源（三选一）">
                    <div class="sec-hint">单元镜像怎么来：应用包构建（上传应用包 + Dockerfile 现场构建）/ 镜像包导入（docker save 的 tar 直接进包）/ 镜像地址导出（从本机 docker 或镜像仓导出）。</div>
                    <div class="source-cards">
                      <div
                        v-for="s in sourceOptions" :key="s.value"
                        class="source-card" :class="{ active: activeUnit.source === s.value }"
                        @click="activeUnit.source = s.value"
                      >
                        <div class="source-title">{{ s.label }}</div>
                        <div class="source-hint">{{ s.hint }}</div>
                      </div>
                    </div>
                    <el-form label-width="130px" class="detail-form">
                      <el-row :gutter="10">
                        <el-col v-if="activeUnit.source === 'artifact'" :span="12">
                          <el-form-item label="镜像名*">
                            <el-input v-model="activeUnit.imageRepo" class="mono" placeholder="mse/my-app（tag=打包版本）" />
                          </el-form-item>
                        </el-col>
                        <el-col v-if="activeUnit.source !== 'artifact'" :span="12">
                          <el-form-item label="缺省镜像地址">
                            <el-input v-model="activeUnit.imageRef" class="mono" placeholder="repo/name:tag（打包时可改）" />
                          </el-form-item>
                        </el-col>
                      </el-row>
                    </el-form>
                    <template v-if="activeUnit.source === 'artifact'">
                      <div class="section-label">Dockerfile 模板（构建上下文 = 应用包所在目录；zip 应用包自动解压；<span class="mono">{{ ph('ARTIFACT_FILE') }}</span> 引用应用包文件名）</div>
                      <TemplateEditor v-model="activeUnit.dockerfile" :vars="buildVarList" height="200px"
                        placeholder="FROM eclipse-temurin:21-jre-jammy&#10;COPY {{ARTIFACT_FILE}} app.jar" />
                    </template>
                  </el-collapse-item>

                  <el-collapse-item name="compose" title="编排服务块（compose）">
                    <el-alert type="info" :closable="false"
                      title="2 空格缩进的服务块（可含多个服务）；运行期值用 ${VAR:-默认} 引用根 .env（console 同源），{{X}} 仅支持单元内置与打包时变量。支持图形/代码双模编辑。" style="margin-bottom: 8px" />
                    <ComposeBlockEditor
                      v-model="activeUnit.composeTemplate" :vars="buildVarList" height="260px"
                      :extra-services="unitExtraServices" :images="catalogImages" :env-vars="unitEnvVars"
                    />
                  </el-collapse-item>

                  <el-collapse-item name="conf" title="配置渲染（可选）">
                    <el-form label-width="130px" class="detail-form">
                      <el-row :gutter="10">
                        <el-col :span="12">
                          <el-form-item label="容器内配置目录">
                            <el-input v-model="activeUnit.confDir" class="mono" placeholder="/app/conf（空=不挂载配置）" />
                          </el-form-item>
                        </el-col>
                      </el-row>
                      <el-form-item label="配置消费 env">
                        <div class="kv-rows">
                          <div v-for="(r, i) in confEnvRows" :key="i" class="kv-row">
                            <el-input v-model="r.k" class="mono" placeholder="ENV_NAME" style="width: 260px" @change="rebuildConfEnv" />
                            <el-input v-model="r.v" class="mono" placeholder="file:/app/conf/" style="flex: 1" @change="rebuildConfEnv" />
                            <el-button link type="danger" @click="confEnvRows.splice(i, 1); rebuildConfEnv()">×</el-button>
                          </div>
                          <el-button size="small" icon="Plus" @click="confEnvRows.push({ k: '', v: '' })">添加</el-button>
                        </div>
                      </el-form-item>
                    </el-form>
                    <div class="section-label">配置模板（安装时渲染到 data/conf/application.yaml；图形树形编辑或文本 YAML 双视图；可用全部安装时变量与组件变量——说明见下方变量字典）</div>
                    <YamlSectionsEditor v-model="activeUnit.configTemplate" :vars="configVarList" height="220px"
                      placeholder="mse:&#10;  tenant-id: {{TENANT_ID}}" />
                  </el-collapse-item>
                </el-collapse>
              </el-tab-pane>

              <el-tab-pane :label="`变量（${(editApp.variables || []).length}）`" name="vars">
                <VarTable :rows="editApp.variables || []" editable @update:rows="editApp.variables = $event" />
                <el-divider content-position="left">组件提供的变量（勾选对应组件打包时自动生效，无需登记）</el-divider>
                <el-table :data="componentVars" border size="small">
                  <el-table-column label="变量" width="220">
                    <template #default="{ row }"><span class="mono ph">{{ ph(row.name) }}</span></template>
                  </el-table-column>
                  <el-table-column prop="kindLabel" label="类型" width="110" />
                  <el-table-column prop="origin" label="来源组件" min-width="140" />
                  <el-table-column prop="desc" label="行为" min-width="240" />
                </el-table>
              </el-tab-pane>

              <el-tab-pane label="编排" name="topo">
                <el-alert type="info" :closable="false" style="margin-bottom: 8px"
                  title="套件编排画布：拖线（单元服务 → 组件服务）= 写入 depends_on 启动依赖并自动勾选组件；点实线删除依赖；虚线为配置模板 {{X}} 占位符接线（只读展示，不自动改写配置）。改动需点上方「保存」生效。" />
                <OrchestrationCanvas
                  :units="editApp.units" :components="components" v-model:selected-codes="editApp.defaultComponents"
                  editable @connect="onCanvasConnect" @disconnect="onCanvasDisconnect"
                />
              </el-tab-pane>
            </el-tabs>
          </div>
        </div>
      </div>
    </el-drawer>

    <!-- 添加单元（编辑器内） -->
    <el-dialog v-model="addUnitDialog" title="添加服务单元" width="720px" append-to-body>
      <div class="tpl-grid">
        <div v-for="t in templates" :key="t.key" class="tpl-card" @click="editorAddUnit(t)">
          <div class="tpl-title"><el-icon><CirclePlus /></el-icon>{{ t.title }}</div>
          <div class="tpl-desc">{{ t.desc }}</div>
        </div>
      </div>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'
import {
  BUILTIN_VARS, ROLE_META, SOURCE_META, CATEGORY_META, scanPlaceholders, composeServiceNames,
  unitTemplates, nextUnitCode, validateAppClient
} from '../lib'
import { addDependency, removeDependency } from '../composeYaml'
import TemplateEditor from '../components/TemplateEditor.vue'
import YamlSectionsEditor from '../components/YamlSectionsEditor.vue'
import ComposeBlockEditor from '../components/ComposeBlockEditor.vue'
import VarTable from '../components/VarTable.vue'
import OrchestrationCanvas from '../components/OrchestrationCanvas.vue'

const apps = ref([])
const components = ref([])
const imageRefs = ref([])
const loading = ref(false)
const templates = unitTemplates()
// 镜像来源三选一卡片（SOURCE_META 权威术语）
const sourceOptions = Object.entries(SOURCE_META).map(([value, m]) => ({ value, ...m }))

// 文本节点里直接写 {{X}} 字面量会被 Vue 插值提前截断，经 script 函数输出
const ph = (n) => '{{' + n + '}}'

// ---- 列表 ----
const roleIcon = (r) => (ROLE_META[r] || ROLE_META.custom).icon
const roleColor = (r) => (ROLE_META[r] || ROLE_META.custom).color
const roleLabel = (r) => (ROLE_META[r] || ROLE_META.custom).label
const sourceLabel = (s) => (SOURCE_META[s] || {}).label || s || '未设置'

async function load() {
  loading.value = true
  try {
    const [a, c, imgs] = await Promise.all([api.get('/api/apps'), api.get('/api/components'), api.get('/api/images').catch(() => [])])
    apps.value = a || []
    components.value = c || []
    imageRefs.value = (imgs || []).map((i) => i.ref || i.fileName || '').filter(Boolean)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function removeApp(a) {
  await ElMessageBox.confirm(`删除应用 ${a.code}？`, '确认', { type: 'warning' })
  await api.del(`/api/apps/${a.code}`)
  ElMessage.success('已删除')
  await load()
}

// ---- 新建向导 ----
const wizard = ref(false)
const wStep = ref(0)
const creating = ref(false)
const wForm = ref({ code: '', name: '', defaultVersion: '1.0.0', remark: '' })
const wUnits = ref([])
const wComps = ref([])
const wVarCandidates = ref([])

function openWizard() {
  wizard.value = true
  wStep.value = 0
  wForm.value = { code: '', name: '', defaultVersion: '1.0.0', remark: '' }
  wUnits.value = []
  wComps.value = components.value.filter((c) => c.defaultChecked).map((c) => c.code)
  wVarCandidates.value = []
}
function wizardAddUnit(t) {
  const code = nextUnitCode(wUnits.value, t.unit.role)
  const u = JSON.parse(JSON.stringify({ ...t.unit, entrypoint: wUnits.value.length === 0 }))
  u.code = code
  u.composeTemplate = String(u.composeTemplate || '').replaceAll('{{CODE}}', code)
  wUnits.value.push(u)
}
function wMove(i, d) {
  const arr = wUnits.value
  ;[arr[i], arr[i + d]] = [arr[i + d], arr[i]]
}
function wNext() {
  if (wStep.value === 0 && !/^[a-z][a-z0-9-]*$/.test(wForm.value.code)) {
    return ElMessage.warning('应用编码须为小写字母数字短横线（字母开头）')
  }
  if (wStep.value === 1) {
    if (!wUnits.value.length) return ElMessage.warning('至少添加一个服务单元')
    // 扫描变量引用 → 变量候选
    const builtins = new Set(BUILTIN_VARS.map((v) => v.name))
    const names = scanPlaceholders(wUnits.value.flatMap((u) => [u.dockerfile, u.composeTemplate, u.configTemplate]))
      .filter((n) => !builtins.has(n))
    wVarCandidates.value = names.map((n) => ({ name: n, label: '', when: 'install', source: 'input', value: '', checked: true, sensitive: false }))
  }
  wStep.value++
}
async function createApp() {
  const app = {
    ...wForm.value,
    units: wUnits.value,
    variables: wVarCandidates.value.filter((r) => r.checked).map(({ checked, ...v }) => v),
    defaultComponents: wComps.value
  }
  const err = validateAppClient(app)
  if (err) return ElMessage.error(err)
  creating.value = true
  try {
    await api.post('/api/apps', app)
    ElMessage.success('应用已创建')
    wizard.value = false
    await load()
    openEditor(apps.value.find((a) => a.code === app.code) || app)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    creating.value = false
  }
}

// ---- 编辑器 ----
const editor = ref(false)
const editApp = ref(null)
const activeUnitIdx = ref(0)
const detailTab = ref('unit')
const openSections = ref(['base', 'source', 'compose', 'conf'])
const saving = ref(false)
const addUnitDialog = ref(false)
const confEnvRows = ref([])

const activeUnit = computed(() => editApp.value?.units[activeUnitIdx.value] || {})

function openEditor(a) {
  editApp.value = JSON.parse(JSON.stringify({
    code: a.code, name: a.name, defaultVersion: a.defaultVersion, units: a.units || [],
    variables: a.variables || [], defaultComponents: a.defaultComponents || [], remark: a.remark || ''
  }))
  if (!editApp.value.units.some((u) => u.entrypoint) && editApp.value.units.length) {
    editApp.value.units[0].entrypoint = true
  }
  activeUnitIdx.value = 0
  detailTab.value = 'unit'
  editor.value = true
  syncConfEnvRows()
}
watch(activeUnitIdx, syncConfEnvRows)
function syncConfEnvRows() {
  confEnvRows.value = Object.entries(activeUnit.value.confEnv || {}).map(([k, v]) => ({ k, v }))
}
function rebuildConfEnv() {
  const o = {}
  for (const r of confEnvRows.value) {
    if (r.k.trim()) o[r.k.trim()] = r.v
  }
  activeUnit.value.confEnv = o
}
function setEntrypoint(v) {
  for (const u of editApp.value.units) u.entrypoint = false
  activeUnit.value.entrypoint = !!v
}
function editMove(i, d) {
  const arr = editApp.value.units
  ;[arr[i], arr[i + d]] = [arr[i + d], arr[i]]
  activeUnitIdx.value = i + d
}
function editRemoveUnit(i) {
  editApp.value.units.splice(i, 1)
  if (activeUnitIdx.value >= editApp.value.units.length) activeUnitIdx.value = editApp.value.units.length - 1
}
function editorAddUnit(t) {
  const code = nextUnitCode(editApp.value.units, t.unit.role)
  const u = JSON.parse(JSON.stringify({ ...t.unit }))
  u.code = code
  u.composeTemplate = String(u.composeTemplate || '').replaceAll('{{CODE}}', code)
  editApp.value.units.push(u)
  addUnitDialog.value = false
  activeUnitIdx.value = editApp.value.units.length - 1
  syncConfEnvRows()
}
async function saveApp() {
  const err = validateAppClient(editApp.value)
  if (err) return ElMessage.error(err)
  saving.value = true
  try {
    await api.post('/api/apps', editApp.value)
    ElMessage.success('已保存')
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}

// ---- 变量上下文（模板编辑器的变量字典数据源） ----
const buildVarList = computed(() => [
  ...BUILTIN_VARS,
  ...(editApp.value?.variables || []).filter((v) => v.when === 'build').map((v) => ({
    name: v.name, label: v.label || v.name, origin: '应用 · 打包时', originType: 'app', when: 'build', sensitive: v.sensitive
  }))
])
const configVarList = computed(() => {
  const appVars = (editApp.value?.variables || []).map((v) => ({
    name: v.name, label: v.label || v.name, origin: '应用自定义', originType: 'app', when: v.when, sensitive: v.sensitive
  }))
  const compVars = componentVars.value.map((c) => ({
    name: c.name, label: c.label || c.desc, origin: c.origin, originType: 'component', when: 'install'
  }))
  return [...appVars, ...compVars]
})
const componentVars = computed(() => {
  const rows = []
  for (const c of components.value) {
    for (const ep of c.endpoints || []) {
      rows.push({ name: ep.placeholder, label: ep.label, kindLabel: '端点', origin: `组件 ${c.code}`,
        desc: `安装器回填 ${ep.from || 'url'}（服务 ${ep.service}:${ep.port}）` })
    }
    for (const cr of c.credentials || []) {
      rows.push({ name: cr.placeholder, label: cr.label, kindLabel: '凭据', origin: `组件 ${c.code}`,
        desc: `安装器${{ ask: '询问（可随机）', random: '随机生成', const: '取固定值' }[cr.generate || 'const']}${cr.env ? `，写 .env ${cr.env}` : ''}` })
    }
  }
  return rows
})
const groupedComponents = computed(() =>
  Object.entries(CATEGORY_META).map(([key, label]) => ({
    key, label, items: components.value.filter((c) => c.category === key)
  })).filter((g) => g.items.length)
)

// ---- 编排上下文（双模编排块编辑器补全 + 套件画布） ----
// 编排块补全目标：其他单元服务 + 默认依赖组件的服务（depends_on 跨块引用）
const unitExtraServices = computed(() => {
  if (!editApp.value) return []
  const out = []
  const active = activeUnit.value.code
  for (const u of editApp.value.units) {
    if (u.code === active) continue
    for (const s of composeServiceNames(u.composeTemplate)) out.push({ name: s, origin: `单元 ${u.code}` })
  }
  const def = editApp.value.defaultComponents || []
  for (const c of components.value.filter((x) => def.includes(x.code))) {
    for (const s of composeServiceNames(c.composeServices)) out.push({ name: s, origin: `组件 ${c.code}` })
  }
  return out
})
// 镜像补全：组件目录镜像 ∪ 镜像仓
const catalogImages = computed(() => [...new Set([
  ...components.value.flatMap((c) => c.images || []),
  ...imageRefs.value
])].filter(Boolean))
// ${env} 补全：默认依赖组件的凭据 env 名
const unitEnvVars = computed(() => {
  if (!editApp.value) return []
  const def = editApp.value.defaultComponents || []
  return [...new Set(components.value
    .filter((c) => def.includes(c.code))
    .flatMap((c) => (c.credentials || []).map((cr) => cr.env || cr.placeholder)))]
})

// 套件编排画布：拖线写 depends_on（手术式，保格式与 {{X}} 原样）
function onCanvasConnect({ unitCode, from, to }) {
  const u = editApp.value?.units.find((x) => x.code === unitCode)
  if (!u) return
  const next = addDependency(u.composeTemplate, from, to)
  if (next === null) return ElMessage.error(`单元 ${unitCode} 编排块解析失败，无法写入依赖`)
  u.composeTemplate = next
}
function onCanvasDisconnect({ unitCode, from, to }) {
  const u = editApp.value?.units.find((x) => x.code === unitCode)
  if (!u) return
  const next = removeDependency(u.composeTemplate, from, to)
  if (next !== null) u.composeTemplate = next
}

onMounted(load)
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
.app-card { cursor: pointer; margin-bottom: 10px; }
.app-card-head { display: flex; justify-content: space-between; align-items: center; }
.app-name { font-size: 14px; }
.app-units { margin: 8px 0 4px; display: flex; flex-wrap: wrap; gap: 5px; }
.unit-chip { display: inline-flex; align-items: center; gap: 3px; border: 1px solid; border-radius: 10px;
  padding: 1px 8px; font-size: 12px; color: var(--el-text-color-regular); font-family: Consolas, Monaco, monospace; }
.entry-flag { font-style: normal; color: var(--el-color-success); margin-left: 2px; font-size: 11px; }
.app-meta { color: var(--el-text-color-secondary); font-size: 12px; display: flex; gap: 12px; }
.app-remark { color: var(--el-text-color-secondary); font-size: 12px; margin-top: 6px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tpl-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; }
.tpl-card { border: 1px solid var(--el-border-color); border-radius: 8px; padding: 12px; cursor: pointer; }
.tpl-card:hover { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.tpl-title { font-weight: 600; display: flex; align-items: center; gap: 6px; margin-bottom: 6px; }
.tpl-desc { color: var(--el-text-color-secondary); font-size: 12px; line-height: 1.6; }
.unit-row { display: flex; align-items: center; gap: 8px; padding: 4px 8px; border-radius: 6px; margin-bottom: 4px;
  border: 1px solid var(--el-border-color-lighter); }
.unit-role-icon { flex: none; }
.empty-tip { color: var(--el-text-color-secondary); text-align: center; padding: 14px; font-size: 12px; }
.comp-group { margin-bottom: 8px; }
.comp-group-title { color: var(--el-text-color-secondary); font-size: 12px; margin: 4px 0; }
.comp-name { color: var(--el-text-color-secondary); margin-left: 4px; font-size: 12px; }
.ph { color: var(--el-color-primary); }
/* 编辑器 */
.editor { display: flex; flex-direction: column; height: 100%; }
.editor-top { display: flex; align-items: center; gap: 10px; padding-bottom: 10px;
  border-bottom: 1px solid var(--el-border-color-lighter); }
.editor-top-spacer { flex: 1; }
.editor-body { display: flex; gap: 12px; flex: 1; min-height: 0; margin-top: 10px; }
.unit-list { width: 250px; flex: none; display: flex; flex-direction: column; gap: 6px; }
.unit-list-title { color: var(--el-text-color-secondary); font-size: 12px; }
.unit-item { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border: 1px solid var(--el-border-color);
  border-radius: 8px; cursor: pointer; }
.unit-item.active { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.unit-item-main { flex: 1; min-width: 0; }
.unit-item-code { font-family: Consolas, Monaco, monospace; font-size: 13px; font-weight: 600;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.unit-entry { margin-left: 6px; }
.unit-item-sub { color: var(--el-text-color-secondary); font-size: 11px; }
.unit-item-ops { display: flex; }
.unit-add { border-style: dashed; width: 100%; }
.unit-detail { flex: 1; min-width: 0; overflow: auto; }
.detail-form { max-width: 1080px; }
.form-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-left: 8px; }
.sec-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-bottom: 8px; line-height: 1.6; }
.section-label { color: var(--el-text-color-secondary); font-size: 12px; margin: 10px 0 6px; }
.source-cards { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; margin-bottom: 12px; }
.source-card { border: 1px solid var(--el-border-color); border-radius: 8px; padding: 10px 12px; cursor: pointer; }
.source-card.active { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.source-title { font-weight: 600; margin-bottom: 4px; }
.source-hint { color: var(--el-text-color-secondary); font-size: 12px; line-height: 1.5; }
.kv-rows { display: flex; flex-direction: column; gap: 6px; }
.kv-row { display: flex; gap: 8px; align-items: center; }
</style>
