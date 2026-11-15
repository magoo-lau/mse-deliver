<template>
  <div class="vt">
    <!-- 可编辑模式：应用变量维护 -->
    <template v-if="editable">
      <el-table :data="rows" border size="small">
        <el-table-column label="变量名" width="160">
          <template #default="{ row }">
            <el-input v-model="row.name" placeholder="TENANT_ID" class="mono-input" />
          </template>
        </el-table-column>
        <el-table-column label="说明（安装时提示）" min-width="140">
          <template #default="{ row }"><el-input v-model="row.label" placeholder="如：租户编号" /></template>
        </el-table-column>
        <el-table-column label="赋值时机" width="110">
          <template #default="{ row }">
            <el-select v-model="row.when">
              <el-option value="install" label="安装时" />
              <el-option value="build" label="打包时" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column label="来源" width="110">
          <template #default="{ row }">
            <el-select v-model="row.source">
              <el-option value="input" label="人工填写" />
              <el-option value="const" label="固定值" />
              <el-option value="random" label="随机生成" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column label="敏感" width="60" align="center">
          <template #default="{ row }">
            <el-switch v-model="row.sensitive" size="small" />
          </template>
        </el-table-column>
        <el-table-column label="值 / 默认值" min-width="130">
          <template #default="{ row }">
            <el-input v-model="row.value" :disabled="row.source === 'random'"
              :show-password="!!row.sensitive" placeholder="random 自动生成" />
          </template>
        </el-table-column>
        <el-table-column label="操作" width="60">
          <template #default="{ $index }">
            <el-button link type="danger" @click="removeRow($index)">删</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-button class="vt-add" icon="Plus" @click="addRow">添加变量</el-button>
      <div class="vt-hint">
        模板中以 <span class="mono">{{ ph('NAME') }}</span> 引用；「安装时」= 客户环境安装器赋值（人工填/固定/随机），「打包时」= 每次打包填值并直接烘进部署包；「敏感」值在界面掩码显示。
      </div>
    </template>
    <!-- 只读模式：汇总展示（来源 = 应用自定义 / 组件接口 / 内置） -->
    <el-table v-else :data="rows" border size="small">
      <el-table-column prop="name" label="变量" width="190">
        <template #default="{ row }"><span class="mono">{{ ph(row.name) }}</span></template>
      </el-table-column>
      <el-table-column prop="label" label="说明" min-width="140" />
      <el-table-column prop="when" label="赋值时机" width="90">
        <template #default="{ row }">{{ row.when === 'build' ? '打包时' : '安装时' }}</template>
      </el-table-column>
      <el-table-column prop="sourceLabel" label="来源方式" width="120" />
      <el-table-column prop="origin" label="定义来源" min-width="120">
        <template #default="{ row }">
          <el-tag size="small" :type="row.originType === 'builtin' ? 'info' : row.originType === 'component' ? 'warning' : 'primary'">
            {{ row.origin }}<span v-if="row.sensitive" class="vt-sens"> · 敏感</span>
          </el-tag>
        </template>
      </el-table-column>
    </el-table>
  </div>
</template>

<script setup>
const props = defineProps({
  rows: { type: Array, default: () => [] },
  editable: { type: Boolean, default: false }
})
const emit = defineEmits(['update:rows'])

// 文本节点里直接写 {{X}} 字面量会被 Vue 插值提前截断，经 script 函数输出
const ph = (n) => '{{' + n + '}}'

function mutate(fn) {
  const next = props.rows.map((r) => ({ ...r }))
  fn(next)
  emit('update:rows', next)
}
function addRow() {
  mutate((rows) => rows.push({ name: '', label: '', when: 'install', source: 'input', value: '', sensitive: false }))
}
function removeRow(i) {
  mutate((rows) => rows.splice(i, 1))
}
</script>

<style scoped>
.vt-add { margin-top: 8px; width: 100%; border-style: dashed; }
.vt-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-top: 6px; }
.mono { font-family: Consolas, Monaco, monospace; font-size: 12px; color: var(--el-color-primary); }
.mono-input :deep(input) { font-family: Consolas, Monaco, monospace; }
.vt-sens { color: var(--el-color-danger); margin-left: 2px; }
</style>
