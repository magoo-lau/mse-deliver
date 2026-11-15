<template>
  <div>
    <el-card class="page-card" shadow="never">
      <template #header>
        <div class="card-head">
          <span>组件目录（第三方设施声明：勾选即随包编排——服务块 + 镜像导出 + 配置接线）</span>
          <div>
            <el-button icon="Download" @click="exportPreset">导出预设</el-button>
            <el-button icon="Upload" @click="importPick">导入预设</el-button>
            <el-button type="primary" icon="Plus" @click="openEdit()">新增组件</el-button>
            <input ref="importInput" type="file" accept=".json,.zip" style="display: none" @change="importPreset" />
          </div>
        </div>
      </template>
      <div class="cat-filter">
        <el-radio-group v-model="catFilter" size="small">
          <el-radio-button value="">全部</el-radio-button>
          <el-radio-button v-for="(label, key) in CATEGORY_META" :key="key" :value="key">{{ label }}</el-radio-button>
        </el-radio-group>
      </div>
      <el-table :data="filtered" border v-loading="loading" size="small">
        <el-table-column prop="code" label="编码" width="140" />
        <el-table-column prop="name" label="名称" width="170" />
        <el-table-column prop="category" label="分类" width="90">
          <template #default="{ row }">
            <el-tag :type="catTag(row.category)" size="small">{{ CATEGORY_META[row.category] || row.category }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="默认勾选" width="80" align="center">
          <template #default="{ row }">{{ row.defaultChecked ? '是' : '-' }}</template>
        </el-table-column>
        <el-table-column label="镜像" min-width="200">
          <template #default="{ row }"><span class="mono">{{ (row.images || []).join(', ') }}</span></template>
        </el-table-column>
        <el-table-column label="端点/凭据/配置段" width="130" align="center">
          <template #default="{ row }">
            {{ (row.endpoints || []).length }} / {{ (row.credentials || []).length }} / {{ Object.keys(row.appConfig || {}).length }}
          </template>
        </el-table-column>
        <el-table-column prop="remark" label="备注" min-width="140" show-overflow-tooltip />
        <el-table-column label="操作" width="170" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="primary" @click="copy(row)">复制</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- 编辑抽屉：分区折叠卡片 -->
    <el-drawer v-model="dialog" :title="form.id ? `编辑组件 ${form.code}` : '新增组件'" size="72%" :close-on-click-modal="false">
      <el-collapse v-model="openSections" class="comp-editor">
        <el-collapse-item name="base" title="基本信息">
          <div class="sec-hint">编码（kebab）全局唯一；分类仅用于展示分组；默认勾选 = 新建应用/打包向导的预选项。</div>
          <el-form label-width="120px">
            <el-row :gutter="10">
              <el-col :span="8"><el-form-item label="编码*"><el-input v-model="form.code" class="mono" :disabled="!!form.id" placeholder="如 mysql" /></el-form-item></el-col>
              <el-col :span="8"><el-form-item label="名称"><el-input v-model="form.name" placeholder="MySQL 8.4" /></el-form-item></el-col>
              <el-col :span="4">
                <el-form-item label="分类">
                  <el-select v-model="form.category">
                    <el-option v-for="(label, key) in CATEGORY_META" :key="key" :label="label" :value="key" />
                  </el-select>
                </el-form-item>
              </el-col>
              <el-col :span="4"><el-form-item label="默认勾选"><el-switch v-model="form.defaultChecked" /></el-form-item></el-col>
            </el-row>
            <el-form-item label="备注"><el-input v-model="form.remark" /></el-form-item>
          </el-form>
        </el-collapse-item>

        <el-collapse-item name="images" title="镜像清单">
          <div class="sec-hint">组件随包分发的镜像（repo/name:tag 形态）。打包时按「本机 docker → 工作台镜像仓 → 报缺」解析导出。</div>
          <div v-for="(img, i) in form.images" :key="i" class="dyn-row">
            <el-input v-model="form.images[i]" class="mono" placeholder="mysql:8.4" />
            <el-button icon="Delete" circle @click="form.images.splice(i, 1)" />
          </div>
          <el-button icon="Plus" @click="form.images.push('')">添加镜像</el-button>
        </el-collapse-item>

        <el-collapse-item name="compose" title="编排服务块（compose）">
          <div class="sec-hint">2 空格缩进的服务块（可含多个服务）。数据卷建议 bind 到 ./data/&lt;名&gt;（console/agent 自动建目录）；运行期值用 ${VAR:-默认} 引用根 .env（与凭据/变量同源）。支持图形/代码双模编辑（连线 = depends_on）。</div>
          <ComposeBlockEditor
            v-model="form.composeServices" :vars="compVars" height="300px"
            :extra-services="catalogServices" :images="catalogImages" :env-vars="compEnvVars"
          />
        </el-collapse-item>

        <el-collapse-item name="endpoints" title="端点映射（组件地址怎么告诉应用配置）">
          <div class="sec-hint">声明「变量 → 本组件服务地址」映射；deliver-console 按分配回填三态：同机 = compose 服务名 / 跨机 = 主机地址:发布端口 / 未落机（外部）= 表单手填。说明会展示在变量字典与 console 安装表单里。</div>
          <el-table :data="form.endpoints" border size="small">
            <el-table-column label="变量" min-width="140">
              <template #default="{ row }"><el-input v-model="row.placeholder" class="mono" placeholder="DB_HOST" /></template>
            </el-table-column>
            <el-table-column label="说明" min-width="120">
              <template #default="{ row }"><el-input v-model="row.label" placeholder="如：数据库地址" /></template>
            </el-table-column>
            <el-table-column label="服务名" min-width="110">
              <template #default="{ row }"><el-input v-model="row.service" class="mono" placeholder="mysql" /></template>
            </el-table-column>
            <el-table-column label="端口" width="80">
              <template #default="{ row }"><el-input v-model="row.port" placeholder="3306" /></template>
            </el-table-column>
            <el-table-column label="取值" width="110">
              <template #default="{ row }">
                <el-select v-model="row.from">
                  <el-option value="url" label="url（默认）" />
                  <el-option value="host" label="host" />
                  <el-option value="port" label="port" />
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="scheme" width="90">
              <template #default="{ row }"><el-input v-model="row.scheme" placeholder="http" /></template>
            </el-table-column>
            <el-table-column width="50" align="center">
              <template #default="{ $index }"><el-button icon="Delete" circle @click="form.endpoints.splice($index, 1)" /></template>
            </el-table-column>
          </el-table>
          <el-button icon="Plus" style="margin-top: 6px" @click="form.endpoints.push({ placeholder: '', label: '', service: '', port: '', from: 'url', scheme: '' })">添加端点</el-button>
        </el-collapse-item>

        <el-collapse-item name="credentials" title="凭据（console 生成/询问/固定）">
          <div class="sec-hint">声明组件需要的密码/账号类变量：ask = 安装表单询问（留空随机）、random = 随机、const = 固定值；值同源写根 .env 与应用配置渲染。说明会展示在变量字典与 console 安装表单里。</div>
          <el-table :data="form.credentials" border size="small">
            <el-table-column label="变量" min-width="150">
              <template #default="{ row }"><el-input v-model="row.placeholder" class="mono" placeholder="MYSQL_ROOT_PASSWORD" /></template>
            </el-table-column>
            <el-table-column label="说明" min-width="130">
              <template #default="{ row }"><el-input v-model="row.label" placeholder="如：root 密码" /></template>
            </el-table-column>
            <el-table-column label=".env 变量名" min-width="140">
              <template #default="{ row }"><el-input v-model="row.env" class="mono" placeholder="缺省=变量名" /></template>
            </el-table-column>
            <el-table-column label="生成方式" width="140">
              <template #default="{ row }">
                <el-select v-model="row.generate">
                  <el-option value="ask" label="安装时询问" />
                  <el-option value="random" label="随机生成" />
                  <el-option value="const" label="固定值" />
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="值 / 默认" min-width="120">
              <template #default="{ row }"><el-input v-model="row.value" /></template>
            </el-table-column>
            <el-table-column width="50" align="center">
              <template #default="{ $index }"><el-button icon="Delete" circle @click="form.credentials.splice($index, 1)" /></template>
            </el-table-column>
          </el-table>
          <el-button icon="Plus" style="margin-top: 6px" @click="form.credentials.push({ placeholder: '', label: '', env: '', generate: 'const', value: '' })">添加凭据</el-button>
        </el-collapse-item>

        <el-collapse-item name="appConfig" title="应用配置注入（勾选本组件时追加的配置段）">
          <div class="sec-hint">图形树形编辑或整段 YAML（文本视图）；打包时按 topKey 追加为独立 YAML 文档（--- 分隔），段内可用本组件的端点/凭据变量。</div>
          <YamlSectionsEditor v-model="appConfigText" :vars="compVars" height="200px" :block-rows="3"
            placeholder="spring:&#10;  elasticsearch:&#10;    uris: {{ES_URIS}}" />
        </el-collapse-item>

        <el-collapse-item name="files" title="编排附件（随包落位的文件）">
          <div class="sec-hint">如 broker.conf / schema 说明文件：声明包内相对路径与内容，打包时原样写入（可被服务块自挂载，如 initdb.d）。</div>
          <div v-for="(f, i) in form.files" :key="i" class="dyn-row" style="align-items: flex-start">
            <el-input v-model="f.path" class="mono" placeholder="deploy/rocketmq/broker.conf" style="width: 240px; flex: none" />
            <el-input v-model="f.content" type="textarea" :rows="3" class="mono" placeholder="文件内容" />
            <el-button icon="Delete" circle @click="form.files.splice(i, 1)" />
          </div>
          <el-button icon="Plus" @click="form.files.push({ path: '', content: '' })">添加附件</el-button>
        </el-collapse-item>
      </el-collapse>
      <div class="editor-footer">
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" :loading="saving" icon="Check" @click="save">保存</el-button>
      </div>
    </el-drawer>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, downloadFile } from '../api'
