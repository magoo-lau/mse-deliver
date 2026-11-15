<template>
  <div class="ptp">
    <el-tree :data="tree" default-expand-all :props="{ label: 'label', children: 'children' }">
      <template #default="{ data }">
        <span class="ptp-node">
          <el-icon v-if="data.dir" class="ptp-dir"><Folder /></el-icon>
          <el-icon v-else class="ptp-file"><Document /></el-icon>
          <span :class="{ 'ptp-note': data.note }">{{ data.label }}</span>
          <span v-if="data.note" class="ptp-note">— {{ data.note }}</span>
        </span>
      </template>
    </el-tree>
    <div class="ptp-hint">目录为打包预览（镜像包文件名按当前版本推算）； extras/ 为附加文件（如 db/schema.sql），manifest.json 为逐文件 SHA-256 清单。</div>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import { safeImageName } from '../lib'

const props = defineProps({
  units: { type: Array, default: () => [] },
  components: { type: Array, default: () => [] },
  extras: { type: Array, default: () => [] }, // 附加文件 [{fileName}]
  version: { type: String, default: '1.0.0' },
  standalone: { type: Boolean, default: false } // 单机部署：随包附带 install.sh
})

const tree = computed(() => {
  const images = []
  for (const u of props.units) {
    const ref = u.source === 'artifact' && u.imageRepo ? `${u.imageRepo}:${props.version}` : u._image || u.imageRef || u.imageRepo || '单元镜像'
    images.push({ label: safeImageName(ref) + '.tar', note: `单元 ${u.code}` })
  }
  for (const c of props.components) {
    for (const img of c.images || []) images.push({ label: safeImageName(img) + '.tar', note: `组件 ${c.code}` })
  }
  const hasConfig = props.units.some((u) => (u.configTemplate || '').trim()) || props.components.some((c) => c.appConfig && Object.keys(c.appConfig).length)
  const compFiles = []
  for (const c of props.components) for (const f of c.files || []) compFiles.push({ label: f.path, note: `组件 ${c.code} 附件` })

  const root = { label: 'deploy-package/', dir: true, children: [

    { label: 'docker-compose.yml', note: '全栈编排（单元 + 组件）' },
    { label: 'components.json', note: '驱动声明（format:3：单元端口/健康/随包制品 + 组件服务/镜像清单）' },
    ...(hasConfig ? [{ label: 'config-template.yaml', note: '配置模板（安装时渲染）' }] : []),
    { dir: true, label: 'images/', children: images },
    ...(props.extras.length ? [{ dir: true, label: 'extras/', children: props.extras.map((e) => ({ label: e.fileName || e })) }] : []),
    ...(compFiles.length ? [{ dir: true, label: '附件（按组件声明落位）', children: compFiles }] : []),
    ...(props.standalone ? [{ label: 'install.sh', note: '单机安装脚本（薄壳：校验/load/变量/配置/up/健康门控）' }] : []),
    { label: 'manifest.json', note: '完整性清单' }
  ] }
  return [root]
})
</script>

<style scoped>
.ptp { border: 1px dashed var(--el-border-color); border-radius: 6px; padding: 8px; background: var(--el-fill-color-blank); }
.ptp-node { display: inline-flex; align-items: center; gap: 5px; font-size: 12px; font-family: Consolas, Monaco, monospace; }
.ptp-dir { color: var(--el-color-warning); }
.ptp-file { color: var(--el-text-color-secondary); }
.ptp-note { color: var(--el-text-color-secondary); }
.ptp-hint { color: var(--el-text-color-secondary); font-size: 12px; margin-top: 4px; }
</style>
