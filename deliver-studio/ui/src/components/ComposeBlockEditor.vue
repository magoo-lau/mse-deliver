<template>
  <div class="cbe">
    <div class="cbe-bar">
      <el-radio-group v-model="mode" size="small">
        <el-radio-button value="code">代码</el-radio-button>
        <el-radio-button value="graph" :disabled="!!parseError">图形</el-radio-button>
      </el-radio-group>
      <span class="cbe-hint">{{ hintText }}</span>
      <span v-if="parseError" class="cbe-err mono" :title="parseError">YAML 解析失败（{{ parseError }}）——修复后可用图形模式</span>
    </div>

    <!-- 代码模式 -->
    <CodeEditor
      v-if="mode === 'code'" v-model="localText" :completions="completions" :height="height"
      :placeholder="phPlaceholder"
    />

    <!-- 图形模式 -->
    <div v-else class="cbe-graph">
      <div class="cbe-canvas">
        <VueFlow
          :delete-key-code="null" :edges-updatable="false" :min-zoom="0.2" fit-view-on-init
          @connect="onConnect" @node-click="onNodeClick" @edge-click="onEdgeClick" @node-drag-stop="onDragStop"
        >
          <Background :gap="16" />
          <Controls />
          <MiniMap />
          <template #node-svc="{ id, data }">
            <div class="svc-node" :class="{ ext: data.external, sel: selected === id }">
              <Handle type="target" :position="Position.Left" />
              <div class="svc-head">
                <span class="svc-name mono">{{ data.name }}</span>
                <el-tag v-if="data.external" size="small" type="info">外部</el-tag>
              </div>
              <div v-if="data.image" class="svc-img mono">{{ data.image }}</div>
              <div v-if="data.ports && data.ports.length" class="svc-ports">
                <span v-for="p in data.ports" :key="p" class="svc-port mono">{{ p }}</span>
              </div>
              <div v-if="data.external && data.origin" class="svc-origin">{{ data.origin }}</div>
              <Handle type="source" :position="Position.Right" :connectable="!data.external" />
            </div>
          </template>
        </VueFlow>
        <div class="cbe-empty" v-if="!serviceNames.length">
          空编排块——先在代码模式写一个服务（或点右侧「添加服务」）
        </div>
      </div>

      <!-- 侧栏：服务列表 / 属性面板 -->
      <div class="cbe-side">
        <template v-if="selected && selectedBody">
          <div class="cbe-side-title">
            <span>服务 <b class="mono">{{ selected }}</b></span>
            <el-button link type="danger" @click="onDeleteService">删除服务</el-button>
          </div>
          <el-form label-width="76px" size="small" class="cbe-form">
            <el-form-item label="服务名">
              <el-input v-model="form.name" class="mono" @change="applyRename" />
            </el-form-item>
            <el-form-item label="镜像">
              <el-input v-model="form.image" class="mono" placeholder="mysql:8.4" @change="applyForm" />
            </el-form-item>
            <el-form-item label="重启策略">
              <el-select v-model="form.restart" allow-create @change="applyForm">
                <el-option v-for="r in ['unless-stopped', 'always', 'no', 'on-failure']" :key="r" :value="r" :label="r" />
              </el-select>
            </el-form-item>

            <el-form-item label="端口">
              <div class="rows">
                <div v-for="(p, i) in form.ports" :key="i" class="row">
                  <el-input v-model="form.ports[i]" class="mono" placeholder="3306:3306" @change="applyForm" />
                  <el-button link type="danger" @click="form.ports.splice(i, 1); applyForm()">×</el-button>
                </div>
                <el-button size="small" icon="Plus" @click="form.ports.push('')">加端口</el-button>
              </div>
            </el-form-item>

            <el-form-item label="环境变量">
              <div class="rows">
                <template v-if="form.envMode === 'list'">
                  <div v-for="(e, i) in form.envRows" :key="i" class="row">
                    <el-input v-model="form.envRows[i]" class="mono" placeholder="TZ=Asia/Shanghai" @change="applyForm" />
                    <el-button link type="danger" @click="form.envRows.splice(i, 1); applyForm()">×</el-button>
                  </div>
                  <el-button size="small" icon="Plus" @click="form.envRows.push('')">加变量（K=V）</el-button>
                </template>
                <template v-else>
                  <div v-for="(e, i) in form.envRows" :key="i" class="row">
                    <el-input v-model="e.k" class="mono" placeholder="TZ" style="width: 130px; flex: none" @change="applyForm" />
                    <el-input v-model="e.v" class="mono" placeholder="Asia/Shanghai" @change="applyForm" />
                    <el-button link type="danger" @click="form.envRows.splice(i, 1); applyForm()">×</el-button>
                  </div>
                  <el-button size="small" icon="Plus" @click="form.envRows.push({ k: '', v: '' })">加变量</el-button>
                </template>
              </div>
            </el-form-item>

            <el-form-item label="数据卷">
              <div class="rows">
                <div v-for="(v, i) in form.volumes" :key="i" class="row">
                  <el-input v-model="form.volumes[i]" class="mono" placeholder="./data/mysql:/var/lib/mysql" @change="applyForm" />
                  <el-button link type="danger" @click="form.volumes.splice(i, 1); applyForm()">×</el-button>
                </div>
                <el-button size="small" icon="Plus" @click="form.volumes.push('')">加数据卷</el-button>
              </div>
            </el-form-item>

            <el-form-item v-if="form.commandArray || form.commandText !== null" label="命令">
              <el-input
                v-if="form.commandArray" v-model="form.commandView" class="mono" readonly
                placeholder="（数组形态请在代码模式编辑）"
              />
              <el-input v-else v-model="form.commandText" type="textarea" :rows="2" class="mono" @change="applyForm" />
            </el-form-item>

            <el-form-item label="依赖">
              <div class="rows">
                <div v-for="d in form.depends" :key="d" class="row">
                  <span class="mono dep-name">{{ d }}</span>
                  <el-button link type="danger" @click="removeDep(d)">×</el-button>
                </div>
                <el-select v-model="form.depAdd" placeholder="添加依赖（depends_on）" size="small" clearable @change="addDep">
                  <el-option v-for="s in depCandidates" :key="s.name" :value="s.name" :label="s.name">
                    <span class="mono">{{ s.name }}</span>
                    <span class="opt-origin">{{ s.origin }}</span>
                  </el-option>
                </el-select>
              </div>
            </el-form-item>
          </el-form>
          <div class="cbe-side-hint">属性面板编辑会规范化重排该服务的 YAML 段（其余服务原样保留）；依赖增删为保格式文本手术。</div>
        </template>
        <template v-else>
          <div class="cbe-side-title">服务（{{ serviceNames.length }}）</div>
          <div v-for="n in serviceNames" :key="n" class="svc-list-item mono" @click="selected = n">
            {{ n }}<span class="svc-list-sub">{{ displayImage(n) }}</span>
          </div>
          <el-button class="cbe-add" icon="Plus" @click="addDialog = true">添加服务</el-button>
        </template>
        <el-button v-if="selected" class="cbe-add" icon="Back" @click="selected = ''">返回服务列表</el-button>
      </div>
    </div>

    <!-- 添加服务 -->
    <el-dialog v-model="addDialog" title="添加服务" width="420px" append-to-body>
      <el-form label-width="90px">
        <el-form-item label="服务名*">
          <el-input v-model="addName" class="mono" placeholder="如 mysql（字母数字短横线）" />
        </el-form-item>
        <el-form-item label="镜像">
          <el-input v-model="addImage" class="mono" placeholder="mysql:8.4（可后补）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="addDialog = false">取消</el-button>
        <el-button type="primary" @click="onAddService">添加</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
