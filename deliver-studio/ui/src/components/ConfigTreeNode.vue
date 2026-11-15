<template>
  <!-- 对象分支：折叠/展开 + 键名可改 + 加子项/删除；子节点递归 -->
  <div v-if="isObj" class="ctn-node ctn-branch">
    <div class="ctn-row">
      <el-icon class="ctn-caret" :class="{ open: expanded }" @click="expanded = !expanded"><CaretRight /></el-icon>
      <input v-model="keyName" class="ctn-key mono" :class="{ danger: keyError }"
             spellcheck="false" @blur="commitKey" @keyup.enter="commitKey" />
      <span class="ctn-tag">{{ childCount }} 项</span>
      <span class="ctn-acts">
        <span class="ctn-act" title="加子项" @click.stop="adding = !adding">＋</span>
        <span class="ctn-act danger" title="删除本键（含子树）" @click.stop="removeMe">✕</span>
      </span>
    </div>
    <!-- 加子项行内表单：键名 + 初始值（空 = 空对象，可继续嵌套） -->
    <div v-if="adding" class="ctn-add">
      <input v-model="addKey" class="ctn-key mono" :class="{ danger: addKeyError }" placeholder="新键名" spellcheck="false" />
      <input v-model="addVal" class="ctn-val mono" placeholder="初始值（留空 = 空对象）" spellcheck="false"
             @keyup.enter="commitAdd" />
      <el-button size="small" type="primary" @click="commitAdd">确定</el-button>
      <el-button size="small" text @click="adding = false">取消</el-button>
    </div>
    <div v-show="expanded" class="ctn-children">
      <div v-if="!childCount" class="ctn-empty">（空对象——点「＋」加子项）</div>
      <ConfigTreeNode v-for="ck in childKeys" :key="ck" :holder="node" :k="ck" :vars="vars" :depth="depth + 1" />
    </div>
  </div>

  <!-- 数组叶子：原始 YAML 块（重排 + 校验，非法标红不回写） -->
  <div v-else-if="isArr" class="ctn-node">
    <div class="ctn-row">
      <span class="ctn-dot">▅</span>
      <input v-model="keyName" class="ctn-key mono" :class="{ danger: keyError }"
             spellcheck="false" @blur="commitKey" @keyup.enter="commitKey" />
      <span class="ctn-tag warn">列表</span>
      <span class="ctn-acts">
        <span class="ctn-act danger" title="删除本键" @click.stop="removeMe">✕</span>
      </span>
    </div>
    <textarea v-model="arrText" class="ctn-arr mono" :class="{ danger: arrError }"
              :rows="Math.min(8, Math.max(3, arrText.split('\n').length))"
              spellcheck="false" @change="commitArr" @blur="commitArr" />
    <div v-if="arrError" class="ctn-err">列表块不是合法 YAML 数组（修正后生效）</div>
  </div>

  <!-- 标量叶子：键名 + 值输入 + 变量芯片（识别 {{X}}）+ 光标处插入变量 -->
  <div v-else class="ctn-node">
    <div class="ctn-row">
      <span class="ctn-dot">·</span>
      <input v-model="keyName" class="ctn-key mono" :class="{ danger: keyError }"
             spellcheck="false" @blur="commitKey" @keyup.enter="commitKey" />
      <input ref="valEl" :model-value="scalarText" class="ctn-val mono" spellcheck="false"
             placeholder="值" @focus="focused = true" @update:model-value="setScalar" />
      <el-select v-if="vars.length" v-model="insertSel" class="ctn-insert" size="small" filterable
                 placeholder="＋变量" @change="insertVar">
        <el-option v-for="v in vars" :key="v.name" :value="v.name"
                   :label="v.label ? `${v.name}（${v.label}）` : v.name" />
      </el-select>
      <span class="ctn-acts">
        <span class="ctn-act danger" title="删除本键" @click.stop="removeMe">✕</span>
      </span>
    </div>
    <div v-if="usedVars.length" class="ctn-vars">
      <span v-for="uv in usedVars" :key="uv.name" class="ctn-chip" :class="{ unknown: !uv.known }"
            :title="uv.known ? (uv.label || uv.name) : '未在变量清单登记（打包时会被拦截，请先登记或改拼法）'">
        {{ uv.name }}{{ uv.known ? '' : ' ？' }}
      </span>
    </div>
  </div>
