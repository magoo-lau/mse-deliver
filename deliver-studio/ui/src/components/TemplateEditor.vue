<template>
  <div class="te">
    <!-- 变量芯片：悬停显示完整说明（KubeSphere 参数配置风格） -->
    <div v-if="!compact && vars && vars.length" class="te-chips">
      <span class="te-chips-label">点击插入：</span>
      <el-tooltip
        v-for="v in vars" :key="v.name" placement="top" :show-after="200"
        :disabled="!varTip(v)"
      >
        <template #content>
          <div class="te-tip">
            <div><b>{{ ph(v.name) }}</b></div>
            <div v-if="v.label">{{ v.label }}</div>
            <div v-if="v.origin" class="te-tip-sub">来源：{{ v.origin }}</div>
            <div v-if="v.when" class="te-tip-sub">赋值：{{ v.when === 'build' ? '打包时' : '安装时' }}{{ v.sensitive ? ' · 敏感' : '' }}</div>
          </div>
        </template>
        <el-tag size="small" :type="v.builtin ? 'info' : 'primary'" effect="plain" class="te-chip" @click="insert(v.name)">
          {{ v.name }}
        </el-tag>
      </el-tooltip>
    </div>
    <!-- CodeMirror 编辑（YAML 高亮 + {{X}} 高亮装饰 + 变量补全） -->
    <CodeEditor
      ref="edRef" v-model="text" :completions="completions" :extra-extensions="decoExts"
      :height="height" :placeholder="placeholder"
    />
    <el-alert
      v-if="unknown.length" class="te-warn" type="warning" :closable="false" show-icon
      :title="`未定义变量引用：${unknown.map((u) => '{{' + u + '}}').join(' ')}——请先在「变量」页签登记（打包时会硬校验拦截）`"
    />
    <!-- 变量字典：全部可用变量的说明与一键插入 -->
    <el-collapse v-if="!compact && vars && vars.length" class="te-dict" v-model="dictOpen">
      <el-collapse-item :name="'dict'" :title="`变量字典（${vars.length}）`">
        <el-table :data="vars" border size="small">
          <el-table-column label="引用" width="190">
            <template #default="{ row }"><span class="mono ph">{{ ph(row.name) }}</span></template>
          </el-table-column>
          <el-table-column prop="label" label="说明" min-width="150" />
          <el-table-column prop="origin" label="来源" min-width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.builtin ? 'info' : row.originType === 'component' ? 'warning' : 'primary'">
                {{ row.origin || (row.builtin ? '单元内置' : '应用') }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="赋值" width="90">
            <template #default="{ row }">
              {{ row.when === 'build' ? '打包时' : '安装时' }}<el-tag v-if="row.sensitive" size="small" type="danger" class="te-sens">敏感</el-tag>
            </template>
          </el-table-column>
          <el-table-column width="70">
            <template #default="{ row }">
              <el-button link type="primary" @click="insert(row.name)">插入</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import { scanPlaceholders } from '../lib'
import CodeEditor from './CodeEditor.vue'
import { varHighlightExtension, varCompletionSource, RefreshAnnotation } from '../cmKit'

const props = defineProps({
  modelValue: { type: String, default: '' },
  vars: { type: Array, default: () => [] }, // [{name,label,origin,originType,when,sensitive,builtin}]
  height: { type: String, default: '180px' },
  placeholder: { type: String, default: '' },
  compact: { type: Boolean, default: false } // 行内嵌入模式：只保留高亮编辑与未定义告警
})
const emit = defineEmits(['update:modelValue'])
const edRef = ref(null)
const dictOpen = ref([])

const ph = (n) => '{{' + n + '}}'

const text = ref(props.modelValue)
watch(() => props.modelValue, (v) => { if (text.value !== v) text.value = v })
watch(text, (v) => emit('update:modelValue', v))

const unknown = computed(() => {
  const known = new Set(props.vars.map((v) => v.name))
  return scanPlaceholders(props.modelValue).filter((n) => !known.has(n))
})

// {{X}} 高亮装饰 + {{ 变量补全（变量字典联动；vars 变化时重配扩展并刷新装饰）
const decoExts = computed(() => [varHighlightExtension(() => props.vars.map((v) => v.name))])
const completions = computed(() => [varCompletionSource(() => props.vars)])
watch(() => props.vars, () => requestAnimationFrame(() => edRef.value?.refresh()), { deep: true })

function varTip(v) {
  return v.label || v.origin || v.when
}

// 在光标处插入 {{NAME}}
function insert(name) {
  edRef.value?.insertAtCursor(`{{${name}}}`)
}
</script>

<style scoped>
.te-chips { margin-bottom: 6px; display: flex; flex-wrap: wrap; gap: 4px; align-items: center; }
.te-chips-label { color: var(--el-text-color-secondary); font-size: 12px; }
.te-chip { cursor: pointer; font-family: Consolas, Monaco, monospace; }
.te-warn { margin-top: 6px; }
.te-dict { margin-top: 6px; border-top: none; }
.te-sens { margin-left: 4px; }
.te-tip { max-width: 320px; line-height: 1.6; }
.te-tip-sub { color: var(--el-text-color-secondary); font-size: 12px; }
.mono { font-family: Consolas, Monaco, monospace; font-size: 12px; }
.ph { color: var(--el-color-primary); }
</style>
