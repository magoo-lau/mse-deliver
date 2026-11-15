<template>
  <div class="dag-node" :class="{ sel: selected }" :style="{ borderColor: data.color }" @click="$emit('select', id)">
    <div class="node-head" :style="{ background: data.color }">
      <span>{{ data.label }}</span>
      <span class="node-ops">
        <el-icon @click.stop="$emit('edit', id)"><Edit /></el-icon>
        <el-icon @click.stop="$emit('remove', id)"><Delete /></el-icon>
      </span>
    </div>
    <div class="node-body">
      <div class="node-title">{{ data.title }}</div>
      <div v-if="data.host" class="node-host">@{{ data.host }}</div>
    </div>
    <Handle type="target" :position="Position.Left" />
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<script setup>
import { Handle, Position } from '@vue-flow/core'
defineProps({ id: String, data: Object, selected: Boolean })
defineEmits(['edit', 'remove', 'select'])
</script>

<style scoped>
.dag-node { border: 2px solid #909399; border-radius: 6px; min-width: 170px; background: var(--el-bg-color); font-size: 12px; cursor: pointer; }
.dag-node.sel { box-shadow: 0 0 0 3px var(--el-color-primary-light-7); }
.node-head { display: flex; justify-content: space-between; align-items: center; padding: 2px 8px; color: #fff; border-radius: 4px 4px 0 0; }
.node-ops { display: flex; gap: 4px; cursor: pointer; }
.node-body { padding: 6px 8px; }
.node-title { font-weight: 500; max-width: 220px; }
.node-host { font-size: 11px; color: var(--el-text-color-secondary); margin-top: 2px; }
</style>