</template>

<script setup>
// 递归树节点编辑器：直接原地改 holder[k]（holder = 父对象，reactive 由上层持有）；
// 键名重命名 = 移键（保持兄弟顺序：重建同层对象）；标量/数组/对象三态按值类型分派。
import { computed, ref } from 'vue'
import { parseArray, dumpValue } from '../yamlDoc'

const props = defineProps({
  holder: { type: Object, required: true }, // 父对象（本节点 = holder[k]）
  k: { type: String, required: true },
  vars: { type: Array, default: () => [] },
  depth: { type: Number, default: 0 }
})

const node = computed(() => props.holder[props.k])
const isObj = computed(() => node.value != null && typeof node.value === 'object' && !Array.isArray(node.value))
const isArr = computed(() => Array.isArray(node.value))
const expanded = ref(props.depth < 2)
const adding = ref(false)
const focused = ref(false)

const childKeys = computed(() => (isObj.value ? Object.keys(node.value) : []))
const childCount = computed(() => childKeys.value.length)

// ---- 键名重命名（同级唯一；顺序保持：重建同层对象） ----
const keyName = ref(props.k)
const keyError = ref(false)
function commitKey() {
  const nk = keyName.value.trim()
  if (nk === props.k) {
    keyError.value = false
    return
  }
  if (!nk || Object.prototype.hasOwnProperty.call(props.holder, nk)) {
    keyError.value = true
    keyName.value = props.k
    return
  }
  // 同层移键保持兄弟顺序：整层重建（holder 为 reactive 代理，delete+assign 保响应性）
  const rebuilt = {}
  for (const key of Object.keys(props.holder)) {
    rebuilt[key === props.k ? nk : key] = props.holder[key]
  }
  for (const key of Object.keys(props.holder)) delete props.holder[key]
  Object.assign(props.holder, rebuilt)
  keyError.value = false
}

// ---- 加子项（对象分支） ----
const addKey = ref('')
const addVal = ref('')
const addKeyError = ref(false)
function commitAdd() {
  const nk = addKey.value.trim()
  if (!nk || Object.prototype.hasOwnProperty.call(node.value, nk)) {
    addKeyError.value = true
    return
  }
  const v = addVal.value
  node.value[nk] = v === '' ? {} : castScalar(v)
  addKeyError.value = false
  addKey.value = ''
  addVal.value = ''
  adding.value = false
  expanded.value = true
}
// 值输入的轻量类型推断（保持 YAML 语义；显式字符串可用引号包裹）
function castScalar(raw) {
  if (raw === 'true') return true
  if (raw === 'false') return false
  if (raw === 'null' || raw === '~') return null
  if (/^-?\d+$/.test(raw)) return parseInt(raw, 10)
  if (/^-?\d+\.\d+$/.test(raw)) return parseFloat(raw)
  return raw.replace(/^"(.*)"$/, '$1').replace(/^'(.*)'$/, '$1')
}

// ---- 标量叶子 ----
const valEl = ref()
const scalarText = computed(() => (node.value == null ? '' : String(node.value)))
function setScalar(v) {
  props.holder[props.k] = v
}

// 值中出现的 {{X}} 变量 → 芯片（已知带说明 / 未知警示）
const usedVars = computed(() => {
  const m = String(scalarText.value).matchAll(/\{\{(\w+)\}\}/g)
  const out = []
  const seen = new Set()
  for (const it of m) {
    if (seen.has(it[1])) continue
    seen.add(it[1])
    const known = props.vars.find((v) => v.name === it[1])
    out.push({ name: it[1], known: !!known, label: known?.label })
  }
  return out
})

