<template>
  <div class="page">
    <div class="page-head">
      <h2>安装源</h2>
      <span class="hint">Docker / docker-compose 基础工具的离线介质与在线通道（console 全局）——编排自动安装与主机池一键装共用；介质按版本维护，主机行弹窗可选版本安装/升级</span>
    </div>

    <el-card shadow="never" class="card">
      <template #header><b>Docker 安装源</b>（编排「自动装 Docker」与主机池一键安装共用：离线介质优先，无介质走在线脚本；仅 Linux）</template>
      <el-form label-width="130px" inline>
        <el-form-item label="在线脚本地址">
          <el-input v-model="ds.scriptUrl" :placeholder="ds.default ? '留空 = ' + ds.default : 'https://get.docker.com'" style="width: 280px" />
        </el-form-item>
        <el-form-item label="registry 镜像加速">
          <el-input v-model="ds.mirrors" placeholder="https://docker.m.daocloud.io（空格/逗号分隔多个）" style="width: 320px" />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" size="small" :loading="dsSaving" @click="saveDS">保存</el-button>
        </el-form-item>
      </el-form>
      <div class="media-row">
        <span class="media-label">离线介质</span>
        <el-select v-model="mediaSlot" size="small" style="width: 130px">
          <el-option value="linux-amd64" label="linux-amd64" />
          <el-option value="linux-arm64" label="linux-arm64" />
        </el-select>
        <el-input v-model="mediaVer" size="small" placeholder="介质版本（如 27.5.1）" style="width: 150px" />
        <el-upload :show-file-list="false" :auto-upload="false" accept=".gz" :on-change="uploadMedia">
          <el-button size="small" :loading="mediaUploading" :disabled="!mediaVer.trim()">
            {{ mediaUploading ? '上传中 ' + mediaPct + '%' : '上传 tar.gz' }}
          </el-button>
        </el-upload>
        <span class="media-tip">布局 bin/（dockerd/docker/containerd/…/docker-compose）+ 可选 systemd/docker.service；官方 static 包可直接上传；同槽位同版本覆盖；指定版本安装仅介质可承载</span>
      </div>
      <div v-for="(vers, slot) in ds.media || {}" :key="slot" class="media-slot">
        <el-tag size="small" type="success" effect="plain">{{ slot }}</el-tag>
        <div v-for="(m, ver) in vers" :key="ver" class="media-item">
          <el-tag size="small" effect="plain">{{ ver === 'legacy' ? '未标注版本' : ver }}</el-tag>
          <span>{{ m.name }}（{{ fmtSize(m.size) }}，sha {{ (m.sha256 || '').slice(0, 10) }}…）</span>
          <el-button link type="danger" size="small" @click="delMedia(slot, ver)">删除</el-button>
        </div>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header><b>Compose 安装源</b>（docker compose 插件，仅 Linux；介质优先，无介质按模板渲染在线直链——在线亦可定版）</template>
      <el-form label-width="130px" inline>
        <el-form-item label="在线直链模板">
          <el-input v-model="cs.template" :placeholder="cs.default ? '留空 = ' + cs.default : cs.default" style="width: 380px" />
        </el-form-item>
        <el-form-item label="在线缺省版本">
          <el-input v-model="cs.version" placeholder="如 v2.30.3（介质缺省时在线装此版）" style="width: 190px" />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" size="small" :loading="csSaving" @click="saveCS">保存</el-button>
        </el-form-item>
      </el-form>
      <div class="media-row">
        <span class="media-label">离线介质</span>
        <el-select v-model="composeSlot" size="small" style="width: 130px">
          <el-option value="linux-amd64" label="linux-amd64" />
          <el-option value="linux-arm64" label="linux-arm64" />
        </el-select>
        <el-input v-model="composeVer" size="small" placeholder="介质版本（如 v2.30.3）" style="width: 150px" />
        <el-upload :show-file-list="false" :auto-upload="false" :on-change="uploadComposeMedia">
          <el-button size="small" :loading="composeUploading" :disabled="!composeVer.trim()">
            {{ composeUploading ? '上传中 ' + composePct + '%' : '上传插件二进制' }}
          </el-button>
        </el-upload>
        <span class="media-tip">官方 docker-compose-{platform} 插件二进制（GitHub releases 同名资产）；同槽位同版本覆盖</span>
      </div>
      <div v-for="(vers, slot) in cs.media || {}" :key="slot" class="media-slot">
        <el-tag size="small" type="success" effect="plain">{{ slot }}</el-tag>
        <div v-for="(m, ver) in vers" :key="ver" class="media-item">
          <el-tag size="small" effect="plain">{{ ver }}</el-tag>
          <span>{{ m.name }}（{{ fmtSize(m.size) }}，sha {{ (m.sha256 || '').slice(0, 10) }}…）</span>
          <el-button link type="danger" size="small" @click="delComposeMedia(slot, ver)">删除</el-button>
        </div>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header><b>说明</b></template>
      <ul class="notes">
        <li>自动安装仅支持 Linux 主机；Windows 请人工预装 Docker Desktop（桌面版自带 compose 插件）——主机池 Windows 行有提示。</li>
        <li>维护语义：按目标版本精确安装——已装==目标即跳过（幂等），≠目标即重装换版；介质上传新版本后到主机池对旧版本主机重复一键装即完成升级。</li>
        <li>旧 docker 单介质数据自动迁移为「未标注版本」条目（不参与精确版本匹配，可删除后重传带版本的）。</li>
      </ul>
    </el-card>
  </div>
