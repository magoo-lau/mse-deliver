<template>
  <el-card class="page-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span>制品库（应用包 / 镜像包 / 附加文件——打包时按单元选用）</span>
      </div>
    </template>
    <div class="toolbar">
      <div class="toolbar-left">
        <el-input v-model="keyword" clearable placeholder="搜索文件名 / 应用 / 版本 / 备注" prefix-icon="Search" style="width: 260px" />
        <el-select v-model="filterKind" style="width: 150px">
          <el-option label="全部类型" value="" />
          <el-option v-for="k in kinds" :key="k.v" :label="k.label" :value="k.v" />
        </el-select>
      </div>
      <el-button type="primary" icon="Upload" @click="dlg = true">上传制品</el-button>
    </div>
    <el-table :data="filtered" border v-loading="loading" size="small">
      <el-table-column label="类型" width="110">
        <template #default="{ row }">
          <el-tag size="small" :type="kindTag(row.kind)">{{ kindLabel(row.kind) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="fileName" label="文件" min-width="240" show-overflow-tooltip />
      <el-table-column prop="appCode" label="应用" width="110" />
      <el-table-column prop="version" label="版本" width="90" />
      <el-table-column label="大小" width="100">
        <template #default="{ row }">{{ fmtSize(row.size) }}</template>
      </el-table-column>
      <el-table-column prop="remark" label="备注" min-width="120" show-overflow-tooltip />
      <el-table-column prop="createdAt" label="上传时间" width="160" />
      <el-table-column label="操作" width="80" fixed="right">
        <template #default="{ row }">
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dlg" title="上传制品" width="520px" @closed="resetDlg">
      <el-form label-width="80px">
        <el-form-item label="类型">
          <el-select v-model="form.kind" style="width: 100%" @change="onKindChange">
            <el-option v-for="k in kinds" :key="k.v" :label="k.label" :value="k.v" />
          </el-select>
        </el-form-item>
        <el-form-item label="应用编码"><el-input v-model="form.appCode" placeholder="可空" /></el-form-item>
        <el-form-item label="版本"><el-input v-model="form.version" placeholder="可空" /></el-form-item>
        <el-form-item label="备注"><el-input v-model="form.remark" placeholder="可空" /></el-form-item>
        <el-form-item label="文件">
          <div>
            <input ref="fileInput" type="file" hidden :accept="accept" @change="onPick" />
            <el-button icon="FolderOpened" @click="fileInput.click()">选择文件</el-button>
            <span v-if="file" class="picked">{{ file.name }}（{{ fmtSize(file.size) }}）</span>
            <span v-else class="picked picked-empty">未选择</span>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlg = false">取消</el-button>
        <el-button type="primary" :disabled="!file" :loading="uploading" @click="submit">上传</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtSize } from '../api'
import { isArtifactKind } from '../lib'

const kinds = [
  { v: 'artifact', label: '应用包（jar/zip/文件）' },
  { v: 'image_tar', label: '镜像包（tar）' },
  { v: 'extra', label: '附加文件' }
]
const materials = ref([])
const loading = ref(false)
const keyword = ref('')
const filterKind = ref('')
const dlg = ref(false)
const uploading = ref(false)
const fileInput = ref(null)
const file = ref(null)
const form = reactive({ kind: 'artifact', appCode: '', version: '', remark: '' })

const accept = computed(() => (form.kind === 'image_tar' ? '.tar' : ''))
const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return materials.value.filter((m) => {
    if (filterKind.value === 'artifact') {
      if (!isArtifactKind(m.kind)) return false
    } else if (filterKind.value && m.kind !== filterKind.value) {
      return false
    }
    if (!kw) return true
    return [m.fileName, m.appCode, m.version, m.remark].some((v) => (v || '').toLowerCase().includes(kw))
  })
})

const kindLabel = (k) => (kinds.find((x) => x.v === k) || {}).label || (isArtifactKind(k) ? `应用包（${k}）` : k)
const kindTag = (k) => (isArtifactKind(k) ? 'primary' : k === 'image_tar' ? 'warning' : 'info')

async function load() {
  loading.value = true
  try {
    materials.value = (await api.get('/api/materials')) || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}
function onPick(ev) {
  file.value = ev.target.files[0] || null
  ev.target.value = ''
}
function onKindChange() {
  file.value = null
}
function resetDlg() {
  form.kind = 'artifact'
  form.appCode = ''
  form.version = ''
  form.remark = ''
  file.value = null
}
async function submit() {
  if (!file.value) return
  uploading.value = true
  try {
    const fd = new FormData()
    // 字段先于 file append：后端流式读 part，kind/appCode 等须先到达
    fd.append('kind', form.kind)
    if (form.appCode) fd.append('appCode', form.appCode)
    if (form.version) fd.append('version', form.version)
    if (form.remark) fd.append('remark', form.remark)
    fd.append('file', file.value)
    await api.upload('/api/materials', fd)
    ElMessage.success('已上传')
    dlg.value = false
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    uploading.value = false
  }
}
async function remove(row) {
  await ElMessageBox.confirm(`删除制品 ${row.fileName}？`, '确认', { type: 'warning' })
  await api.del(`/api/materials/${row.id}`)
  ElMessage.success('已删除')
  await load()
}
onMounted(load)
</script>

<style scoped>
.card-head { display: flex; justify-content: space-between; align-items: center; }
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px; }
.toolbar-left { display: flex; align-items: center; gap: 8px; }
.picked { margin-left: 8px; }
.picked-empty { color: var(--el-text-color-secondary); }
</style>