// ＋变量下拉：在最近聚焦的值输入框光标处插入 {{X}}
const insertSel = ref('')
function insertVar(name) {
  const cur = String(scalarText.value)
  const el = valEl.value
  let pos = cur.length
  if (el && focused.value && typeof el.selectionStart === 'number') pos = el.selectionStart
  props.holder[props.k] = cur.slice(0, pos) + '{{' + name + '}}' + cur.slice(pos)
  insertSel.value = ''
}

// ---- 数组叶子 ----
const arrText = ref(isArr.value ? dumpValue(node.value) : '')
const arrError = ref(false)
function commitArr() {
  const parsed = parseArray(arrText.value)
  if (!parsed) {
    arrError.value = true
    return
  }
  arrError.value = false
  props.holder[props.k] = parsed
  arrText.value = dumpValue(parsed)
}

// ---- 删除本键 ----
function removeMe() {
  delete props.holder[props.k]
}
</script>

<style scoped>
.ctn-node { margin-left: 0; }
.ctn-children { margin-left: 18px; padding-left: 8px; border-left: 1px dashed var(--el-border-color-lighter); }
.ctn-row { display: flex; align-items: center; gap: 6px; margin: 3px 0; min-height: 26px; }
.ctn-caret { cursor: pointer; color: var(--el-text-color-secondary); transition: transform .15s; flex: none; }
.ctn-caret.open { transform: rotate(90deg); }
.ctn-dot { color: var(--el-text-color-placeholder); flex: none; font-size: 10px; width: 14px; text-align: center; }
.ctn-key { width: 170px; flex: none; border: 1px solid transparent; border-radius: 4px; padding: 2px 6px;
  font-size: 12px; background: transparent; color: var(--el-text-color-primary); }
.ctn-key:hover { border-color: var(--el-border-color-lighter); background: var(--el-fill-color-light); }
.ctn-key:focus { outline: none; border-color: var(--el-color-primary); background: var(--el-bg-color); }
.ctn-key.danger { border-color: var(--el-color-danger); }
.ctn-val { flex: 1; min-width: 120px; border: 1px solid var(--el-border-color-lighter); border-radius: 4px;
  padding: 2px 6px; font-size: 12px; background: var(--el-bg-color); color: var(--el-text-color-primary); }
.ctn-val:focus { outline: none; border-color: var(--el-color-primary); }
.ctn-tag { color: var(--el-text-color-secondary); font-size: 11px; flex: none; }
.ctn-tag.warn { color: var(--el-color-warning); }
.ctn-acts { display: none; gap: 4px; flex: none; }
.ctn-row:hover .ctn-acts { display: inline-flex; }
.ctn-act { cursor: pointer; font-size: 13px; color: var(--el-text-color-secondary); width: 18px; text-align: center; }
.ctn-act:hover { color: var(--el-color-primary); }
.ctn-act.danger:hover { color: var(--el-color-danger); }
.ctn-add { display: flex; align-items: center; gap: 6px; margin: 4px 0 4px 40px; }
.ctn-add .ctn-val { flex: 1; }
.ctn-children .ctn-add { margin-left: 22px; }
.ctn-empty { color: var(--el-text-color-placeholder); font-size: 12px; margin: 2px 0 2px 40px; }
.ctn-insert { width: 150px; flex: none; }
.ctn-vars { margin: 1px 0 1px 190px; display: flex; gap: 4px; flex-wrap: wrap; }
.ctn-chip { font-size: 11px; padding: 0 6px; border-radius: 8px; line-height: 18px;
  background: var(--el-color-primary-light-9); color: var(--el-color-primary); }
.ctn-chip.unknown { background: var(--el-color-warning-light-9); color: var(--el-color-warning); }
.ctn-arr { width: calc(100% - 190px); margin-left: 190px; border: 1px solid var(--el-border-color-lighter);
  border-radius: 4px; padding: 4px 6px; font-size: 12px; background: var(--el-bg-color);
  color: var(--el-text-color-primary); resize: vertical; }
.ctn-arr:focus { outline: none; border-color: var(--el-color-primary); }
.ctn-arr.danger { border-color: var(--el-color-danger); }
.ctn-err { margin-left: 190px; color: var(--el-color-danger); font-size: 11px; }
.mono { font-family: Consolas, Monaco, monospace; }
</style>
