<template>
  <div class="oc">
    <!-- 左侧：组件面板（编辑态） -->
    <div v-if="editable" class="oc-panel">
      <div class="oc-panel-title">组件目录（勾选随包编排）</div>
      <div v-for="g in groupedComponents" :key="g.key" class="oc-group">
        <div class="oc-group-title">{{ g.label }}</div>
        <label
          v-for="c in g.items" :key="c.code" class="oc-comp"
          :class="{ on: selectedSet.has(c.code), ghosted: ghostCodes.has(c.code) }"
        >
          <input type="checkbox" :checked="selectedSet.has(c.code)" @change="toggleComponent(c.code)" />
          <span class="oc-comp-code mono">{{ c.code }}</span>
          <span class="oc-comp-name">{{ c.name }}</span>
        </label>
      </div>
      <div v-if="!groupedComponents.length" class="oc-panel-empty">组件目录为空</div>
    </div>

    <!-- 画布 -->
    <div ref="canvasEl" class="oc-canvas">
      <VueFlow
        :delete-key-code="null" :edges-updatable="false" :nodes-connectable="editable"
        :min-zoom="0.15"
        @connect="onConnect" @edge-click="onEdgeClick" @node-click="onNodeClick"
        @nodes-initialized="onNodesReady"
      >
        <Background :gap="16" />
        <Controls />
        <MiniMap />
        <template #node-svc="{ id, data }">
          <div
            class="oc-node" :class="{ unit: data.kind === 'unit', comp: data.kind === 'comp', ghost: data.kind === 'ghost' }"
            :style="{ borderColor: data.color }"
          >
            <Handle type="target" :position="Position.Left" />
            <div class="oc-node-head">
              <span class="oc-node-name mono">{{ data.name }}</span>
              <el-tag v-if="data.entry" size="small" type="success">入口</el-tag>
              <el-tag v-if="data.kind === 'ghost'" size="small" type="danger">未勾选</el-tag>
            </div>
            <div class="oc-node-sub">{{ data.sub }}</div>
            <div v-if="data.image" class="oc-node-img mono">{{ data.image }}</div>
            <div v-if="data.kind === 'ghost' && editable" class="oc-node-ops">
              <el-button size="small" type="primary" link @click.stop="toggleComponent(data.compCode)">勾选该组件</el-button>
            </div>
            <Handle type="source" :position="Position.Right" :connectable="data.kind === 'unit'" />
          </div>
        </template>
      </VueFlow>
      <div class="oc-legend">
        <span>实线箭头 = depends_on 启动依赖（点连线删除）</span>
        <span>虚线 = 配置接线（配置模板引用组件 {{ '{{X}' + '}' }} 占位符，只读，点线看说明）</span>
        <span class="miss">红色虚线 = 引用了未勾选组件（打包会被校验拦截）</span>
      </div>
    </div>
  </div>
</template>

