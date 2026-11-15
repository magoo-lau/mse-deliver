<template>
  <div class="yse">
    <div class="yse-bar">
      <el-radio-group v-model="view" size="small" @change="onViewChange">
        <el-radio-button value="graph">图形</el-radio-button>
        <el-radio-button value="text">文本</el-radio-button>
      </el-radio-group>
      <span class="yse-hint">{{ view === 'graph' ? hints.graph : hints.text }}</span>
    </div>

    <!-- 图形视图：递归树编辑（解析失败自动落回文本视图并提示） -->
    <div v-if="view === 'graph'" class="yse-graph" :style="{ maxHeight: height, overflow: 'auto' }">
      <div v-if="!rootKeys.length" class="yse-empty">
        （空配置——点「＋」添加第一个顶层键）
        <el-button size="small" icon="Plus" @click="addRoot">加配置项</el-button>
      </div>
      <ConfigTreeNode v-for="k in rootKeys" :key="k" :holder="model" :k="k" :vars="vars" :depth="0" />
    </div>

    <!-- 文本视图：高亮模板编辑器 -->
    <TemplateEditor v-else v-model="text" :vars="vars" :height="height" :placeholder="placeholder" />

    <el-alert v-if="graphFallback" type="warning" :closable="false" class="yse-warn"
              title="当前配置无法图形化（YAML 不合法 / 多文档 / 含仅文本可表达的结构），已切到文本视图" />
  </div>
</template>

<script setup>
// 配置模板编辑器（图形 | 文本 双视图）：
//   图形 = 树形表单（YamlDoc 解析为对象树，节点级编辑值/键/增删，变量芯片与光标处插入）；
//   文本 = TemplateEditor 高亮编辑。模型始终是 YAML 字符串（调用方零感知）；
//   解析失败（含多文档/注释以外的语法错）自动落文本视图。原「表格视图」（顶层键+缩进文本块）已废弃。
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { parseDoc, dumpDoc } from '../yamlDoc'
import TemplateEditor from './TemplateEditor.vue'
import ConfigTreeNode from './ConfigTreeNode.vue'

const props = defineProps({
  modelValue: { type: String, default: '' },
  vars: { type: Array, default: () => [] },
  height: { type: String, default: '220px' },
  placeholder: { type: String, default: '' }
})
const emit = defineEmits(['update:modelValue'])

// 模板插值里的字面 {{X}} 会被 Vue 当插值起点——提示文案经 script 拼接（Guide.vue ph() 同款约束）
const PH_X = '{{' + 'X}}'
const hints = {
  graph: `树形表单编辑配置项；值中 ${PH_X} 显示为变量芯片（＋变量在光标处插入）。图形视图会规范化重排格式（注释会丢失）`,
  text: '直接编辑 YAML，变量高亮并校验（复杂结构/注释请用本视图）'
}

const view = ref('text')
const text = ref(props.modelValue)
const graphFallback = ref(false)
const model = reactive({})
let syncing = false // 图形→emit 回写期间忽略外部 modelValue 回流（防自激循环）

// 顶层键同步派生（重命名/增删即时反映——防抖序列化窗口内 props.k 不得失配）
const rootKeys = computed(() => Object.keys(model))

watch(() => props.modelValue, (v) => {
  if (view.value === 'text' && !syncing) text.value = v
})

// 进入视图时切换数据形态；图形视图解析失败 → 告警并回落文本
function onViewChange(v) {
  if (v === 'graph') {
    const parsed = parseDoc(props.modelValue)
    if (!parsed) {
      graphFallback.value = true
      view.value = 'text'
      return
    }
    graphFallback.value = false
    reloadModel(parsed)
  } else {
    const next = dumpDoc(model)
    text.value = next
    emit('update:modelValue', next)
  }
}

function reloadModel(parsed) {
  for (const k of Object.keys(model)) delete model[k]
  Object.assign(model, parsed)
}

// 图形编辑（树内原地改 reactive model）→ 防抖序列化回文本模型
let timer
watch(model, () => {
  clearTimeout(timer)
  timer = setTimeout(() => {
    syncing = true
    emit('update:modelValue', dumpDoc(model))
    nextTick(() => { syncing = false })
  }, 250)
}, { deep: true })

// 空态「加配置项」：顶层加一个键（重名自动后缀）
function addRoot() {
  let name = 'key'
  let i = 1
  while (Object.prototype.hasOwnProperty.call(model, name)) name = 'key-' + ++i
  model[name] = {}
}

// 初始视图：能图形化则缺省图形
const initial = parseDoc(props.modelValue)
if (initial) {
  reloadModel(initial)
  view.value = 'graph'
} else {
  graphFallback.value = !!props.modelValue.trim()
}

watch(text, (v) => { if (!syncing) emit('update:modelValue', v) })
</script>

<style scoped>
.yse-bar { display: flex; align-items: center; gap: 10px; margin-bottom: 6px; }
.yse-hint { color: var(--el-text-color-secondary); font-size: 12px; }
.yse-graph { border: 1px solid var(--el-border-color-lighter); border-radius: 4px;
  padding: 8px 10px; background: var(--el-bg-color); }
.yse-empty { color: var(--el-text-color-placeholder); font-size: 12px; display: flex;
  align-items: center; gap: 10px; padding: 6px 0; }
.yse-warn { margin-top: 6px; }
</style>
