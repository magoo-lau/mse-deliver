<template>
  <div ref="hostRef" class="ce-host" :style="{ height }"></div>
</template>

<script setup>
// CodeMirror 6 通用封装（YAML）：语法高亮 / 行号折叠 / 动态补全源 / 亮暗主题跟随 Element Plus
// 移植自 ai-ui JsonCodeEditor 的主题方案（Compartment 动态重配 + MutationObserver 监听 html.dark）
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Compartment, EditorState } from '@codemirror/state'
import { EditorView, placeholder as cmPlaceholder } from '@codemirror/view'
import { basicSetup } from 'codemirror'
import { yaml } from '@codemirror/lang-yaml'
import { autocompletion } from '@codemirror/autocomplete'
import { oneDark } from '@codemirror/theme-one-dark'
import { RefreshAnnotation } from '../cmKit'

const props = defineProps({
  modelValue: { type: String, default: '' },
  height: { type: String, default: '200px' },
  placeholder: { type: String, default: '' },
  readonly: { type: Boolean, default: false },
  // CM6 补全源数组（override 模式，完全接管补全）
  completions: { type: Array, default: () => [] },
  // 额外扩展（如 {{X}} 装饰高亮），变化时经 Compartment 重配
  extraExtensions: { type: Array, default: () => [] }
})
const emit = defineEmits(['update:modelValue'])

const hostRef = ref(null)
let view = null
let themeObserver = null
const themeComp = new Compartment()
const compComp = new Compartment()
const extComp = new Compartment()

// 亮色主题映射 Element Plus 变量（暗色走 oneDark）
const lightTheme = EditorView.theme({
  '&': { backgroundColor: 'var(--el-bg-color)', color: 'var(--el-text-color-primary)' },
  '.cm-gutters': { backgroundColor: 'var(--el-fill-color-light)', color: 'var(--el-text-color-secondary)', border: 'none' },
  '.cm-activeLine': { backgroundColor: 'var(--el-fill-color)' },
  '.cm-activeLineGutter': { backgroundColor: 'var(--el-fill-color)' },
  '&.cm-focused': { outline: 'none' },
  '.cm-content': { caretColor: 'var(--el-text-color-primary)' }
})

function themeExtension() {
  return document.documentElement.classList.contains('dark') ? oneDark : lightTheme
}

onMounted(() => {
  view = new EditorView({
    state: EditorState.create({
      doc: props.modelValue || '',
      extensions: [
        basicSetup,
        yaml(),
        compComp.of(autocompletion({ override: props.completions })),
        extComp.of(props.extraExtensions),
        themeComp.of(themeExtension()),
        cmPlaceholder(props.placeholder),
        EditorView.lineWrapping,
        ...(props.readonly ? [EditorView.editable.of(false), EditorState.readOnly.of(true)] : []),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) emit('update:modelValue', u.state.doc.toString())
        })
      ]
    }),
    parent: hostRef.value
  })
  themeObserver = new MutationObserver(() => view?.dispatch({ effects: themeComp.reconfigure(themeExtension()) }))
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
})

onBeforeUnmount(() => {
  themeObserver?.disconnect()
  view?.destroy()
  view = null
})

watch(
  () => props.modelValue,
  (v) => {
    if (view && view.state.doc.toString() !== (v || '')) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v || '' } })
    }
  }
)
watch(
  () => props.completions,
  (v) => view?.dispatch({ effects: compComp.reconfigure(autocompletion({ override: v || [] })) })
)
watch(
  () => props.extraExtensions,
  (v) => view?.dispatch({ effects: extComp.reconfigure(v || []) })
)

// 在光标处插入文本（变量芯片等外部入口用）
function insertAtCursor(text) {
  if (!view || props.readonly) return
  const sel = view.state.selection.main
  view.dispatch({ changes: { from: sel.from, to: sel.to, insert: text }, selection: { anchor: sel.from + text.length } })
  view.focus()
}

// dispatch 空事务驱动装饰扩展重建（变量字典等外部数据变化后调用）
function refresh() {
  view?.dispatch({ annotations: RefreshAnnotation.of(true) })
}

defineExpose({ insertAtCursor, refresh })
</script>

<style>
/* 全局（非 scoped）：编辑器外壳与 {{X}} 占位高亮（装饰 span 在 CM6 DOM 内，供各编辑器复用） */
.ce-host { border: 1px solid var(--el-border-color); border-radius: 4px; overflow: hidden; background: var(--el-bg-color); }
/* 固定高度编辑器必备：编辑器填满宿主高度、超高内容在编辑器内部滚动（缺这两条则 .cm-editor 随内容撑高被宿主裁掉，同 JsonCodeEditor） */
.ce-host .cm-editor { height: 100%; }
.ce-host .cm-scroller { font-family: Consolas, Monaco, monospace; font-size: 12px; line-height: 21px; overflow: auto; }
.ce-host .cm-placeholder { color: var(--el-text-color-placeholder); }
.ph-ok { color: var(--el-color-primary); background: var(--el-color-primary-light-9); border-radius: 2px; padding: 0 1px; }
.ph-bad {
  color: var(--el-color-danger); background: var(--el-color-danger-light-9);
  border-radius: 2px; padding: 0 1px; text-decoration: underline wavy var(--el-color-danger);
}
</style>
