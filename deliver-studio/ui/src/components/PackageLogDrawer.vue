<template>
  <el-drawer :model-value="open" :title="`打包日志 #${cur?.id}`" size="55%" @update:model-value="emit('update:open', $event)">
    <el-descriptions :column="3" border size="small" style="margin-bottom: 8px">
      <el-descriptions-item label="状态">{{ cur?.status }}</el-descriptions-item>
      <el-descriptions-item label="阶段">{{ cur?.stage }}</el-descriptions-item>
      <el-descriptions-item label="包">{{ cur?.name }} {{ cur?.version }}</el-descriptions-item>
    </el-descriptions>
    <pre class="log">{{ cur?.log || '（无日志）' }}</pre>
  </el-drawer>
</template>

<script setup>
// 打包/重打包任务日志抽屉（套件打包页与交付包页共用）：running 态 1.5s 轮询刷新，
// 每次刷新回调 refreshed（父页据此重载记录表，与拆页前行为一致）
import { onUnmounted, ref, watch } from 'vue'
import { api } from '../api'

const props = defineProps({
  open: { type: Boolean, default: false },
  rec: { type: Object, default: null } // 打开时传入的记录（行数据即可，随后改拉详情）
})
const emit = defineEmits(['update:open', 'refreshed'])

const cur = ref(null)
let pollTimer = null

async function refresh() {
  if (!cur.value) return stopPoll()
  try {
    cur.value = await api.get(`/api/packages/${cur.value.id}`)
    emit('refreshed', cur.value)
    if (cur.value.status !== 'running') stopPoll()
  } catch { /* 详情拉取失败按现状停轮询，避免死循环报错 */ stopPoll() }
}
function startPoll() {
  stopPoll()
  pollTimer = window.setInterval(refresh, 1500)
}
function stopPoll() {
  if (pollTimer) window.clearInterval(pollTimer)
  pollTimer = null
}

watch(() => props.open, (v) => {
  stopPoll()
  if (v && props.rec) {
    cur.value = props.rec
    if (props.rec.status === 'running') startPoll()
    else refresh()
  }
})
onUnmounted(stopPoll)
</script>

<style scoped>
.log { background: var(--el-fill-color-darker); color: var(--el-text-color-primary); padding: 10px;
  border-radius: 4px; font-size: 12px; line-height: 1.6; max-height: 70vh; overflow: auto; white-space: pre-wrap; }
</style>