<script setup>
// 套件级编排画布：应用单元 + 已勾选组件 = 节点（compose 服务粒度），三类边：
//   实线 depends_on（跨块汇总）/ 虚线占位符接线（configTemplate 引用组件端点凭据，只读展示）/ 红虚线缺失告警
// 编辑态：拖线（单元服务 → 组件服务）= 写 depends_on 到该单元编排块 + 自动勾选组件；组件面板勾选随包
// 只读态：打包向导预览。compose 文本仍是唯一事实源——连线事件由父视图经 composeYaml 手术式写回
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { VueFlow, useVueFlow, Handle, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import { MiniMap } from '@vue-flow/minimap'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import '@vue-flow/minimap/dist/style.css'
import { ROLE_META, CATEGORY_META, composeServiceNames, parseComposeEdges, scanPlaceholders } from '../lib'

const props = defineProps({
  units: { type: Array, default: () => [] },
  components: { type: Array, default: () => [] }, // 全量组件目录
  selectedCodes: { type: Array, default: () => [] },
  editable: { type: Boolean, default: false }
})
const emit = defineEmits(['update:selectedCodes', 'connect', 'disconnect'])

const { setNodes, setEdges, fitView } = useVueFlow()
const selectedSet = computed(() => new Set(props.selectedCodes))

// 目录索引：服务名 → 组件（depends_on 悬空目标定位归属组件）
const svcOwner = computed(() => {
  const m = new Map()
  for (const c of props.components) {
    for (const s of composeServiceNames(c.composeServices)) m.set(s, c.code)
  }
  return m
})
// 目录索引：占位符 → {compCode, service}（端点/凭据）
const phOwner = computed(() => {
  const m = new Map()
  for (const c of props.components) {
    for (const ep of c.endpoints || []) m.set(ep.placeholder, { compCode: c.code, service: ep.service })
    for (const cr of c.credentials || []) m.set(cr.placeholder, { compCode: c.code, service: '' })
  }
  return m
})

// 占位符接线边数据（configTemplate 引用 → 组件），含未勾选（缺失）标记
const wiringEdges = computed(() => {
  const out = []
  for (const u of props.units || []) {
    const unitSvcs = composeServiceNames(u.composeTemplate)
    const fromSvc = unitSvcs[0] || u.code
    const refs = scanPlaceholders(u.configTemplate || '')
    const byComp = new Map()
    for (const r of refs) {
      const o = phOwner.value.get(r)
      if (!o) continue // 应用自有变量，非组件接线
      if (!byComp.has(o.compCode)) byComp.set(o.compCode, { placeholders: [], service: o.service })
      const g = byComp.get(o.compCode)
      g.placeholders.push(r)
      if (!g.service && o.service) g.service = o.service
    }
    for (const [compCode, g] of byComp) {
      out.push({ unitCode: u.code, fromSvc, compCode, toSvc: g.service, placeholders: g.placeholders, missing: !selectedSet.value.has(compCode) })
    }
  }
  return out
})

// depends_on 边（单元块 + 已勾选组件块；目标悬空时指向归属组件 ghost）
const depEdges = computed(() => {
  const out = []
  const svcNode = new Map() // 服务名 → 节点 id
  for (const u of props.units || []) {
    for (const s of composeServiceNames(u.composeTemplate)) svcNode.set(s, `u:${u.code}:${s}`)
  }
  for (const c of props.components.filter((x) => selectedSet.value.has(x.code))) {
    for (const s of composeServiceNames(c.composeServices)) svcNode.set(s, `c:${c.code}:${s}`)
  }
  const push = (from, to, ownerUnit, ownerComp) => {
    let target = svcNode.get(to)
    let ghostComp = ''
    if (!target) {
      const compCode = svcOwner.value.get(to)
      if (compCode) {
        ghostComp = compCode
        target = `ghost:${compCode}`
      } else return // 完全未知目标：引擎校验兜底，画布不展示
    }
    const owner = ownerUnit ? `u:${ownerUnit}` : `c:${ownerComp}`
    out.push({ from, to, source: svcNode.get(from), target, ownerUnit, ownerComp: ownerComp || '', owner, ghostComp })
  }
  for (const u of props.units || []) {
    for (const e of parseComposeEdges(u.composeTemplate)) push(e.from, e.to, u.code, '')
  }
  for (const c of props.components.filter((x) => selectedSet.value.has(x.code))) {
    for (const e of parseComposeEdges(c.composeServices)) push(e.from, e.to, '', c.code)
  }
  return out.filter((e) => e.source && e.target && e.source !== e.target)
})

// ghost 组件集合 = 被 depends_on 悬空引用或占位符引用的未勾选组件
const ghostCodes = computed(() => {
  const s = new Set()
  for (const e of depEdges.value) if (e.ghostComp) s.add(e.ghostComp)
  for (const w of wiringEdges.value) if (w.missing) s.add(w.compCode)
  return s
})

const groupedComponents = computed(() =>
  Object.entries(CATEGORY_META)
    .map(([key, label]) => ({ key, label, items: props.components.filter((c) => c.category === key) }))
    .filter((g) => g.items.length)
)

const COMP_COLOR = { database: '#f56c6c', cache: '#e6a23c', middleware: '#409eff', custom: '#909399' }

// 画布容器实测宽（列分布用）：页签隐藏时宽为 0，忽略过小的采样避免布局塌缩
const canvasEl = ref(null)
const canvasW = ref(1200)
let canvasRO = null
onMounted(() => {
  canvasRO = new ResizeObserver((entries) => {
    const w = entries[0]?.contentRect?.width || 0
    if (w >= 50) canvasW.value = w
  })
  if (canvasEl.value) canvasRO.observe(canvasEl.value)
})
onBeforeUnmount(() => canvasRO?.disconnect())

const graphNodes = computed(() => {
  const nodes = []
  const feUnits = (props.units || []).filter((u) => u.role === 'frontend')
  const svUnits = (props.units || []).filter((u) => u.role !== 'frontend')
  const selComps = props.components.filter((c) => selectedSet.value.has(c.code))
  // 非空列（前端单元 → 后端单元 → 组件 → 未勾选 ghost）按容器宽均匀铺开；列宽下限防窄画布重叠（fitView 兜底缩出）
  const cols = [
    { items: feUnits.map((u) => ({ kind: 'unit', unit: u })) },
    { items: svUnits.map((u) => ({ kind: 'unit', unit: u })) },
    { items: selComps.map((c) => ({ kind: 'comp', comp: c })) },
    { items: [...ghostCodes.value].map((code) => ({ kind: 'ghost', comp: props.components.find((c) => c.code === code) || { code, name: '' } })) }
  ].filter((c) => c.items.length)
  const pad = 30
  const nodeW = 240
  const step = Math.max(nodeW + 40, (canvasW.value - pad * 2 - nodeW) / Math.max(1, cols.length - 1))
  cols.forEach((c, i) => { c.x = pad + i * step })
  for (const col of cols) {
    let y = 40
    for (const item of col.items) {
      if (item.kind === 'unit') {
        const u = item.unit
        const svcs = composeServiceNames(u.composeTemplate)
        const list = svcs.length ? svcs : [u.code]
        for (const s of list) {
          const body = firstBodyOf(u.composeTemplate, s)
          nodes.push({
            id: `u:${u.code}:${s}`,
            type: 'svc',
            position: { x: col.x, y },
            data: {
              kind: 'unit', name: s, unitCode: u.code,
              sub: `${u.name || u.code} · ${(ROLE_META[u.role || 'custom'] || ROLE_META.custom).label}`,
              entry: !!u.entrypoint, color: (ROLE_META[u.role || 'custom'] || ROLE_META.custom).color,
              image: body
            }
          })
          y += 104
        }
      } else if (item.kind === 'comp') {
        const c = item.comp
        for (const s of composeServiceNames(c.composeServices)) {
          nodes.push({
            id: `c:${c.code}:${s}`,
            type: 'svc',
            position: { x: col.x, y },
            data: {
              kind: 'comp', name: s, compCode: c.code,
              sub: `组件 ${c.code} · ${CATEGORY_META[c.category] || c.category}`,
              color: COMP_COLOR[c.category] || COMP_COLOR.custom, image: ''
            }
          })
          y += 104
        }
        if (!composeServiceNames(c.composeServices).length) {
          nodes.push({ id: `c:${c.code}:${c.code}`, type: 'svc', position: { x: col.x, y }, data: { kind: 'comp', name: c.code, compCode: c.code, sub: `组件 ${c.code}（无服务块）`, color: COMP_COLOR[c.category] || COMP_COLOR.custom, image: '' } })
          y += 104
        }
      } else {
        const c = item.comp
        nodes.push({
          id: `ghost:${c.code}`,
          type: 'svc',
          position: { x: col.x, y },
          data: { kind: 'ghost', name: c.code, compCode: c.code, sub: '被引用但未勾选', color: '#f56c6c', image: '' }
        })
        y += 104
      }
    }
  }
  return nodes
})

// 取服务块内某服务的 image（首行展示用，文本正则轻取）
function firstBodyOf(block, svc) {
  const lines = String(block || '').split('\n')
  const idx = lines.findIndex((l) => l.trim() === `${svc}:` && l.startsWith('  '))
  if (idx < 0) return ''
  for (let i = idx + 1; i < lines.length; i++) {
    const m = lines[i].match(/^ {4}image:\s*(.+)$/)
    if (m) return m[1].trim().replace(/^['"]|['"]$/g, '')
    if (/^ {2}\S/.test(lines[i])) break
  }
  return ''
}

const graphEdges = computed(() => {
  const edges = []
  for (const e of depEdges.value) {
    edges.push({
      id: `dep:${e.from}->${e.to}`,
      source: e.source,
      target: e.target,
      markerEnd: MarkerType.ArrowClosed,
      style: { stroke: e.ghostComp ? '#f56c6c' : 'var(--el-color-primary)', strokeWidth: 1.6 },
      data: { kind: 'dep', owner: e.owner, ownerUnit: e.ownerUnit, from: e.from, to: e.to }
    })
  }
  for (const w of wiringEdges.value) {
    // 未勾选组件没有 c: 服务节点，接线统一指向其 ghost 节点——否则 vue-flow 因目标缺失静默丢边，红虚线告警画不出来
    const comp = props.components.find((c) => c.code === w.compCode)
    const compSvcs = composeServiceNames(comp?.composeServices || '')
    const toSvc = w.toSvc && compSvcs.includes(w.toSvc) ? w.toSvc : compSvcs[0]
    const target = w.missing ? `ghost:${w.compCode}` : `c:${w.compCode}:${toSvc || w.compCode}`
    const sourceNode = graphNodes.value.find((n) => n.id.startsWith(`u:${w.unitCode}:`) && n.data.name === w.fromSvc)
    const source = sourceNode ? sourceNode.id : (graphNodes.value.find((n) => n.id.startsWith(`u:${w.unitCode}:`)) || {}).id
    if (!source) continue
    if (!graphNodes.value.some((n) => n.id === target)) continue
    const label = w.placeholders.slice(0, 2).map((p) => `{{${p}}}`).join(' ') + (w.placeholders.length > 2 ? ' …' : '')
    edges.push({
      id: `wire:${w.unitCode}:${w.compCode}`,
      source,
      target,
      label,
      style: { stroke: w.missing ? '#f56c6c' : 'var(--el-text-color-secondary)', strokeWidth: 1.2, strokeDasharray: '5 4' },
      markerEnd: { type: MarkerType.ArrowClosed, color: w.missing ? '#f56c6c' : 'var(--el-text-color-secondary)' }
    })
  }
  return edges
})

watch(
  [graphNodes, graphEdges],
  () => {
    setNodes(graphNodes.value)
    setEdges(graphEdges.value)
  },
  { immediate: true, deep: false }
)

// 全量节点尺寸测量完成（含页签从隐藏转可见、节点集重建后）才 fitView——
// 直接在 watch 里调会在测量前空转（vue-flow 对无尺寸节点不参与适配），这是画布不铺满的根因
function onNodesReady() {
  fitView({ padding: 0.12, duration: 200 })
}

// ---- 交互 ----
function toggleComponent(code) {
  const next = new Set(props.selectedCodes)
  if (next.has(code)) next.delete(code)
  else next.add(code)
  emit('update:selectedCodes', [...next])
}

async function onNodeClick({ node }) {
  if (!props.editable || node.data.kind !== 'ghost') return
  try {
    await ElMessageBox.confirm(`勾选组件 ${node.data.compCode}？（当前配置引用了它但未随包编排）`, '勾选组件', { type: 'warning' })
  } catch { return }
  toggleComponent(node.data.compCode)
}

function onConnect({ source, target }) {
  if (!props.editable || !source || !target) return
  if (!source.startsWith('u:')) {
    ElMessage.warning('连线起点须为应用单元服务（组件内部依赖请在「组件目录」编辑）')
    return
  }
  const unitCode = source.split(':')[1]
  const fromSvc = source.split(':').slice(2).join(':')
  let toSvc = ''
  if (target.startsWith('u:')) {
    toSvc = target.split(':').slice(2).join(':')
  } else if (target.startsWith('c:')) {
    const compCode = target.split(':')[1]
    toSvc = target.split(':').slice(2).join(':')
    if (!selectedSet.value.has(compCode)) toggleComponent(compCode)
  } else if (target.startsWith('ghost:')) {
    const compCode = target.split(':')[1]
    const comp = props.components.find((c) => c.code === compCode)
    toSvc = composeServiceNames(comp?.composeServices || '')[0] || compCode
    if (!selectedSet.value.has(compCode)) toggleComponent(compCode)
  } else return
  if (!toSvc || fromSvc === toSvc) return
  emit('connect', { unitCode, from: fromSvc, to: toSvc })
}

function onEdgeClick({ edge }) {
  if (!props.editable) return
  const d = edge.data
  if (d && d.kind === 'dep') {
    if (!d.ownerUnit) {
      ElMessage.warning('组件内部依赖请在「组件目录」中编辑该组件')
      return
    }
    emit('disconnect', { unitCode: d.ownerUnit, from: d.from, to: d.to })
    return
  }
  if (String(edge.id || '').startsWith('wire:')) {
    ElMessage.info('虚线为配置接线的只读投影：配置模板 {{X}} 占位符 → 组件。请在「单元详情 → 配置渲染 → 配置模板」中增删占位符来调整')
  }
}
</script>

<style scoped>
.oc { display: flex; gap: 10px; }
.oc-panel { width: 210px; flex: none; border: 1px solid var(--el-border-color-lighter); border-radius: 4px;
  padding: 8px; height: calc(100vh - 340px); min-height: 480px; overflow: auto; }
.oc-panel-title { color: var(--el-text-color-secondary); font-size: 12px; margin-bottom: 6px; }
.oc-panel-empty { color: var(--el-text-color-secondary); font-size: 12px; padding: 8px 0; }
.oc-group { margin-bottom: 8px; }
.oc-group-title { color: var(--el-text-color-secondary); font-size: 11px; margin: 4px 0 2px; }
.oc-comp { display: flex; align-items: center; gap: 6px; padding: 3px 6px; border-radius: 4px; cursor: pointer; font-size: 12px; }
.oc-comp:hover { background: var(--el-fill-color-light); }
.oc-comp.on { background: var(--el-color-primary-light-9); }
.oc-comp.ghosted .oc-comp-code { color: var(--el-color-danger); }
.oc-comp-code { font-size: 12px; }
.oc-comp-name { color: var(--el-text-color-secondary); font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1; }
.oc-canvas { flex: 1; min-width: 0; height: calc(100vh - 340px); min-height: 480px;
  border: 1px solid var(--el-border-color); border-radius: 4px;
  position: relative; background: var(--el-fill-color-blank); overflow: hidden; }
.oc-legend { position: absolute; left: 8px; bottom: 8px; display: flex; flex-wrap: wrap; gap: 12px;
  font-size: 11px; color: var(--el-text-color-secondary); background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter); border-radius: 4px; padding: 3px 8px; max-width: calc(100% - 60px); }
.oc-legend .miss { color: var(--el-color-danger); }
.oc-node {
  min-width: 160px; max-width: 240px; padding: 8px 10px; border-radius: 6px;
  border: 1.5px solid; background: var(--el-bg-color); font-size: 12px;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.08);
}
.oc-node.ghost { border-style: dashed; background: var(--el-fill-color-light); }
.oc-node-head { display: flex; align-items: center; gap: 6px; }
.oc-node-name { font-weight: 600; }
.oc-node-sub { color: var(--el-text-color-secondary); font-size: 11px; margin-top: 2px; }
.oc-node-img { color: var(--el-text-color-secondary); font-size: 10px; margin-top: 2px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.oc-node-ops { margin-top: 4px; }
.mono { font-family: Consolas, Monaco, monospace; }
</style>