import { CATEGORY_META, splitYamlSections, joinYamlSections, composeServiceNames } from '../lib'
import TemplateEditor from '../components/TemplateEditor.vue'
import YamlSectionsEditor from '../components/YamlSectionsEditor.vue'
import ComposeBlockEditor from '../components/ComposeBlockEditor.vue'

const components = ref([])
const loading = ref(false)
const dialog = ref(false)
const saving = ref(false)
const importInput = ref(null)
const appConfigRows = ref([])
const catFilter = ref('')
const openSections = ref(['base', 'images', 'compose', 'endpoints'])

const blank = () => ({
  id: 0, code: '', name: '', category: 'middleware', defaultChecked: false,
  images: [], composeServices: '', files: [], appConfig: {},
  endpoints: [], credentials: [], remark: ''
})
const form = ref(blank())

const filtered = computed(() => catFilter.value ? components.value.filter((c) => c.category === catFilter.value) : components.value)
// 模板编辑器变量芯片：本组件声明的端点/凭据变量（实时联动，含说明）
const compVars = computed(() => [
  ...form.value.endpoints.map((e) => ({ name: e.placeholder, label: e.label || '端点', origin: '组件端点', when: 'install' })),
  ...form.value.credentials.map((c) => ({ name: c.placeholder, label: c.label || '凭据', origin: '组件凭据', when: 'install' }))
].filter((v) => v.name))

