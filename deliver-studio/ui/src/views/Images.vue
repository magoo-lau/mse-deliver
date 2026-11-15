<template>
  <el-card class="page-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span>本地镜像仓（打包时组件镜像解析顺序：本机 docker → 本仓 → 报缺）</span>
      </div>
    </template>
    <el-form inline>
      <el-form-item label="上传 tar">
        <input ref="uploadInput" type="file" accept=".tar" @change="upload" />
      </el-form-item>
      <el-form-item label="镜像地址（可选）">
        <el-input v-model="uploadRef" class="mono" placeholder="mysql:8.4（用于展示与识别）" style="width: 220px" />
      </el-form-item>
      <el-form-item label="从本机 docker 导入">
        <el-input v-model="dockerRef" class="mono" placeholder="repo/name:tag" style="width: 220px" />
        <el-button type="primary" icon="Download" style="margin-left: 6px" :loading="importing" @click="fromDocker">docker save 入仓</el-button>
      </el-form-item>
    </el-form>
    <el-table :data="images" border v-loading="loading">
      <el-table-column prop="fileName" label="仓内文件" min-width="240">
        <template #default="{ row }"><span class="mono">{{ row.fileName }}</span></template>
      </el-table-column>
      <el-table-column prop="imageRef" label="镜像地址" min-width="160" />
      <el-table-column label="大小" width="100">
        <template #default="{ row }">{{ fmtSize(row.size) }}</template>
      </el-table-column>
      <el-table-column prop="createdAt" label="入库时间" width="160" />
      <el-table-column label="操作" width="80" fixed="right">
        <template #default="{ row }">
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>
</template>

<script setup>
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtSize } from '../api'

const images = ref([])
const loading = ref(false)
const importing = ref(false)
const uploadInput = ref(null)
const uploadRef = ref('')
const dockerRef = ref('')

async function load() {
  loading.value = true
  try {
    images.value = (await api.get('/api/images')) || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
async function upload(ev) {
  const file = ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  const fd = new FormData()
  fd.append('file', file)
  if (uploadRef.value) fd.append('ref', uploadRef.value)
  try {
    await api.upload('/api/images', fd)
    ElMessage.success('已入仓')
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}
async function fromDocker() {
  if (!dockerRef.value) return ElMessage.warning('请填镜像地址 repo/name:tag')
  importing.value = true
  try {
    await api.post('/api/images', { ref: dockerRef.value })
    ElMessage.success('已入仓')
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    importing.value = false
  }
}
async function remove(row) {
  await ElMessageBox.confirm(`删除镜像 ${row.fileName}？`, '确认', { type: 'warning' })
  await api.del(`/api/images/${row.id}`)
  ElMessage.success('已删除')
  await load()
}
onMounted(load)
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
</style>