// 编排服务块双模编辑器：代码（CodeMirror+补全）⇄ 图形（@vue-flow 节点连线 = depends_on）
// compose 文本是唯一事实源：图形操作经 composeYaml 手术式写回，两端始终一致
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { VueFlow, useVueFlow, Handle, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import { MiniMap } from '@vue-flow/minimap'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import '@vue-flow/minimap/dist/style.css'
import CodeEditor from './CodeEditor.vue'
import { parseBlock, modelEdges, blockError, addDependency, removeDependency, renameService, updateService, addService, removeService } from '../composeYaml'
import { composeCompletion } from '../composeComplete'

const props = defineProps({
  modelValue: { type: String, default: '' },
  vars: { type: Array, default: () => [] },
  extraServices: { type: Array, default: () => [] }, // [{name, origin}] 块外可依赖服务（其他单元/组件）
  images: { type: Array, default: () => [] },
  envVars: { type: Array, default: () => [] },
  height: { type: String, default: '260px' }
})
const emit = defineEmits(['update:modelValue'])

const phPlaceholder = '  mysql:\n    image: mysql:8.4\n    depends_on:\n      - ...'

const mode = ref('code')
const selected = ref('')
const addDialog = ref(false)
const addName = ref('')
const addImage = ref('')
const positions = reactive({}) // 服务名 → {x,y} 手动拖动位置记忆（会话内）

// 提示文案经 script 输出（文本节点里直接写 {{X}} 字面量会被 Vue 插值截断）
const hintText = computed(() =>
  mode.value === 'code'
    ? '直接编辑 YAML，Ctrl+空格补全：键位 / depends_on 目标 / 镜像 / {{变量}} / ${env}'
    : '节点=服务，连线=depends_on 启动依赖；点节点编辑属性，从节点右侧圆点拖出连线到目标节点，点连线删除依赖'
)

const { setNodes, setEdges, fitView } = useVueFlow()

// ---- 模型（唯一事实源 = modelValue 文本） ----
const model = computed(() => parseBlock(props.modelValue))
const parseError = computed(() => {
  if (model.value) return ''
  return blockError(props.modelValue) || '结构不符合编排服务块形态（须为 2 空格缩进的服务名键）'
})
const serviceNames = computed(() => (model.value ? Object.keys(model.value) : []))
const edges = computed(() => (model.value ? modelEdges(model.value) : []))
const externalTargets = computed(() => {
  const names = new Set(serviceNames.value)
  const origin = new Map(props.extraServices.map((s) => [s.name, s.origin || '']))
  const out = []
  for (const e of edges.value) {
    if (!names.has(e.to) && !out.some((x) => x.name === e.to)) {
      out.push({ name: e.to, origin: origin.get(e.to) || '未登记的外部服务' })
    }
  }
  return out
})

const localText = ref(props.modelValue)
watch(() => props.modelValue, (v) => { if (localText.value !== v) localText.value = v })
watch(localText, (v) => emit('update:modelValue', v))

// ---- 补全（代码模式） ----
const completions = computed(() => [
  composeCompletion({
    services: () => serviceNames.value,
    extraServices: () => props.extraServices,
    images: () => props.images,
    vars: () => props.vars,
    envVars: () => props.envVars
  })
])

// ---- 图形模式：节点与连线（depends_on 拓扑分层布局 + 拖动位置记忆） ----
const graphNodes = computed(() => {
  if (!model.value) return []
  const names = serviceNames.value
  const inBlock = new Set(names)
  const depsOf = new Map(names.map((n) => [n, []]))
  for (const e of edges.value) {
    if (depsOf.has(e.from) && inBlock.has(e.to)) depsOf.get(e.from).push(e.to)
  }
  const depth = new Map()
  const calc = (n, seen = new Set()) => {
    if (depth.has(n)) return depth.get(n)
    if (seen.has(n)) return 0
    seen.add(n)
    const ds = depsOf.get(n) || []
    const d = ds.length ? Math.max(...ds.map((x) => calc(x, seen))) + 1 : 0
    depth.set(n, d)
    return d
  }
  names.forEach((n) => calc(n))
  const byDepth = new Map()
  for (const n of names) {
    const d = depth.get(n) || 0
    if (!byDepth.has(d)) byDepth.set(d, [])
    byDepth.get(d).push(n)
  }
  const nodes = []
  for (const [d, list] of byDepth) {
    list.forEach((n, i) => {
      const body = model.value[n] || {}
      nodes.push({
        id: n,
        type: 'svc',
        position: positions[n] || { x: 30 + d * 250, y: 36 + i * 112 },
        data: {
          name: n,
          image: typeof body.image === 'string' ? body.image : '',
          ports: Array.isArray(body.ports) ? body.ports.filter((p) => typeof p === 'string') : []
        }
      })
    })
  }
  const maxDepth = names.length ? Math.max(0, ...names.map((n) => depth.get(n) || 0)) : 0
  externalTargets.value.forEach((t, i) => {
    nodes.push({
      id: 'ext:' + t.name,
      type: 'svc',
      position: positions['ext:' + t.name] || { x: 30 + (maxDepth + 1) * 250, y: 36 + i * 112 },
      data: { name: t.name, external: true, origin: t.origin }
    })
  })
  return nodes
})
const graphEdges = computed(() =>
  edges.value.map((e) => ({
    id: `e:${e.from}->${e.to}`,
    source: e.from,
    target: serviceNames.value.includes(e.to) ? e.to : 'ext:' + e.to,
    markerEnd: MarkerType.ArrowClosed,
    style: { stroke: serviceNames.value.includes(e.to) ? 'var(--el-color-primary)' : 'var(--el-text-color-secondary)', strokeWidth: 1.6 }
  }))
)

// 图形模式挂载/模型变化时同步画布（imperative setNodes/setEdges，对齐 ai-ui FlowEditor 用法）
watch(
  [mode, graphNodes, graphEdges],
  () => {
    if (mode.value !== 'graph') return
    nextTick(() => {
      setNodes(graphNodes.value)
      setEdges(graphEdges.value)
      fitView({ padding: 0.12, duration: 200 })
    })
  },
  { immediate: true }
)

function onDragStop({ node }) {
  positions[node.id] = { ...node.position }
}

function onConnect({ source, target }) {
  if (!source || !target) return
  const toName = target.startsWith('ext:') ? target.slice(4) : target
  if (source === toName) return
  if (edges.value.some((e) => e.from === source && e.to === toName)) return
  const next = addDependency(props.modelValue, source, toName)
  if (next === null) return ElMessage.error(`服务 ${source} 不在编排块内`)
  emit('update:modelValue', next)
}

function onEdgeClick({ edge }) {
  const from = edge.source
  const toName = edge.target.startsWith('ext:') ? edge.target.slice(4) : edge.target
  const next = removeDependency(props.modelValue, from, toName)
  if (next !== null) emit('update:modelValue', next)
}

function onNodeClick({ node }) {
  selected.value = node.data.external ? '' : node.id
}

// ---- 属性面板 ----
const selectedBody = computed(() => (selected.value && model.value ? model.value[selected.value] : null))
const form = reactive({
  name: '', image: '', restart: '', ports: [], envMode: 'map', envRows: [], volumes: [],
  commandText: null, commandArray: false, commandView: '', depends: [], depAdd: ''
})
let applying = false

function syncForm() {
  const body = selectedBody.value
  if (!body) return
  form.name = selected.value
  form.image = typeof body.image === 'string' ? body.image : ''
  form.restart = typeof body.restart === 'string' ? body.restart : ''
  form.ports = Array.isArray(body.ports) ? [...body.ports] : []
  if (Array.isArray(body.environment)) {
    form.envMode = 'list'
    form.envRows = [...body.environment]
  } else if (body.environment && typeof body.environment === 'object') {
    form.envMode = 'map'
    form.envRows = Object.entries(body.environment).map(([k, v]) => ({ k, v: String(v) }))
  } else {
    form.envMode = 'map'
    form.envRows = []
  }
  form.volumes = Array.isArray(body.volumes) ? [...body.volumes] : []
  if (Array.isArray(body.command)) {
    form.commandArray = true
    form.commandView = JSON.stringify(body.command)
    form.commandText = null
  } else if (typeof body.command === 'string') {
    form.commandArray = false
    form.commandText = body.command
  } else {
    form.commandArray = false
    form.commandText = null
  }
  const deps = body.depends_on
  form.depends = Array.isArray(deps) ? [...deps] : deps && typeof deps === 'object' ? Object.keys(deps) : deps ? [deps] : []
  form.depAdd = ''
}

watch([selected, () => props.modelValue], () => {
  if (!applying) syncForm()
})

function buildBody() {
  const body = { ...(selectedBody.value || {}) }
  body.image = form.image || undefined
  body.restart = form.restart || undefined
  const ports = form.ports.filter((p) => String(p).trim())
  if (ports.length) body.ports = ports
  else delete body.ports
  if (form.envMode === 'list') {
    const rows = form.envRows.filter((x) => String(x).trim())
    if (rows.length) body.environment = rows
    else delete body.environment
  } else {
    const rows = form.envRows.filter((r) => r.k.trim())
    if (rows.length) {
      const o = {}
      for (const r of rows) o[r.k.trim()] = r.v
      body.environment = o
    } else delete body.environment
  }
  const vols = form.volumes.filter((v) => String(v).trim())
  if (vols.length) body.volumes = vols
  else delete body.volumes
  // 数组形态命令不在面板编辑（保留原值，代码模式改）
  if (!form.commandArray && form.commandText !== null) body.command = form.commandText
  return body
}

function applyForm() {
  if (!selected.value) return
  applying = true
  const next = updateService(props.modelValue, selected.value, buildBody())
  applying = false
  if (next === null) return ElMessage.error('属性写入失败')
  emit('update:modelValue', next)
}

function applyRename() {
  const next = String(form.name || '').trim()
  if (!next || next === selected.value) return
  if (!/^[A-Za-z0-9_.-]+$/.test(next)) {
    ElMessage.warning('服务名须为字母数字短横线形态')
    form.name = selected.value
    return
  }
  if (serviceNames.value.includes(next)) {
    ElMessage.warning(`服务名已存在: ${next}`)
    form.name = selected.value
    return
  }
  const out = renameService(props.modelValue, selected.value, next)
  if (out === null) return
  emit('update:modelValue', out)
  selected.value = next
}

function addDep(name) {
  if (!name) return
  const next = addDependency(props.modelValue, selected.value, name)
  form.depAdd = ''
  if (next === null) return
  emit('update:modelValue', next)
}
function removeDep(name) {
  const next = removeDependency(props.modelValue, selected.value, name)
  if (next !== null) emit('update:modelValue', next)
}

const depCandidates = computed(() => {
  const others = serviceNames.value.filter((n) => n !== selected.value).map((n) => ({ name: n, origin: '本块服务' }))
  const inBlock = new Set(serviceNames.value)
  const extra = props.extraServices.filter((s) => !inBlock.has(s.name)).map((s) => ({ name: s.name, origin: s.origin || '外部服务' }))
  return [...others, ...extra]
})

function displayImage(name) {
  const img = model.value?.[name]?.image
  return typeof img === 'string' ? img : ''
}

// ---- 添加 / 删除服务 ----
function onAddService() {
  const name = addName.value.trim()
  if (!/^[A-Za-z0-9_.-]+$/.test(name)) return ElMessage.warning('服务名须为字母数字短横线形态')
  if (serviceNames.value.includes(name)) return ElMessage.warning(`服务名已存在: ${name}`)
  const body = { restart: 'unless-stopped' }
  if (addImage.value.trim()) body.image = addImage.value.trim()
  emit('update:modelValue', addService(props.modelValue, name, body))
  addDialog.value = false
  addName.value = ''
  addImage.value = ''
  selected.value = name
}
async function onDeleteService() {
  try {
    await ElMessageBox.confirm(`删除服务 ${selected.value} 并级联清理依赖引用？`, '确认', { type: 'warning' })
  } catch { return }
  emit('update:modelValue', removeService(props.modelValue, selected.value))
  selected.value = ''
}
</script>

<style scoped>
.cbe-bar { display: flex; align-items: center; gap: 10px; margin-bottom: 6px; flex-wrap: wrap; }
.cbe-hint { color: var(--el-text-color-secondary); font-size: 12px; }
.cbe-err { color: var(--el-color-danger); font-size: 12px; }
.cbe-graph { display: flex; gap: 10px; }
.cbe-canvas { flex: 1; min-width: 0; height: 420px; border: 1px solid var(--el-border-color); border-radius: 4px;
  position: relative; background: var(--el-fill-color-blank); overflow: hidden; }
.cbe-empty { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center;
  color: var(--el-text-color-secondary); font-size: 12px; pointer-events: none; }
.cbe-side { width: 320px; flex: none; border: 1px solid var(--el-border-color-lighter); border-radius: 4px;
  padding: 10px; max-height: 420px; overflow: auto; }
.cbe-side-title { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px;
  font-size: 13px; }
.cbe-side-hint { color: var(--el-text-color-secondary); font-size: 11px; margin-top: 8px; line-height: 1.6; }
.cbe-form :deep(.el-form-item) { margin-bottom: 10px; }
.cbe-add { width: 100%; border-style: dashed; margin-top: 8px; }
.svc-list-item { padding: 5px 8px; border: 1px solid var(--el-border-color-lighter); border-radius: 4px;
  margin-bottom: 4px; cursor: pointer; font-size: 12px; display: flex; gap: 8px; align-items: center; }
.svc-list-item:hover { border-color: var(--el-color-primary); }
.svc-list-sub { color: var(--el-text-color-secondary); font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rows { display: flex; flex-direction: column; gap: 4px; width: 100%; }
.row { display: flex; gap: 4px; align-items: center; }
.row .el-input { flex: 1; }
.dep-name { flex: 1; font-size: 12px; }
.opt-origin { color: var(--el-text-color-secondary); font-size: 11px; margin-left: 8px; }
.mono { font-family: Consolas, Monaco, monospace; }
/* vue-flow 节点卡片 */
.svc-node {
  min-width: 150px; max-width: 230px; padding: 8px 10px; border-radius: 6px;
  border: 1.5px solid var(--el-color-primary); background: var(--el-bg-color);
  font-size: 12px; cursor: pointer; box-shadow: 0 1px 4px rgba(0, 0, 0, 0.08);
}
.svc-node.ext { border-color: var(--el-border-color); border-style: dashed; background: var(--el-fill-color-light); }
.svc-node.sel { border-width: 2px; box-shadow: 0 0 0 2px var(--el-color-primary-light-7); }
.svc-head { display: flex; align-items: center; gap: 6px; }
.svc-name { font-weight: 600; }
.svc-img { color: var(--el-text-color-secondary); font-size: 11px; margin-top: 2px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.svc-ports { display: flex; flex-wrap: wrap; gap: 3px; margin-top: 4px; }
.svc-port { border: 1px solid var(--el-border-color); border-radius: 3px; padding: 0 4px; font-size: 10px;
  color: var(--el-text-color-secondary); }
.svc-origin { color: var(--el-text-color-secondary); font-size: 10px; margin-top: 4px; }
</style>