</template>

<script setup>
import { reactive, ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'

// Docker 安装源（在线脚本 + 版本化介质）
const ds = reactive({ scriptUrl: '', mirrors: '', default: '', media: {} })
const dsSaving = ref(false)
const mediaSlot = ref('linux-amd64')
const mediaVer = ref('') // 上传介质版本号（版本化介质库必填）
const mediaUploading = ref(false)
const mediaPct = ref(0)

// Compose 安装源（在线直链模板 + 版本化介质）
const cs = reactive({ template: '', default: '', version: '', media: {} })
const csSaving = ref(false)
const composeSlot = ref('linux-amd64')
const composeVer = ref('')
const composeUploading = ref(false)
const composePct = ref(0)

async function loadDS() {
  try {
    const res = await api.get('/api/docker-source')
    ds.scriptUrl = res.scriptUrl || ''
    ds.mirrors = (res.mirrors || []).join(' ')
    ds.default = res.default || ''
    ds.media = res.media || {}
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function saveDS() {
  dsSaving.value = true
  try {
    await api.put('/api/docker-source', { scriptUrl: ds.scriptUrl.trim(), mirrors: ds.mirrors.trim() })
    ElMessage.success('已保存')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    dsSaving.value = false
  }
}

async function loadCS() {
  try {
    const res = await api.get('/api/compose-source')
    cs.template = res.template || ''
    cs.default = res.default || ''
    cs.version = res.version || ''
    cs.media = res.media || {}
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function saveCS() {
  csSaving.value = true
  try {
    await api.put('/api/compose-source', { template: cs.template.trim(), version: cs.version.trim() })
    ElMessage.success('已保存')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    csSaving.value = false
  }
}

async function uploadMedia(file) {
  if (!file?.raw) return
  mediaUploading.value = true
  mediaPct.value = 0
  try {
    const fd = new FormData()
    fd.append('file', file.raw)
    fd.append('version', mediaVer.value.trim())
    const res = await api.upload(`/api/docker-source/media/${mediaSlot.value}`, fd, (p) => (mediaPct.value = p))
    ds.media = res.media || ds.media
    ElMessage.success(`介质已上传（${mediaSlot.value} ${mediaVer.value.trim()}）`)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    mediaUploading.value = false
  }
}

async function delMedia(slot, ver) {
  const label = ver === 'legacy' ? '未标注版本' : ver
  try {
    await ElMessageBox.confirm(`删除离线介质 ${slot} ${label}（已装主机的缓存不受影响）？`, '删除介质', { type: 'warning' })
    const res = await api.del(`/api/docker-source/media/${slot}/${encodeURIComponent(ver)}`)
    ds.media = res.media || ds.media
    ElMessage.success('已删除')
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

async function uploadComposeMedia(file) {
  if (!file?.raw) return
  composeUploading.value = true
  composePct.value = 0
  try {
    const fd = new FormData()
    fd.append('file', file.raw)
    fd.append('version', composeVer.value.trim())
    const res = await api.upload(`/api/compose-source/media/${composeSlot.value}`, fd, (p) => (composePct.value = p))
    cs.media = res.media || cs.media
    ElMessage.success(`compose 介质已上传（${composeSlot.value} ${composeVer.value.trim()}）`)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    composeUploading.value = false
  }
}

async function delComposeMedia(slot, ver) {
  try {
    await ElMessageBox.confirm(`删除 compose 介质 ${slot} ${ver}？`, '删除介质', { type: 'warning' })
    const res = await api.del(`/api/compose-source/media/${slot}/${encodeURIComponent(ver)}`)
    cs.media = res.media || cs.media
    ElMessage.success('已删除')
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message)
  }
}

function fmtSize(n) {
  if (!n) return '?'
  return n >= 1 << 30 ? (n / (1 << 30)).toFixed(2) + 'GB' : (n / (1 << 20)).toFixed(1) + 'MB'
}

onMounted(() => {
  loadDS()
  loadCS()
})
</script>

<style scoped>
.page-head { display: flex; align-items: baseline; gap: 12px; margin-bottom: 12px; }
.page-head h2 { margin: 0; font-size: 17px; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); }
.card { margin-bottom: 14px; }
.media-row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; font-size: 12px; }
.media-label { color: var(--el-text-color-secondary); }
.media-tip { color: var(--el-text-color-placeholder); font-size: 11px; }
.media-slot { margin-top: 8px; display: flex; flex-direction: column; gap: 4px; }
.media-item { display: flex; align-items: center; gap: 6px; margin-left: 8px; font-size: 12px; }
.notes { margin: 0; padding-left: 18px; font-size: 12px; color: var(--el-text-color-regular); line-height: 1.9; }
</style>
