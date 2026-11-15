<template>
  <div class="page">
    <div class="page-head">
      <h2>部署包</h2>
      <span class="hint">上传 deliver-studio format:3 部署包（或平台最终交付包）——上传即完成 manifest 强校验</span>
    </div>

    <el-card shadow="never" class="card">
      <template #header><b>上传</b></template>
      <el-upload drag :show-file-list="false" accept=".zip" :http-request="doUpload" :disabled="uploading">
        <el-icon class="el-icon--upload"><UploadFilled /></el-icon>
        <div class="el-upload__text">拖入部署包 zip 或 <em>点击选择</em></div>
      </el-upload>
      <div v-if="uploading" class="uploading">
        <template v-if="uploadPct < 100">上传中 {{ uploadPct }}%……</template>
        <template v-else>服务器解析中（并发解包 + manifest SHA-256 校验）……</template>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header>
        <div class="row-between"><b>包列表</b><el-button @click="load" :loading="loading">刷新</el-button></div>
      </template>
      <el-table :data="pkgs" size="small">
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="app" label="应用" min-width="140" />
        <el-table-column prop="version" label="版本" width="110" />
        <el-table-column label="包指纹" width="150">
          <template #default="{ row }">
            <el-tooltip v-if="row.pkgSha" :content="row.pkgSha" placement="top">
              <span class="mono">{{ row.pkgSha.slice(0, 12) }}…</span>
            </el-tooltip>
            <span v-else class="dim">-</span>
          </template>
        </el-table-column>
        <el-table-column label="文件数" width="90">
          <template #default="{ row }">{{ (row.files || []).length }}</template>
        </el-table-column>
        <el-table-column label="上传时间" width="170">
          <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="220" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="detail(row)">查看 / 编排</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-drawer v-model="detailOpen" size="72%" :title="detailTitle">
      <template v-if="d">
        <el-alert v-if="d.prefillMissing" type="warning" :closable="false" class="no-prefill-tip"
          title="该包未叠加交付附件（无 delivery/vars.env）——租户编号/编码/license 公钥等安装变量不会预填，需在编排第③步手动输入；正式交付请使用 studio『出交付包』产出的最终交付包" />
        <el-descriptions :column="3" size="small" border class="desc">
          <el-descriptions-item label="入口单元">{{ d.decl?.entry?.unit || '-' }}</el-descriptions-item>
          <el-descriptions-item label="入口端口">{{ d.decl?.entry?.port || '-' }}</el-descriptions-item>
          <el-descriptions-item label="健康路径">{{ d.decl?.entry?.healthPath || '-' }}</el-descriptions-item>
        </el-descriptions>

        <h4>服务清单（编排分配的素材）</h4>
        <el-table :data="d.services" size="small">
          <el-table-column prop="name" label="服务" min-width="120" />
          <el-table-column label="归属" width="150">
            <template #default="{ row }">
              <el-tag size="small" :type="row.kind === 'unit' ? 'primary' : row.kind === 'comp' ? 'warning' : 'info'" effect="plain">{{ row.owner || row.kind }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="image" label="镜像" min-width="180" show-overflow-tooltip />
          <el-table-column label="发布端口" width="130">
            <template #default="{ row }">
              <span v-if="row.ports && Object.keys(row.ports).length">
                {{ Object.entries(row.ports).map(([c, h]) => c === h ? c : `${h}:${c}`).join(' ') }}
              </span>
              <span v-else class="dim">未发布</span>
            </template>
          </el-table-column>
          <el-table-column label="依赖" min-width="120">
            <template #default="{ row }">{{ (row.deps || []).join(', ') || '-' }}</template>
          </el-table-column>
          <el-table-column label="制品（裸机）" width="120">
            <template #default="{ row }">{{ row.artifact || '-' }}</template>
          </el-table-column>
        </el-table>

        <h4>安装变量</h4>
        <el-table :data="varsTable" size="small">
          <el-table-column prop="name" label="变量" min-width="140" />
          <el-table-column prop="label" label="说明" min-width="140" />
          <el-table-column prop="source" label="来源" width="90" />
          <el-table-column label="预填（delivery/vars.env）" min-width="160">
            <template #default="{ row }">
              <span v-if="d.prefill?.[row.name] !== undefined">{{ row.source === 'random' || row.sensitive ? '已预填（隐藏）' : d.prefill[row.name] }}</span>
              <span v-else class="dim">-</span>
            </template>
          </el-table-column>
        </el-table>

        <template v-if="diffAny">
          <h4>与已装版本差异（增量安装预览）</h4>
          <div class="diff">
            <el-tag type="success">新增 {{ (d.diff?.added || []).length }}</el-tag>
            <el-tag type="warning">变更 {{ (d.diff?.changed || []).length }}</el-tag>
            <el-tag>未变 {{ (d.diff?.unchanged || []).length }}</el-tag>
            <el-tag type="danger">移除 {{ (d.diff?.removed || []).length }}</el-tag>
          </div>
        </template>

        <div class="drawer-foot">
          <el-button type="primary" @click="goDesign">去编排安装 →</el-button>
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtTime } from '../api'
import { gotoView, designPkgId } from '../nav'

