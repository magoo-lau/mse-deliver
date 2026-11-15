<template>
  <div class="page">
    <div class="page-head">
      <h2>编排安装</h2>
      <span class="hint">① 选包 → ② 拖拽分配（服务 → 主机）→ ③ 变量/外部端点 → ④ DAG 预览与微调 → ⑤ 执行</span>
      <div class="head-actions">
        <input ref="fileInput" type="file" accept=".json,application/json" style="display: none" @change="onImportFile" />
        <el-button size="small" @click="pickImport">导入编排</el-button>
        <el-button size="small" type="primary" plain @click="exportOrch">导出编排</el-button>
      </div>
    </div>

    <el-steps :active="step" simple class="steps">
      <el-step title="选包" /><el-step title="分配" /><el-step title="变量" /><el-step title="DAG" /><el-step title="执行" />
    </el-steps>

    <el-alert v-if="step >= 1 && detailData?.prefillMissing" type="warning" :closable="false" class="no-prefill-tip"
      title="该包未叠加交付附件（无 delivery/vars.env）：租户编号/编码/license 公钥等安装变量不会预填，第③步需手动输入；正式交付请使用 studio『出交付包』产出的最终交付包" />

    <!-- ① 选包 -->
    <el-card v-if="step === 0" shadow="never" class="card">
      <el-table :data="pkgs" size="small" @row-click="pickPkg" highlight-current-row>
        <el-table-column prop="id" label="#" width="60" />
        <el-table-column prop="app" label="应用" min-width="150" />
        <el-table-column prop="version" label="版本" width="120" />
        <el-table-column label="上传时间" width="170">
          <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="" width="100" fixed="right">
          <template #default><el-button link type="primary">选择 →</el-button></template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- ② 分配画布 -->
    <el-card v-else shadow="never" class="card">
      <template #header>
        <div class="row-between">
          <b>{{ pkg?.app }} {{ pkg?.version }} —— 服务分配</b>
          <div class="row-gap">
            <el-tooltip content="服务分配到无 docker 的 Linux 主机时，安装前自动在该主机安装 Docker（console 离线介质优先、在线脚本兜底）" placement="top">
              <el-switch v-model="placement.dockerInstall" size="small" active-text="自动装 Docker" />
            </el-tooltip>
            <el-radio-group v-model="mode" size="small">
              <el-tooltip content="apply 语义：未变更主机整组跳过，变更服务原地重建（数据保留）——已装应用升级/换版本选此项" placement="top">
                <el-radio-button value="incremental">升级（仅变更）</el-radio-button>
              </el-tooltip>
              <el-tooltip content="全部主机重跑编排：同项目已有容器原地接管重建（幂等），新主机全新安装" placement="top">
                <el-radio-button value="fresh">全量安装</el-radio-button>
              </el-tooltip>
              <el-tooltip content="覆盖重装（apply --force 对应物）：忽略增量比对全量重新物化执行——配置漂移/损坏/怀疑状态不一致时用；数据目录 ./data 保留" placement="top">
                <el-radio-button value="overwrite">覆盖重装</el-radio-button>
              </el-tooltip>
            </el-radio-group>
            <el-button size="small" @click="step = 0">换包</el-button>
          </div>
        </div>
      </template>

      <el-alert v-if="upgradeInfo" type="success" :closable="false" class="banner"
                :title="`检测到本应用已有部署——已切换为升级模式：新增 ${upgradeInfo.added.length} / 变更 ${upgradeInfo.changed.length} / 移除 ${upgradeInfo.removed.length}，未变更主机整组跳过，变更服务原地重建（数据保留）`"
                :description="upgradeDetail" />

      <div class="assign">
        <div class="svc-pane">
          <div class="pane-title">服务（拖到右侧主机）</div>
          <div v-for="s in services" :key="s.name" class="svc-item"
               :class="{ skipped: compSkipped(s.name) }"
               :draggable="!compSkipped(s.name)" @dragstart="dragging = s.name">
            <el-tag size="small" :type="s.kind === 'unit' ? 'primary' : s.kind === 'comp' ? 'warning' : 'info'" effect="plain">{{ s.kind === 'unit' ? '单元' : s.kind === 'comp' ? '组件' : '其他' }}</el-tag>
            <el-tag v-if="roleOf(s.name)" size="small" type="danger" effect="plain">{{ roleOf(s.name) }}</el-tag>
            <span class="svc-name">{{ s.name }}</span>
            <span v-if="s.unitPort" class="svc-port">:{{ s.unitPort }}</span>
            <el-tooltip v-if="s.artifact" content="含随包制品，可 systemd 裸机部署"><el-icon class="ok-icon"><CircleCheck /></el-icon></el-tooltip>
            <el-tooltip v-if="s.kind === 'comp' && topoFormsOf(s.name).length" content="组态多实例拓扑（如 MySQL 主从：包只出素材，拓扑安装时组态，免回 studio 重打包）" placement="top">
              <el-button link type="primary" size="small" style="margin-left: 2px" @click.stop="openTopo(s.name)">组态</el-button>
            </el-tooltip>
            <el-tooltip v-if="s.kind === 'comp' && !instBySvc[s.name]" content="整组跳过：不安装也不复用外部实例（对应配置段剔除、变量与端点免填）" placement="top">
              <el-icon v-if="!compSkipped(s.name)" class="skip-icon" @click.stop="toggleSkipComp(s.name)"><CircleClose /></el-icon>
            </el-tooltip>
            <el-tag v-if="compSkipped(s.name)" size="small" type="info" effect="dark" style="cursor: pointer" @click.stop="toggleSkipComp(s.name)">已跳过</el-tag>
          </div>
          <div class="pane-tip">单元服务必须分配到主机；组件三态：分配安装 / 不分配=复用外部实例（第③步手填端点）/ 点 ⊘ 整组跳过（不装不复用，变量免填）。带「组态」的组件可展开多实例拓扑（主从等），实例服务需分别拖到不同主机</div>
        </div>
        <div class="host-grid">
          <div v-for="h in hosts" :key="h.id" class="host-card" :class="{ drop: dragOver === h.id }"
               @dragover.prevent="dragOver = h.id" @dragleave="dragOver = 0" @drop.prevent="drop(h)">
            <div class="host-head">
              <b>{{ h.name }}</b>
              <el-tag size="small" :type="h.online ? 'success' : 'info'">{{ h.online ? '在线' : '离线' }}</el-tag>
              <span class="host-addr">{{ h.addr }}</span>
            </div>
            <div class="host-caps">
              <el-tag size="small" :type="h.info.docker ? 'success' : placement.dockerInstall ? 'warning' : 'danger'" effect="plain">
                {{ h.info.docker ? 'docker' + (h.info.dockerVersion ? ' ' + h.info.dockerVersion : '') : '无 docker' }}
              </el-tag>
              <el-tag size="small" :type="h.info.systemd ? 'success' : 'info'" effect="plain">systemd</el-tag>
              <el-tag v-if="h.info.javaVersion" size="small" type="success" effect="plain">java</el-tag>
            </div>
            <div v-if="hostHolders(h).length" class="host-held">
              <span class="held-title">占用端口</span>
              <el-tooltip v-for="c in hostHolders(h)" :key="c.name" placement="top"
                          :content="(c.hostPorts || []).join('/') + ' ← ' + c.name + (c.image ? '（' + c.image + '）' : '') + (holderLevel(h, c) === 'reuse' ? ' · 本项目容器：升级将原地接管重建' : holderLevel(h, c) === 'legacy' ? ' · 本应用历史遗留：可清理后安装' : '')">
                <el-tag size="small" effect="plain" class="held-tag"
                        :type="holderLevel(h, c) === 'reuse' ? 'success' : holderLevel(h, c) === 'legacy' ? 'warning' : 'info'">
                  {{ (c.hostPorts || []).join('/') }} ← {{ c.name }}
                </el-tag>
              </el-tooltip>
              <el-button v-if="hostHolders(h).some((c) => holderLevel(h, c) !== 'reuse')" link type="danger" size="small" @click.stop="cleanupHolders(h)">
                清理遗留/冲突容器
              </el-button>
            </div>
            <div class="host-svcs">
              <div v-for="s in svcsOn(h.id)" :key="s.name" class="placed"
                   :class="placedLevel(h, s) ? 'placed-' + placedLevel(h, s) : ''"
                   :title="placedTitle(h, s)">
                <el-tag v-if="placedLevel(h, s) === 'reuse'" size="small" type="success" effect="plain">{{ takeoverLabel }}</el-tag>
                <el-tag v-else-if="placedLevel(h, s) === 'legacy'" size="small" type="warning" effect="plain">遗留占用</el-tag>
                <el-tag v-else-if="placedLevel(h, s) === 'conflict'" size="small" type="danger" effect="plain">端口冲突</el-tag>
                <el-tag v-if="resOf(s.name)" size="small" type="info" effect="plain" @click.stop="openRes(s.name)" style="cursor: pointer">{{ resLabel(s.name) }}</el-tag>
                <span>{{ s.name }}</span>
                <el-select v-if="s.kind === 'unit' && s.artifact && h.info.systemd && h.info.javaVersion"
                           :model-value="svcMode(s.name)" size="small" style="width: 92px"
                           @update:model-value="(v) => (placement.mode[s.name] = v)" @click.stop>
                  <el-option value="docker" label="docker" />
                  <el-option value="systemd" label="systemd" />
                </el-select>
                <el-tooltip content="编辑资源限额（CPU/内存）与端口覆盖（实例）" placement="top">
                  <el-icon class="rm" style="color: var(--el-color-primary)" @click.stop="openRes(s.name)"><Setting /></el-icon>
                </el-tooltip>
                <el-icon class="rm" @click.stop="unassign(s.name)"><Close /></el-icon>
              </div>
              <div v-if="!svcsOn(h.id).length" class="empty">拖服务到此处</div>
            </div>
          </div>
        </div>
      </div>
      <div class="warn-zone">
        <el-alert v-for="(w, i) in assignWarnings" :key="i" :title="w" type="warning" :closable="false" class="warn" />
      </div>
      <div class="actions">
        <el-button @click="step = 0">上一步</el-button>
        <el-button type="primary" @click="toVars" :disabled="unassignedUnits.length > 0"
                   :title="unassignedUnits.length ? '单元服务未分配主机：' + unassignedUnits.join('、') : ''">下一步：变量与端点</el-button>
      </div>
    </el-card>

    <!-- ③ 变量与外部端点 -->
    <el-drawer v-model="varsOpen" size="560px" title="③ 变量与外部端点" :close-on-click-modal="false">
      <el-form label-width="180px">
        <template v-for="v in inputVars" :key="v.name">
          <el-form-item :label="v.label || v.name">
            <el-input v-model="placement.vars[v.name]" :type="v.kind === 'cred' ? 'password' : 'text'" show-password />
            <div class="field-hint">{{ v.name }}<template v-if="v.kind === 'cred'">（{{ v.hint }}）</template></div>
          </el-form-item>
        </template>
      </el-form>
      <template v-if="extEndpoints.length">
        <el-divider>外部组件端点（未落机组件，手填实际地址）</el-divider>
        <el-form label-width="180px">
          <el-form-item v-for="ep in extEndpoints" :key="ep.placeholder" :label="ep.placeholder">
            <el-input v-model="placement.ext[ep.placeholder]" :placeholder="epHint(ep)" />
          </el-form-item>
        </el-form>
      </template>
      <div class="drawer-foot">
        <el-button @click="varsOpen = false">取消</el-button>
        <el-button type="primary" :loading="building" @click="buildDag">生成 DAG →</el-button>
      </div>
    </el-drawer>

    <!-- ④ DAG -->
    <el-drawer v-model="dagOpen" size="76%" title="④ 执行编排（DAG）——可增删节点/编辑/插入 cmd 与 gate" :close-on-click-modal="false">
      <div class="dag-toolbar">
        <el-button size="small" @click="addNode('cmd')">+ cmd 节点</el-button>
        <el-button size="small" @click="addNode('gate')">+ gate 节点</el-button>
        <el-button size="small" @click="regen">重置为自动 DAG</el-button>
        <el-button size="small" type="primary" plain :loading="previewing" @click="previewChanges">变更预览</el-button>
        <el-button size="small" type="success" :loading="starting" @click="start">⑤ 执行安装 →</el-button>
        <el-tag v-for="w in dagWarnings" :key="w" type="warning" size="small">{{ w }}</el-tag>
      </div>
      <div class="dag-tip">节点可拖动；从节点右侧锚点拖到另一节点左侧锚点 = 添加依赖（先完成后者才跑）；点选连线按 Delete = 删除依赖</div>
      <div class="dag-wrap">
        <VueFlow :nodes="dagNodes" :edges="dagEdges" fit-view-on-init
                 @connect="onConnect" @node-drag-stop="onDragStop" @edges-change="onEdgesChange">
          <Background /><Controls /><MiniMap />
          <template #node-custom="nodeProps">
            <DagNodeCard v-bind="nodeProps" :selected="selectedId === nodeProps.id"
                         @select="selectedId = $event" @edit="editNode" @remove="removeNode" />
          </template>
        </VueFlow>
      </div>
      <div class="dag-detail">
        <template v-if="selNode">
          <div class="detail-head">
            <el-tag size="small" :color="kindColor(selNode.kind)" style="border: none">{{ kindLabel(selNode.kind) }}</el-tag>
            <b>{{ selNode.title || selNode.id }}</b>
            <span class="detail-id">{{ selNode.id }}</span>
            <span class="detail-ops">
              <el-button size="small" @click="editNode(selNode.id)">编辑</el-button>
              <el-button size="small" type="danger" plain @click="removeNode(selNode.id); selectedId = ''">删除</el-button>
            </span>
          </div>
          <el-descriptions :column="3" size="small" border class="detail-desc">
            <el-descriptions-item label="类型">{{ kindLabel(selNode.kind) }}</el-descriptions-item>
            <el-descriptions-item label="主机">{{ selNode.host ? hostName(selNode.host) || selNode.host : '—' }}</el-descriptions-item>
            <el-descriptions-item label="依赖">{{ depTitles(selNode).length ? depTitles(selNode).join('、') : '无（起始节点）' }}</el-descriptions-item>
            <el-descriptions-item v-if="selNode.mode" label="模式" :span="3">{{ selNode.mode }}</el-descriptions-item>
            <el-descriptions-item v-if="selNode.note" label="说明" :span="3">{{ selNode.note }}</el-descriptions-item>
            <el-descriptions-item v-if="selNode.kind === 'cmd'" label="脚本" :span="3">
              <pre class="detail-script">{{ selNode.script || '（未填写）' }}</pre>
            </el-descriptions-item>
          </el-descriptions>
        </template>
        <div v-else class="detail-empty">点击节点查看详情（类型 / 主机 / 依赖 / 脚本）</div>
      </div>
    </el-drawer>
    <!-- 导入编排：设备映射（槽位 → 本环境主机） -->
    <el-dialog v-model="importDlg" title="导入编排 —— 为架构槽位选择设备" width="760px" :close-on-click-modal="false">
      <template v-if="importFile">
        <el-alert type="info" :closable="false" class="mb8"
                  :title="'部署包 ' + importFile.package.app + ' ' + importFile.package.version + '（导出于 ' + fmtImportTime(importFile.exportedAt) + '）——设备按下方映射选择后可继续调整'" />
        <el-table :data="slotRows" size="small">
          <el-table-column label="架构槽位" min-width="210">
            <template #default="{ row }">
              <b>{{ row.name }}</b>
              <div class="slot-caps">
                <el-tag size="small" effect="plain">{{ row.os || '?' }}/{{ row.arch || '?' }}</el-tag>
                <el-tag size="small" :type="row.docker ? 'success' : 'danger'" effect="plain">{{ row.docker ? 'docker' : '无docker' }}</el-tag>
                <el-tag v-if="row.systemd" size="small" type="success" effect="plain">systemd</el-tag>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="→ 本环境主机" min-width="250">
            <template #default="{ row }">
              <el-select v-model="slotMap[row.id]" size="small" style="width: 100%" placeholder="选择实际主机">
                <el-option v-for="h in hosts" :key="h.id" :value="h.id"
                           :label="h.name + '（' + h.info.os + '/' + h.info.arch + (h.info.docker ? ' ·docker' : '') + (h.online ? '' : ' ·离线') + '）'" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="提示" min-width="180">
            <template #default="{ row }">
              <span v-if="diffHint(row)" class="hint-warn">{{ diffHint(row) }}</span>
              <span v-else class="hint-ok">✓ 能力匹配</span>
            </template>
          </el-table-column>
        </el-table>
      </template>
      <template #footer>
        <el-button @click="importDlg = false">取消</el-button>
        <el-button type="primary" :disabled="!mapReady" @click="applyImport">应用并继续调整</el-button>
      </template>
    </el-dialog>

    <!-- 统一变更预览（与执行同引擎同口径：期望态渲染 × inventory 逐服务 diff） -->
    <el-dialog v-model="previewDlg" title="变更预览 —— 期望态 × 已装清单（与执行同一比对口径）" width="720px">
      <el-alert v-if="previewData?.mode === 'overwrite'" type="warning" :closable="false" class="mb8"
                title="覆盖重装：忽略比对结果全量重新物化执行（数据目录 ./data 保留）——下表差异仅供参考" />
      <el-alert v-else-if="previewData?.mode === 'fresh'" type="info" :closable="false" class="mb8"
                title="全量安装：全部服务视为新增" />
      <el-table v-if="previewData" :data="previewData.changes" size="small" max-height="420">
        <el-table-column prop="service" label="服务" min-width="140" />
        <el-table-column prop="host" label="主机" width="110" />
        <el-table-column label="动作" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="{ create: 'success', update: 'warning', remove: 'danger', nochange: 'info' }[row.action]" effect="plain">
              {{ { create: '新增', update: '变更', remove: '移除', nochange: '未变' }[row.action] }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="原因" min-width="220">
          <template #default="{ row }">{{ (row.reasons || []).join('；') || '—' }}</template>
        </el-table-column>
      </el-table>
      <div v-if="previewData" class="preview-sum">
        新增 {{ countAction('create') }} · 变更 {{ countAction('update') }} · 移除 {{ countAction('remove') }} · 未变 {{ countAction('nochange') }}
      </div>
    </el-dialog>

    <!-- 组态向导：把组件展开成多实例拓扑（主从等；包=素材、console 组态，免重打包） -->
    <el-dialog v-model="topoDlg" title="组态多实例拓扑" width="640px">
      <el-alert type="info" :closable="false" class="mb8"
                :title="`组件 ${topoComp} 展开为多实例拓扑——实例服务分别拖到不同主机（反亲和强制分机）；GTID 自动建复制、探针守护双线程就绪。单机→组态为全新数据目录（./data/<实例名>），原单实例数据不自动迁移。`" />
      <el-radio-group v-model="topoFormId" size="small" style="margin-bottom: 10px">
        <el-radio-button v-for="f in topoFormsOf(topoComp)" :key="f.id" :value="f.id">{{ f.title }}</el-radio-button>
      </el-radio-group>
      <el-table v-if="curTopoForm" :data="curTopoForm.instances" size="small">
        <el-table-column prop="service" label="实例服务" min-width="180" />
        <el-table-column prop="roleTitle" label="角色" width="80" />
        <el-table-column prop="port" label="宿主端口" width="90" />
        <el-table-column prop="serverId" label="server-id" width="100" />
      </el-table>
      <template #footer>
        <el-button v-if="instancedComps.has(topoComp)" @click="clearTopo">恢复单机（取消组态）</el-button>
        <el-button @click="topoDlg = false">取消</el-button>
        <el-button type="primary" :disabled="!topoFormId" @click="applyTopo">生成实例</el-button>
      </template>
    </el-dialog>

    <!-- 服务参数编辑：资源限额（CPU/内存，K8s resources 对应物）+ 实例端口覆盖 -->
    <el-dialog v-model="resDlg" :title="`编辑 ${resSvc} 的参数`" width="460px">
      <el-form label-width="130px">
        <el-form-item label="CPU 限额（核）">
          <el-input v-model="resForm.cpus" placeholder="如 2 / 0.5（留空=不限）" />
        </el-form-item>
        <el-form-item label="内存上限（MB）">
          <el-input v-model="resForm.memMB" placeholder="如 4096（留空=不限）" />
        </el-form-item>
        <el-form-item v-if="instBySvc[resSvc]" label="宿主端口覆盖">
          <el-input v-model="resForm.port" :placeholder="instBySvc[resSvc].port || '3306'" />
        </el-form-item>
      </el-form>
      <div class="field-hint">资源限额物化为 compose deploy.resources.limits（改限额经精确 diff 只重建本服务）；端口覆盖仅组态实例支持（改宿主发布端口）</div>
      <template #footer>
        <el-button @click="resDlg = false">取消</el-button>
        <el-button type="primary" @click="applyRes">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { VueFlow } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import { MiniMap } from '@vue-flow/minimap'
import { api, fmtTime } from '../api'
import { gotoView, designPkgId } from '../nav'
import DagNodeCard from '../components/DagNodeCard.vue'

const step = ref(0)
const pkgs = ref([])
const hosts = ref([])
const pkg = ref(null)
const detailData = ref(null)
const mode = ref('fresh')
const dragging = ref('')
const dragOver = ref(0)
const varsOpen = ref(false)
const dagOpen = ref(false)
const building = ref(false)
const starting = ref(false)
const dagNodes = ref([])
const dagEdges = ref([])
const dagWarnings = ref([])
const selectedId = ref('')
// 手动拖动后的节点坐标缓存（id→{x,y}）：结构变化重渲染不丢手动布局；regen 清空
const posCache = new Map()
const assignWarnings = computed(() => crossHostWarnings())

const placement = reactive({ mode: {}, host: {}, vars: {}, ext: {}, skip: [], dockerInstall: false, instances: [], resources: {} })
let dagModels = []
let pendingDag = null // 导入编排携带的 DAG（进入第④步时优先于 autodag 采用，保留设计调整）

// 导入编排状态
const fileInput = ref(null)
const importDlg = ref(false)
const importFile = ref(null)
const slotMap = reactive({}) // 槽位 id（h{原主机ID}）→ 本库主机 id

// ---- console 组态器（包=素材、安装时组态拓扑，ADR-11）：实例合并进服务面板 ----
const instancedComps = computed(() => new Set((placement.instances || []).map((i) => i.component)))
const instBySvc = computed(() => {
  const m = {}
  for (const i of placement.instances || []) m[i.service] = i
  return m
})
function formInstancesOf(compCode, formId) {
  for (const f of detailData.value?.topologyForms?.[compCode] || []) {
    if (!formId || f.id === formId) return f
  }
  return null
}
// 实例缺省端口（形态声明）；组件无形态（不匹配内置模板）返回空
function topoFormsOf(svcName) {
  const code = compCodeOf(svcName)
  return code ? (detailData.value?.topologyForms?.[code] || []) : []
}
const services = computed(() => {
  const pkgSvcs = detailData.value?.services || []
  const hidden = instancedComps.value
  const out = pkgSvcs.filter((s) => {
    const code = compCodeOf(s.name)
    return !(code && hidden.has(code))
  })
  for (const inst of placement.instances || []) {
    const f = formInstancesOf(inst.component, inst.form)
    const spec = (f?.instances || []).find((x) => x.service === inst.service)
    out.push({
      name: inst.service, owner: 'comp:' + inst.component, kind: 'comp', primary: inst.role === 'primary',
      image: '', ports: spec ? { '3306': inst.port || spec.port } : {}, deps: [], instance: true,
    })
  }
  return out
})
const selNode = computed(() => dagModels.find((n) => n.id === selectedId.value) || null)
const inputVars = computed(() => {
  const out = []
  for (const v of detailData.value?.decl?.variables || []) {
    if (v.source === 'input') out.push({ name: v.name, label: v.label, kind: 'var', hint: '' })
  }
  for (const c of detailData.value?.decl?.components || []) {
    if (placement.skip.includes(c.code)) continue // 跳过组件：凭据免填
    const placed = (c.services || []).some((s) => placement.host[s])
    for (const cred of c.credentials || []) {
      if (cred.generate === 'ask' || (!placed && cred.generate !== 'random')) {
        out.push({ name: cred.placeholder, label: cred.label || (c.code + ' 凭据'), kind: 'cred', hint: cred.generate === 'ask' ? '留空=随机' : '固定值' })
      }
    }
  }
  return out
})
const extEndpoints = computed(() => {
  const out = []
  for (const c of detailData.value?.decl?.components || []) {
    if (placement.skip.includes(c.code)) continue // 跳过组件：端点免填
    const placed = (c.services || []).some((s) => placement.host[s])
    if (placed) continue
    for (const ep of c.endpoints || []) out.push({ placeholder: ep.placeholder, from: ep.from, port: ep.port })
  }
  return out
})
const unassignedUnits = computed(() =>
  services.value.filter((s) => s.kind === 'unit' && !placement.host[s.name]).map((s) => s.name)
)

function epHint(ep) {
  if (ep.from === 'port') return '外部端口（如 3307）'
  if (ep.from === 'host') return '主机地址（如 192.168.1.100）'
  return '地址:端口（如 192.168.1.100:3306）'
}

function svcsOn(hostId) {
  return services.value.filter((s) => placement.host[s.name] === hostId)
}
// 与后端 sanitizeSeg 同规则（console pkgload.go）：小写去空白、只留 [a-z0-9._-]，空回退 app
function sanitizeSegJs(s) {
  const seg = String(s || '').trim().toLowerCase().replace(/[^a-z0-9._-]/g, '')
  return seg || 'app'
}
// 当前包在该主机上的 compose 项目名（engine.go projOf 同式：seg + -h + 主机ID）
function projectOf(h) {
  return sanitizeSegJs(pkg.value?.app) + '-h' + h.id
}
function appSeg() {
  return sanitizeSegJs(pkg.value?.app) + '-h'
}
// 占用者三档（K8s apply 接管口径）：本项目=升级接管（原地重建，数据保留）；同包历史=旧主机
// 分配/包名变更遗留（可清理）；其余=异源冲突（清理需确认不属于他人在用的服务）
function holderLevel(h, c) {
  const proj = c.project || ''
  if (proj && proj === projectOf(h)) return 'reuse'
  if (proj && proj.startsWith(appSeg())) return 'legacy'
  return 'foreign'
}
// 主机已发布端口的运行容器（agent 心跳端口档案——分配阶段占用预览；暂无档案（agent 较旧/心跳
// 未达）不展示，建会话校验会降级提示并以执行期自检兜底）
function hostHolders(h) {
  return (h.info?.containers || []).filter((c) => (c.hostPorts || []).length)
}
// 服务发布到该主机时的端口占用三档分类（提示性预览；权威拦截/放行以建会话校验为准）
function portConflictOn(h, svc) {
  const held = {}
  for (const c of h.info?.containers || []) {
    for (const p of c.hostPorts || []) if (!held[p]) held[p] = c
  }
  const out = { reuse: [], legacy: [], foreign: [] }
  for (const p of Object.values(svc.ports || {})) {
    const c = held[p]
    if (!c) continue
    out[holderLevel(h, c)].push(`${p} ← ${c.name}${c.image ? '（' + c.image + '）' : ''}`)
  }
  return out
}
// 服务在该主机上是升级接管还是被拦（placed 行着色/提示用）
function placedLevel(h, svc) {
  const cls = portConflictOn(h, svc)
  if (cls.foreign.length) return 'conflict'
  if (cls.legacy.length) return 'legacy'
  if (cls.reuse.length) return 'reuse'
  return ''
}
function placedTitle(h, svc) {
  const cls = portConflictOn(h, svc)
  if (cls.reuse.length && !cls.legacy.length && !cls.foreign.length) {
    return '主机已有本项目容器：本次安装将原地升级重建变更服务（数据保留）'
  }
  const blocked = [...cls.legacy, ...cls.foreign]
  return blocked.length
    ? '宿主端口被占用：' + blocked.join('、') + (cls.legacy.length ? '（本应用历史遗留，可清理后安装）' : '') + '——发起安装将被预检拦截'
    : ''
}
function svcMode(name) {
  return placement.mode[name] || 'docker'
}
// 组件三态：分配安装 / 外部复用（不分配）/ 整组跳过（不装不复用——配置段剔除、变量端点免填）
function compCodeOf(svc) {
  for (const c of detailData.value?.decl?.components || []) {
    if ((c.services || []).includes(svc)) return c.code
  }
  return ''
}
function compSkipped(svc) {
  const code = compCodeOf(svc)
  return !!code && placement.skip.includes(code)
}
function toggleSkipComp(svc) {
  const code = compCodeOf(svc)
  if (!code) return
  const i = placement.skip.indexOf(code)
  if (i >= 0) {
    placement.skip.splice(i, 1)
  } else {
    placement.skip.push(code)
    for (const c of detailData.value?.decl?.components || []) {
      if (c.code !== code) continue
      for (const s of c.services || []) unassign(s)
    }
  }
}
function drop(h) {
  dragOver.value = 0
  if (!dragging.value) return
  const svc = services.value.find((x) => x.name === dragging.value)
  // 拖动=想安装：若组件处于跳过态先解除
  const code = compCodeOf(dragging.value)
  if (code && placement.skip.includes(code)) {
    placement.skip = placement.skip.filter((c) => c !== code)
  }
  placement.host[dragging.value] = h.id
  if (placement.mode[dragging.value] === undefined) placement.mode[dragging.value] = 'docker'
  // 端口占用分级提示（分配阶段前置反馈；权威拦截以建会话校验为准）
  if (svc) {
    const cls = portConflictOn(h, svc)
    const blocked = [...cls.legacy, ...cls.foreign]
    if (cls.reuse.length && !blocked.length) {
      ElMessage.info(`「${svc.name}」：主机已有本项目容器，本次安装将原地升级重建变更服务（数据保留）`)
    } else if (blocked.length) {
      ElMessage.warning(`「${svc.name}」宿主端口被占用：${blocked.join('、')}${cls.legacy.length ? '（本应用历史遗留，可清理后安装）' : ''}——发起安装将被预检拦截`)
    }
  }
  dragging.value = ''
}
function unassign(name) {
  delete placement.host[name]
  delete placement.mode[name]
}

function crossHostWarnings() {
  const out = []
  for (const s of services.value) {
    const myHost = placement.host[s.name]
    if (!myHost) continue
    for (const dep of s.deps || []) {
      const depHost = placement.host[dep]
      const depSvc = services.value.find((x) => x.name === dep)
      if (!depSvc) continue
      if (!depHost) {
        out.push(s.name + ' 依赖的 ' + dep + ' 未落机（外部复用）——请确认第三步端点已填')
        continue
      }
      if (depHost !== myHost && depSvc.kind !== 'unit' && !Object.keys(depSvc.ports || {}).length) {
        out.push(s.name + '（主机' + myHost + '）跨机依赖 ' + dep + '（主机' + depHost + '），但 ' + dep + ' 未发布端口——跨机访问会不通，请在组件编排块加 ports')
      }
    }
  }
  // 反亲和预检（spreadHosts 组件：同组件服务必须分机——主从同机即单点；发起安装会硬拦）
  for (const c of detailData.value?.decl?.components || []) {
    if (!c.spreadHosts) continue
    const byHost = {}
    for (const s of c.services || []) {
      const h = placement.host[s]
      if (h) (byHost[h] = byHost[h] || []).push(s)
    }
    for (const [h, svcs] of Object.entries(byHost)) {
      if (svcs.length > 1) {
        out.push(`组件 ${c.code} 声明了反亲和（主从等）：${svcs.join('、')} 同落主机 ${hostName(Number(h)) || h}——发起安装将被硬拦，请分机分配`)
      }
    }
  }
  return [...new Set(out)]
}

// 角色归属（format:4 角色化组件 / console 组态实例：主从 primary/replica 等标签展示）
function roleOf(svc) {
  const inst = instBySvc.value[svc]
  if (inst) return inst.role
  for (const c of detailData.value?.decl?.components || []) {
    for (const r of c.roles || []) {
      if ((r.services || []).includes(svc)) return r.name
    }
  }
  return ''
}

async function loadPkgs() {
  const res = await api.get('/api/packages')
  pkgs.value = res.packets || res.packages || []
}
async function loadHosts() {
  const res = await api.get('/api/hosts')
  hosts.value = (res.hosts || []).filter((h) => !h.revoked)
}

async function pickPkg(row) {
  try {
    detailData.value = await api.get('/api/packages/' + row.id)
    pkg.value = row
    // 默认不预分配：拖拽分配交给用户（此前默认全部塞本机，多主机场景极易误装）
    placement.host = {}
    placement.mode = {}
    placement.vars = {}
    placement.ext = {}
    placement.skip = []
    placement.instances = []
    placement.resources = {}
    placement.dockerInstall = false
    evalUpgrade()
    step.value = 1
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// 升级感知：diff 预览（对已装清单 inventory）检测到已有部署 → 升级横幅引导 + 默认切升级模式
//（apply 语义：未变更主机整组跳过、变更服务原地重建；预览为 blockHash/image 粗粒度，
//  变量与配置变更在发起安装时精确比对）
const upgradeInfo = ref(null)
const upgradeDetail = computed(() => {
  const u = upgradeInfo.value
  if (!u) return ''
  const parts = []
  if (u.changed.length) parts.push('变更：' + u.changed.join('、'))
  if (u.added.length) parts.push('新增：' + u.added.join('、'))
  if (u.removed.length) parts.push('移除：' + u.removed.join('、'))
  parts.push('未变：' + (u.unchanged.length || 0) + ' 个服务（跳过）')
  return parts.join('；') + '。变量与配置变更在发起安装时精确比对'
})
function evalUpgrade() {
  upgradeInfo.value = null
  const d = detailData.value?.diff
  if (!d) return
  const changed = d.changed || [], added = d.added || [], removed = d.removed || [], unchanged = d.unchanged || []
  if (unchanged.length + changed.length + removed.length === 0) return // 全新应用（无已装记录）
  upgradeInfo.value = { added, changed, removed, unchanged }
  mode.value = 'incremental'
}

// ---- 一键清理占用容器（复用主机远程执行通道：单节点 cmd 会话，脚本随会话留档可追溯） ----

// 当前分配下的可清理占用容器（按主机聚合；发起安装被拒后「清理并重试」用）——本项目容器
// 是升级接管对象，不清；仅同包遗留与异源
function conflictHosts() {
  const out = []
  for (const h of hosts.value) {
    const svcs = svcsOn(h.id)
    if (!svcs.length) continue
    const need = new Set(svcs.flatMap((s) => Object.values(s.ports || {})))
    const list = (h.info?.containers || []).filter((c) =>
      holderLevel(h, c) !== 'reuse' && (c.hostPorts || []).some((p) => need.has(p)))
    if (list.length) out.push({ host: h, containers: list })
  }
  return out
}

// 执行清理并等待完成（agent 任务后即时心跳，端口档案随 done 刷新）
async function execCleanup(hostId, containers) {
  const names = containers.map((c) => c.name).join(' ')
  const res = await api.post('/api/exec', {
    hostIds: [hostId], script: `docker rm -f ${names}`,
    title: `清理端口冲突容器（${containers.length} 个）`, timeoutSec: 120
  })
  const item = (res.items || [])[0]
  if (!item?.sessionId) throw new Error(item?.error || '执行会话创建失败')
  for (let i = 0; i < 90; i++) {
    const d = await api.get('/api/sessions/' + item.sessionId)
    const st = d.session?.status
    if (st === 'done') return
    if (st === 'failed' || st === 'aborted') throw new Error('清理执行失败——见安装记录日志')
    await new Promise((r) => setTimeout(r, 2000))
  }
  throw new Error('等待清理超时——见安装记录进度')
}

// 确认弹窗 + 执行（主机卡入口）；bind 挂数据保留在 data/ 目录、named volume 不删除
async function cleanupHolders(h) {
  const list = (h.info?.containers || []).filter((c) => (c.hostPorts || []).length && holderLevel(h, c) !== 'reuse')
  if (!list.length) return
  const hasForeign = list.some((c) => holderLevel(h, c) === 'foreign')
  const lines = list.map((c) => `· ${c.name}（${c.image || '?'}${c.project ? '，项目 ' + c.project : ''}，端口 ${(c.hostPorts || []).join('/')}）`).join('\n')
  try {
    await ElMessageBox.confirm(
      `将执行 docker rm -f 删除以下容器：\n${lines}${hasForeign ? '\n\n⚠ 其中包含不属于本应用的容器——请确认可以删除！' : ''}\n\n容器删除后 bind 挂数据仍保留在主机 data/ 目录；named volume 不删除。`,
      `清理主机 ${h.name} 的占用容器（${list.length} 个）`,
      { confirmButtonText: '清理', cancelButtonText: '取消', type: hasForeign ? 'error' : 'warning' }
    )
  } catch { return }
  try {
    await execCleanup(h.id, list)
    await loadHosts()
    ElMessage.success('清理完成——端口档案已刷新，可继续分配/发起安装')
  } catch (e) {
    ElMessage.error('清理失败：' + e.message)
  }
}

function toVars() {
  for (const v of detailData.value?.decl?.variables || []) {
    if (v.source === 'input' && placement.vars[v.name] === undefined) {
      placement.vars[v.name] = detailData.value?.prefill?.[v.name] ?? v.value ?? ''
    }
  }
  varsOpen.value = true
}

async function buildDag() {
  building.value = true
  try {
    const res = await api.post('/api/schemes/autodag', {
      packageId: pkg.value.id, placement: { ...placement }, mode: mode.value
    })
    dagModels = res.dag || []
    dagWarnings.value = res.warnings || []
    if (pendingDag) {
      // 导入编排的 DAG 优先（保留 cmd/gate/连线等设计调整；key 已按本库主机重写，引擎按 key
      // 对齐复用物化载荷）。与当前分配对不上的自动节点剔除（改动分配后可「重置为自动 DAG」重生成）
      const autoIds = new Set(dagModels.map((n) => n.id))
      const keep = pendingDag.filter((n) => !AUTO_KINDS.includes(n.kind) || autoIds.has(n.id))
      const dropped = pendingDag.length - keep.length
      const keptIds = new Set(keep.map((n) => n.id))
      dagModels = keep.map((n) => ({ ...n, deps: (n.deps || []).filter((d) => keptIds.has(d)) }))
      pendingDag = null
      if (dropped > 0) {
        ElMessage.warning(`导入 DAG 中 ${dropped} 个自动节点与当前分配不符，已剔除（可点「重置为自动 DAG」重生成）`)
      }
    }
    if (mode.value === 'incremental' && !dagModels.length) {
      ElMessage.warning('增量计划为空：全部服务与配置均未变化')
    }
    renderDag()
    varsOpen.value = false
    dagOpen.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    building.value = false
  }
}

const AUTO_KINDS = ['load', 'compose', 'systemd', 'wait', 'plugin', 'docker']

// ---- 统一变更预览（POST /api/sessions/preview：期望态渲染 × inventory 逐服务 diff，与执行同口径） ----
const previewDlg = ref(false)
const previewing = ref(false)
const previewData = ref(null)
function countAction(a) {
  return (previewData.value?.changes || []).filter((c) => c.action === a).length
}
async function previewChanges() {
  previewing.value = true
  try {
    previewData.value = await api.post('/api/sessions/preview', {
      packageId: pkg.value.id, placement: { ...placement }, mode: mode.value
    })
    previewDlg.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    previewing.value = false
  }
}

// ---- console 组态器：多实例拓扑向导 + 服务参数（资源限额/端口覆盖）编辑 ----
const topoDlg = ref(false)
const topoComp = ref('')
const topoFormId = ref('')
const curTopoForm = computed(() => formInstancesOf(topoComp.value, topoFormId.value))
function openTopo(svcName) {
  topoComp.value = compCodeOf(svcName)
  const forms = detailData.value?.topologyForms?.[topoComp.value] || []
  const cur = (placement.instances || []).find((i) => i.component === topoComp.value)
  topoFormId.value = cur ? cur.form : (forms[0]?.id || '')
  topoDlg.value = true
}
function applyTopo() {
  const f = curTopoForm.value
  if (!f) return
  clearTopo(true)
  for (const spec of f.instances || []) {
    placement.instances.push({ component: topoComp.value, form: f.id, service: spec.service, role: spec.role })
  }
  topoDlg.value = false
  ElMessage.success(`已组态 ${f.title}：${(f.instances || []).map((x) => x.service).join('、')}——请把实例分别拖到不同主机`)
}
function clearTopo(silent) {
  const code = topoComp.value
  for (const i of placement.instances.filter((x) => x.component === code)) {
    delete placement.host[i.service]
    delete placement.mode[i.service]
  }
  placement.instances = placement.instances.filter((x) => x.component !== code)
  if (!silent) {
    topoDlg.value = false
    ElMessage.success('已恢复单机形态（原服务重新可拖拽分配）')
  }
}

// 服务参数编辑：资源限额（K8s resources 对应物）+ 实例宿主端口覆盖
const resDlg = ref(false)
const resSvc = ref('')
const resForm = reactive({ cpus: '', memMB: '', port: '' })
function resOf(svc) { return placement.resources?.[svc] }
function resLabel(svc) {
  const r = resOf(svc)
  if (!r) return ''
  const parts = []
  if (r.cpus) parts.push(r.cpus + 'C')
  if (r.memMB) parts.push(Math.round(r.memMB / 1024 * 10) / 10 + 'G')
  return parts.join('/') || '已设'
}
function openRes(svc) {
  resSvc.value = svc
  const r = placement.resources?.[svc]
  resForm.cpus = r?.cpus || ''
  resForm.memMB = r?.memMB ? String(r.memMB) : ''
  resForm.port = instBySvc.value[svc]?.port || ''
  resDlg.value = true
}
function applyRes() {
  const mem = parseInt(resForm.memMB, 10)
  const r = { cpus: resForm.cpus.trim(), memMB: isNaN(mem) || mem <= 0 ? 0 : mem }
  if (r.cpus || r.memMB) placement.resources[resSvc.value] = r
  else delete placement.resources[resSvc.value]
  const inst = instBySvc.value[resSvc.value]
  if (inst) inst.port = resForm.port.trim()
  resDlg.value = false
}

// 同项目旧容器接管标签按模式区分措辞（fresh/覆盖重装同样接管同名容器原地重建，数据保留——
// 非模式失效；彻底干净安装请先在应用中心卸载或清理容器）
const takeoverLabel = computed(() => (mode.value === 'incremental' ? '升级接管' : '接管重建'))

function kindColor(kind) {
  return { docker: '#0db7ed', load: '#909399', compose: '#409eff', systemd: '#9254de', cmd: '#e6a23c', wait: '#67c23a', gate: '#f56c6c', plugin: '#13c2c2' }[kind] || '#909399'
}
function kindLabel(kind) {
  return { docker: '装 Docker', load: '装载', compose: '编排', systemd: '裸机', cmd: '命令', wait: '等待', gate: '门禁', plugin: '插件' }[kind] || kind
}

function renderDag() {
  const kindOrder = ['docker', 'load', 'compose', 'systemd', 'cmd', 'wait', 'plugin', 'gate']
  const byKind = {}
  for (const n of dagModels) (byKind[n.kind] = byKind[n.kind] || []).push(n)
  dagNodes.value = dagModels.map((n) => {
    const col = kindOrder.indexOf(n.kind)
    const row = byKind[n.kind].indexOf(n)
    const pos = posCache.get(n.id)
    return {
      id: n.id, type: 'custom', position: pos || { x: col * 230, y: row * 110 },
      data: { title: n.title || n.id, kind: n.kind, color: kindColor(n.kind), label: kindLabel(n.kind), host: hostName(n.host) }
    }
  })
  const ids = new Set(dagModels.map((n) => n.id))
  dagEdges.value = dagModels.flatMap((n) =>
    (n.deps || []).filter((d) => ids.has(d)).map((d) => ({
      id: d + '->' + n.id, source: d, target: n.id, animated: true
    }))
  )
}

function depTitles(n) {
  const byId = new Map(dagModels.map((x) => [x.id, x]))
  return (n.deps || []).map((d) => byId.get(d)?.title || d)
}

// 手动连线：source→target = target 依赖 source（先完成 source）。自环/重复/成环拒绝
function onConnect({ source, target }) {
  if (!source || !target || source === target) return
  const t = dagModels.find((n) => n.id === target)
  if (!t) return
  if ((t.deps || []).includes(source)) {
    ElMessage.warning('依赖已存在：' + source + ' → ' + target)
    return
  }
  if (wouldCreateCycle(source, target)) {
    ElMessage.warning('会产生循环依赖，已拒绝')
    return
  }
  t.deps = [...(t.deps || []), source]
  renderDag()
}

// source 已（传递）依赖 target 时，再加 source→target 即成环
function wouldCreateCycle(source, target) {
  const byId = new Map(dagModels.map((x) => [x.id, x]))
  const seen = new Set()
  const dfs = (id) => {
    if (id === target) return true
    if (seen.has(id)) return false
    seen.add(id)
    return (byId.get(id)?.deps || []).some(dfs)
  }
  return dfs(source)
}

function onDragStop(e) {
  if (e?.node?.id) posCache.set(e.node.id, { ...e.node.position })
}

// 删边（选中连线按 Delete）→ 同步摘除模型依赖
function onEdgesChange(changes) {
  let dirty = false
  for (const c of changes || []) {
    if (c.type !== 'remove') continue
    const idx = String(c.id || '').lastIndexOf('->')
    if (idx < 0) continue
    const source = c.id.slice(0, idx)
    const target = c.id.slice(idx + 2)
    const t = dagModels.find((n) => n.id === target)
    if (t && (t.deps || []).includes(source)) {
      t.deps = t.deps.filter((d) => d !== source)
      dirty = true
    }
  }
  if (dirty) renderDag()
}

function hostName(id) {
  return hosts.value.find((h) => h.id === id)?.name || ''
}

async function regen() {
  posCache.clear()
  selectedId.value = ''
  pendingDag = null
  await buildDag()
  varsOpen.value = false
}

function addNode(kind) {
  const i = dagModels.filter((n) => n.kind === kind).length + 1
  const id = kind + '-custom-' + i
  dagModels.push({ id, kind, title: kind === 'cmd' ? '自定义命令' : '人工确认', host: hosts.value[0]?.id || 0, script: '', deps: [], note: '' })
  renderDag()
}

async function editNode(id) {
  const n = dagModels.find((x) => x.id === id)
  if (!n) return
  try {
    if (n.kind === 'plugin') {
      // 配置中心插件节点：发布模式（idempotent=仅写缺失 / force=按基线覆盖线上差异）
      const { value } = await ElMessageBox.prompt(
        '发布模式：idempotent（缺省，仅写缺失项——保护线上手改）或 force（按基线覆盖差异项）',
        '编辑插件节点 ' + id, { inputValue: n.mode || 'idempotent' })
      const mode = (value || '').trim()
      if (mode !== 'idempotent' && mode !== 'force') {
        ElMessage.warning('模式须为 idempotent 或 force')
        return
      }
      n.mode = mode
      renderDag()
      return
    }
    const { value } = await ElMessageBox.prompt(
      undefined, (n.kind === 'cmd' ? '编辑命令节点 ' : '编辑节点 ') + id, {
        inputType: n.kind === 'cmd' ? 'textarea' : 'text',
        inputValue: n.kind === 'cmd' ? n.script : (n.note || n.title || '')
      })
    if (n.kind === 'cmd') n.script = value
    else { n.title = value; n.note = value }
    renderDag()
  } catch { /* 取消 */ }
}

async function removeNode(id) {
  dagModels = dagModels.filter((n) => n.id !== id)
  for (const n of dagModels) n.deps = (n.deps || []).filter((d) => d !== id)
  renderDag()
}

async function start() {
  if (dagModels.some((n) => n.kind === 'cmd' && !n.script)) {
    ElMessage.warning('存在未填脚本的 cmd 节点（点击节点编辑）')
    return
  }
  starting.value = true
  try {
    const res = await api.post('/api/sessions', {
      packageId: pkg.value.id, placement: { ...placement }, dag: dagModels, mode: mode.value
    })
    ElMessage.success('会话 #' + res.id + ' 已启动（' + res.mode + '，' + res.nodes + ' 节点）')
    dagOpen.value = false
    gotoView('Sessions')
  } catch (e) {
    // 建会话预检拒绝（端口冲突）：按当前分配重算可清理清单，提供「一键清理并重试」；
    // 清理经远程执行通道下发，agent 任务后即时刷新端口档案，重试即可通过预检
    if (e.message && e.message.includes('端口冲突')) {
      const pending = conflictHosts()
      const detail = pending.map((x) => `主机 ${x.host.name}：${x.containers.map((c) => c.name).join('、')}`).join('\n')
      try {
        await ElMessageBox.confirm(
          `${e.message}\n\n可一键清理以下占用容器后自动重试（bind 挂数据保留在主机 data/ 目录）：\n${detail}\n\n其中若含不属于本应用的容器，请确认可以删除。`,
          '安装未发起（端口预检未通过）',
          { confirmButtonText: '一键清理并重试', cancelButtonText: '取消', type: 'warning' }
        )
      } catch { return }
      try {
        await Promise.all(pending.map((x) => execCleanup(x.host.id, x.containers)))
        await loadHosts()
        ElMessage.success('占用容器已清理，自动重试发起安装……')
      } catch (e2) {
        ElMessage.error('清理未全部完成：' + e2.message)
        return
      }
      return start() // 清理完成自动重试（端口档案已刷新）
    }
    ElMessage.error(e.message)
  } finally {
    starting.value = false
  }
}

onMounted(async () => {
  await Promise.all([loadPkgs(), loadHosts()])
  // 部署包详情「去编排」带入预选包：命中即跳过选包步直接进入分配；包已删则留在选包步
  if (designPkgId.value) {
    const row = pkgs.value.find((p) => p.id === designPkgId.value)
    designPkgId.value = 0
    if (row) await pickPkg(row)
  }
})

// ---- 编排导出/导入（跨环境分发部署架构；文件不含镜像，包按 app+version 引用） ----

function sensitiveSet() {
  const set = new Set()
  for (const v of detailData.value?.decl?.variables || []) {
    if (v.sensitive) set.add(v.name)
  }
  for (const c of detailData.value?.decl?.components || []) {
    for (const cred of c.credentials || []) set.add(cred.placeholder)
  }
  return set
}

// slotize 收集分配与 DAG 引用的主机 → 槽位（id=h{主机ID}，带能力快照供导入方参考）
function slotize() {
  const ids = new Set()
  for (const svc in placement.host) ids.add(placement.host[svc])
  for (const n of dagModels || []) if (n.host) ids.add(n.host)
  const map = {}
  const slots = [...ids].sort((a, b) => a - b).map((id) => {
    const h = hosts.value.find((x) => x.id === id)
    map[id] = 'h' + id
    return {
      id: 'h' + id, name: h ? h.name : '（已删除主机 #' + id + '）',
      os: h?.info?.os || '', arch: h?.info?.arch || '',
      docker: !!h?.info?.docker, systemd: !!h?.info?.systemd,
      javaVersion: h?.info?.javaVersion || '', labels: h?.labels || ''
    }
  })
  return { slots, map }
}

function exportOrch() {
  if (!pkg.value || !detailData.value) return ElMessage.warning('请先在第①步选择部署包')
  if (unassignedUnits.value.length) return ElMessage.warning('尚有单元服务未分配：' + unassignedUnits.value.join('、'))
  const { slots, map } = slotize()
  const sens = sensitiveSet()
  const vars = { ...placement.vars }
  const redacted = []
  for (const k in vars) {
    if (sens.has(k)) { vars[k] = ''; redacted.push(k) }
  }
  const orch = {
    kind: 'mse-deliver-orchestration', format: 1, exportedAt: new Date().toISOString(),
    package: { app: detailData.value.app, version: detailData.value.version, pkgSha: detailData.value.pkgSha },
    dockerInstall: !!placement.dockerInstall,
    slots,
    placement: {
      mode: { ...placement.mode },
      host: Object.fromEntries(Object.entries(placement.host).map(([s, id]) => [s, map[id]])),
      vars, ext: { ...placement.ext },
      skip: [...placement.skip],
      instances: [...(placement.instances || [])],
      resources: JSON.parse(JSON.stringify(placement.resources || {}))
    },
    dag: (dagModels || []).map((n) => ({
      id: n.id, kind: n.kind, title: n.title || '', host: n.host ? map[n.host] : '',
      script: n.script || '', note: n.note || '', mode: n.mode || '', deps: n.deps || []
    })),
    redacted
  }
  const blob = new Blob([JSON.stringify(orch, null, 2)], { type: 'application/json' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `${detailData.value.app}-${detailData.value.version}-orchestration.json`
  a.click()
  URL.revokeObjectURL(a.href)
  ElMessage.success('已导出编排' + (redacted.length ? `（${redacted.length} 个敏感变量已置空，导入后需重填）` : ''))
}

function pickImport() { fileInput.value?.click() }

async function onImportFile(e) {
  const f = e.target.files?.[0]
  e.target.value = ''
  if (!f) return
  let orch
  try { orch = JSON.parse(await f.text()) } catch { return ElMessage.error('文件不是合法 JSON') }
  if (orch.kind !== 'mse-deliver-orchestration' || !orch.format) {
    return ElMessage.error('不是 deliver-console 编排导出文件（kind/format 不符）')
  }
  if (!orch.package?.app) return ElMessage.error('文件缺少包引用（package.app）')
  await Promise.all([loadPkgs(), loadHosts()])
  const target = pkgs.value.find((p) => p.app === orch.package.app && p.version === orch.package.version)
  if (!target) {
    ElMessage.error(`本库没有部署包 ${orch.package.app} ${orch.package.version}——请先在「部署包」页上传同名包再导入`)
    return
  }
  if (orch.package.pkgSha && target.pkgSha && orch.package.pkgSha !== target.pkgSha) {
    try {
      await ElMessageBox.confirm(
        `包指纹不一致（导出 ${orch.package.pkgSha.slice(0, 12)}… / 本库 ${target.pkgSha.slice(0, 12)}…，包内容有差异），仍继续导入？`,
        '包指纹不一致', { type: 'warning' })
    } catch { return }
  }
  importFile.value = orch
  for (const s of orch.slots || []) {
    const same = hosts.value.find((h) => h.name === s.name)
    slotMap[s.id] = same ? same.id : 0
  }
  importDlg.value = true
}

const slotRows = computed(() => importFile.value?.slots || [])
const mapReady = computed(() =>
  (importFile.value?.slots || []).length > 0 && importFile.value.slots.every((s) => slotMap[s.id]))

function diffHint(row) {
  const h = hosts.value.find((x) => x.id === slotMap[row.id])
  if (!h) return ''
  if (row.os && h.info.os !== row.os) return `系统不同（槽位 ${row.os} / 目标 ${h.info.os}）`
  if (row.docker && !h.info.docker) return '目标无 docker——可开启「自动装 Docker」'
  if (row.systemd && !h.info.systemd) return '目标无 systemd（裸机单元不可用）'
  return ''
}

function fmtImportTime(iso) {
  try { return new Date(iso).toLocaleString() } catch { return iso || '' }
}

async function applyImport() {
  const orch = importFile.value
  if (!mapReady.value) return ElMessage.warning('还有槽位未选择目标主机')
  try {
    const target = pkgs.value.find((p) => p.app === orch.package.app && p.version === orch.package.version)
    detailData.value = await api.get('/api/packages/' + target.id)
    pkg.value = target
    const toHost = {}
    for (const s of orch.slots || []) toHost[s.id] = slotMap[s.id]
    placement.mode = {}
    placement.host = {}
    placement.vars = {}
    placement.ext = {}
    placement.skip = [...(orch.placement?.skip || [])]
    placement.instances = [...(orch.placement?.instances || [])]
    placement.resources = { ...(orch.placement?.resources || {}) }
    placement.dockerInstall = !!orch.dockerInstall
    for (const [svc, sid] of Object.entries(orch.placement?.host || {})) {
      if (toHost[sid]) placement.host[svc] = toHost[sid]
    }
    for (const [s, m] of Object.entries(orch.placement?.mode || {})) placement.mode[s] = m
    for (const [k, v] of Object.entries(orch.placement?.vars || {})) placement.vars[k] = v || ''
    for (const [k, v] of Object.entries(orch.placement?.ext || {})) placement.ext[k] = v
    // DAG 预置：自动节点 key 内嵌旧主机号（load-h3 等）→ 按映射重写为本库主机号，
    // 会话创建时引擎按 key 对齐复用物化载荷；cmd/gate 等自定义节点仅重挂主机
    const oldToNew = {}
    for (const [sid, hid] of Object.entries(toHost)) {
      const old = Number(String(sid).replace(/^h/, ''))
      if (old && hid) oldToNew[old] = hid
    }
    const rwKey = (k) => {
      const m = /^(load|compose|docker-install)-h(\d+)(-base)?$/.exec(k || '')
      return m && oldToNew[m[2]] ? `${m[1]}-h${oldToNew[m[2]]}${m[3] || ''}` : k
    }
    pendingDag = null
    if (Array.isArray(orch.dag) && orch.dag.length) {
      pendingDag = orch.dag.map((n) => ({
        id: rwKey(n.id), kind: n.kind, title: n.title || '', host: toHost[n.host] || 0,
        script: n.script || '', note: n.note || '', mode: n.mode || '',
        deps: (n.deps || []).map(rwKey)
      }))
    }
    dagModels = []
    dagWarnings.value = []
    importDlg.value = false
    step.value = 1
    const redacted = (orch.redacted || []).length
    ElMessage.success('编排已导入——可继续调整分配；第③步' + (redacted ? `补填 ${redacted} 个敏感变量后` : '') + '生成/采用 DAG')
  } catch (e) {
    ElMessage.error(e.message)
  }
}
</script>

<style scoped>
.page-head { display: flex; align-items: baseline; gap: 12px; margin-bottom: 12px; }
.page-head h2 { margin: 0; font-size: 17px; }
.page-head .hint { flex: 1; }
.head-actions { display: flex; gap: 8px; align-items: center; }
.mb8 { margin-bottom: 8px; }
.preview-sum { margin-top: 8px; font-size: 12px; color: var(--el-text-color-secondary); }
.slot-caps { display: flex; gap: 4px; margin-top: 4px; }
.hint-warn { font-size: 12px; color: var(--el-color-warning); }
.hint-ok { font-size: 12px; color: var(--el-color-success); }
.steps { margin-bottom: 14px; }
.no-prefill-tip { margin-bottom: 12px; }
.card { margin-bottom: 14px; }
.row-between { display: flex; justify-content: space-between; align-items: center; }
.row-gap { display: flex; gap: 10px; align-items: center; }
.assign { display: flex; gap: 14px; }
.svc-pane { width: 250px; flex-shrink: 0; border: 1px dashed var(--el-border-color); border-radius: 6px; padding: 10px; }
.pane-title { font-size: 12px; color: var(--el-text-color-secondary); margin-bottom: 8px; }
.svc-item { display: flex; align-items: center; gap: 6px; padding: 5px 6px; border-radius: 4px; cursor: grab; margin-bottom: 4px; background: var(--el-fill-color-light); }
.svc-item.skipped { opacity: 0.55; cursor: pointer; text-decoration: line-through; }
.skip-icon { color: var(--el-text-color-placeholder); cursor: pointer; }
.skip-icon:hover { color: var(--el-color-warning); }
.svc-name { font-weight: 500; }
.svc-port { font-size: 11px; color: var(--el-text-color-secondary); }
.ok-icon { color: var(--el-color-success); }
.pane-tip { margin-top: 8px; font-size: 11px; color: var(--el-text-color-placeholder); }
.host-grid { flex: 1; display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 10px; align-content: start; }
.host-card { border: 1px solid var(--el-border-color); border-radius: 6px; padding: 10px; min-height: 150px; }
.host-card.drop { border-color: var(--el-color-primary); box-shadow: 0 0 0 2px var(--el-color-primary-light-8); }
.host-head { display: flex; gap: 8px; align-items: center; }
.host-addr { font-size: 11px; color: var(--el-text-color-secondary); }
.host-caps { display: flex; gap: 4px; margin: 6px 0; }
.host-held { display: flex; align-items: center; gap: 4px; flex-wrap: wrap; margin: 2px 0 4px; }
.held-title { font-size: 11px; color: var(--el-text-color-secondary); }
.held-tag { max-width: 100%; }
.host-svcs { min-height: 60px; }
.placed { display: flex; align-items: center; gap: 8px; padding: 3px 6px; border-radius: 4px; background: var(--el-color-primary-light-9); margin-bottom: 4px; cursor: pointer; }
.placed.conflict { background: var(--el-color-danger-light-9); outline: 1px dashed var(--el-color-danger); }
.placed-reuse { background: var(--el-color-success-light-9); }
.placed-legacy { background: var(--el-color-warning-light-9); outline: 1px dashed var(--el-color-warning); }
.placed-conflict { background: var(--el-color-danger-light-9); outline: 1px dashed var(--el-color-danger); }
.banner { margin-bottom: 10px; }
.placed span { flex: 1; font-size: 12px; }
.rm { color: var(--el-text-color-placeholder); }
.empty { font-size: 11px; color: var(--el-text-color-placeholder); text-align: center; padding-top: 24px; }
.warn-zone { margin-top: 10px; display: flex; flex-direction: column; gap: 6px; }
.warn { padding: 4px 10px; }
.actions { margin-top: 14px; text-align: right; }
.field-hint { font-size: 11px; color: var(--el-text-color-placeholder); }
.drawer-foot { margin-top: 14px; text-align: right; display: flex; gap: 8px; justify-content: flex-end; }
.dag-toolbar { display: flex; gap: 8px; align-items: center; margin-bottom: 10px; flex-wrap: wrap; }
.dag-tip { font-size: 11px; color: var(--el-text-color-placeholder); margin-bottom: 8px; }
.dag-wrap { height: 520px; border: 1px solid var(--el-border-color); border-radius: 6px; }
.dag-detail { margin-top: 10px; border: 1px solid var(--el-border-color); border-radius: 6px; padding: 10px; min-height: 60px; }
.detail-head { display: flex; gap: 10px; align-items: center; margin-bottom: 8px; }
.detail-head b { font-size: 13px; }
.detail-id { font-size: 11px; color: var(--el-text-color-placeholder); font-family: monospace; }
.detail-ops { margin-left: auto; display: flex; gap: 6px; }
.detail-desc { max-height: 200px; overflow: auto; }
.detail-script { margin: 0; font-family: monospace; font-size: 12px; white-space: pre-wrap; word-break: break-all; }
.detail-empty { font-size: 12px; color: var(--el-text-color-placeholder); text-align: center; padding: 14px 0; }
</style>