// ---- 双模编排块编辑器上下文 ----
// depends_on 跨块补全目标：其他组件的服务
const catalogServices = computed(() => {
  const out = []
  for (const c of components.value) {
    if (c.code === form.value.code) continue
    for (const s of composeServiceNames(c.composeServices)) out.push({ name: s, origin: `组件 ${c.code}` })
  }
  return out
})
const catalogImages = computed(() => [...new Set(components.value.flatMap((c) => c.images || []))].filter(Boolean))
// ${env} 补全：本组件凭据 env 名
const compEnvVars = computed(() => [...new Set((form.value.credentials || []).map((cr) => cr.env || cr.placeholder))])

// 应用配置注入双视图：appConfigRows(topKey+段) ⇄ 整段 YAML 文本
const appConfigText = computed({
  get: () => joinYamlSections('', appConfigRows.value.filter((r) => r.key || r.value).map((r) => ({ key: r.key, block: r.value }))),
  set: (v) => {
    appConfigRows.value = splitYamlSections(v).sections.map((s) => ({ key: s.key, value: s.block }))
  }
})

async function load() {
  loading.value = true
  try {
    components.value = (await api.get('/api/components')) || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
function openEdit(row) {
  form.value = row
    ? { ...row, images: [...(row.images || [])], endpoints: (row.endpoints || []).map((x) => ({ ...x })),
        credentials: (row.credentials || []).map((x) => ({ ...x })), files: (row.files || []).map((x) => ({ ...x })) }
    : blank()
  appConfigRows.value = Object.entries(row?.appConfig || {}).map(([key, value]) => ({ key, value }))
  openSections.value = ['base', 'images', 'compose', 'endpoints']
  dialog.value = true
}
async function copy(row) {
  const dup = JSON.parse(JSON.stringify(row))
  dup.code = row.code + '-copy'
  dup.name = (row.name || row.code) + ' 副本'
  dup.id = 0
  await api.post('/api/components', dup)
  ElMessage.success('已复制为 ' + dup.code)
  await load()
}
async function save() {
  if (!form.value.code) return ElMessage.warning('组件编码必填')
  form.value.appConfig = {}
  for (const s of appConfigRows.value) {
    if (s.key && s.value) form.value.appConfig[s.key] = s.value
  }
  saving.value = true
  try {
    await api.post('/api/components', form.value)
    ElMessage.success('已保存')
    dialog.value = false
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}
async function remove(row) {
  await ElMessageBox.confirm(`删除组件 ${row.code}？`, '确认', { type: 'warning' })
  await api.del(`/api/components/${row.code}`)
  ElMessage.success('已删除')
  await load()
}
function importPick() {
  importInput.value.click()
}
async function importPreset(ev) {
  const file = ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  let replace = false
  try {
    await ElMessageBox.confirm(
      '是 → 替换导入（先清空应用与组件再导入）；否 → 合并导入（按编码 upsert，保留未提及内容）',
      `导入 ${file.name}`,
      { confirmButtonText: '替换导入', cancelButtonText: '合并导入', distinguishCancelAndClose: true, type: 'warning' }
    )
    replace = true
  } catch (action) {
    if (action === 'close') return
  }
  try {
    const fd = new FormData()
    fd.append('file', file)
    const res = await api.upload('/api/import' + (replace ? '?replace=1' : ''), fd)
    const parts = [`应用 ${res.apps}`, `组件 ${res.components}`]
    if (res.settings) parts.push(`设置 ${res.settings}`)
    if (res.materials) parts.push(`制品 ${res.materials}`)
    if (res.images) parts.push(`镜像 ${res.images}`)
    if (res.skipped) parts.push(`跳过 ${res.skipped}`)
    ElMessage.success(`已导入：${parts.join('、')}`)
    await load()
  } catch (e) {
    ElMessage.error('导入失败：' + e.message)
  }
}
async function exportPreset() {
  await downloadFile('/api/export', 'components-preset.json')
  ElMessage.success('已导出（含应用定义与组件目录）')
}
function catTag(cat) {
  return { database: 'danger', cache: 'warning', middleware: 'primary', custom: 'info' }[cat] || 'info'
}
onMounted(load)
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
.cat-filter { margin-bottom: 10px; }
.sec-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-bottom: 8px; line-height: 1.6; }
.dyn-row { display: flex; gap: 6px; margin-bottom: 8px; align-items: center; }
.dyn-row .el-input, .dyn-row .el-textarea, .dyn-row > :first-child:not(.te) { flex: 1; }
.editor-footer { position: sticky; bottom: 0; padding: 10px 0; background: var(--el-bg-color);
  display: flex; justify-content: flex-end; gap: 8px; border-top: 1px solid var(--el-border-color-lighter); }
</style>