const pkgs = ref([])
const loading = ref(false)
const uploading = ref(false)
const uploadPct = ref(0)
const detailOpen = ref(false)
const d = ref(null)
const detailTitle = ref('')

const varsTable = computed(() => {
  const vars = [...(d.value?.decl?.variables || []).map((v) => ({ ...v, kind: 'var' }))]
  for (const c of d.value?.decl?.components || []) {
    for (const cred of c.credentials || []) vars.push({ name: cred.placeholder, label: cred.label || `${c.name || c.code} 凭据`, source: 'cred:' + (cred.generate || 'const'), kind: 'cred' })
  }
  return vars
})
const diffAny = computed(() => {
  const df = d.value?.diff
  return df && (df.added?.length || df.changed?.length || df.removed?.length)
})

async function load() {
  loading.value = true
  try {
    const res = await api.get('/api/packages')
    pkgs.value = res.packets || res.packages || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function doUpload(opt) {
  uploading.value = true
  uploadPct.value = 0
  try {
    const fd = new FormData()
    fd.append('file', opt.file)
    const res = await api.upload('/api/packages', fd, (p) => { uploadPct.value = p })
    ElMessage.success(res.overwritten ? `已覆盖登记 ${res.app} ${res.version}（同版本旧记录已更新）` : `已登记 ${res.app} ${res.version}`)
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    uploading.value = false
  }
}

async function detail(row) {
  try {
    d.value = await api.get(`/api/packages/${row.id}`)
    detailTitle.value = `${d.value.app} ${d.value.version}`
    detailOpen.value = true
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(`删除部署包 ${row.app} ${row.version}？`, '删除', { type: 'warning' })
    await api.del(`/api/packages/${row.id}`)
    await load()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

function goDesign() {
  designPkgId.value = d.value?.id || 0
  detailOpen.value = false
  gotoView('Design')
}

onMounted(load)
</script>

<style scoped>
.page-head { display: flex; align-items: baseline; gap: 12px; margin-bottom: 12px; }
.page-head h2 { margin: 0; font-size: 17px; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); }
.card { margin-bottom: 14px; }
.row-between { display: flex; justify-content: space-between; align-items: center; }
.uploading { margin-top: 8px; font-size: 12px; color: var(--el-text-color-secondary); }
.desc { margin-bottom: 8px; }
.no-prefill-tip { margin-bottom: 10px; }
h4 { margin: 14px 0 6px; font-size: 13px; }
.dim { color: var(--el-text-color-placeholder); }
.mono { font-family: Consolas, Monaco, monospace; }
.diff { display: flex; gap: 8px; }
.drawer-foot { margin-top: 18px; text-align: right; }
</style>
