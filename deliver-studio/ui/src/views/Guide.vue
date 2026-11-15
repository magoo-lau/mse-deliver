<template>
  <div class="guide">
    <!-- 锚点导航 -->
    <div class="guide-nav">
      <a v-for="s in sections" :key="s.id" :href="'#' + s.id" class="guide-nav-item">{{ s.title }}</a>
    </div>

    <el-card id="g-login" class="page-card" shadow="never">
      <template #header><span>登录与账号</span></template>
      <div class="guide-p">工作台启用<b>账号密码登录</b>（单管理员 <span class="mono">admin</span>）：首次启动自动生成随机密码并打印在<b>启动控制台</b>，登录后可在侧栏底部「改密码」修改；忘记密码用 <span class="mono">-reset-pass</span> 参数重启重新生成（旧密码与会话全部失效）。</div>
      <div class="guide-p">外部工具（collect-kit）推送物料时先 <span class="mono">POST /api/login</span> 换 token，后续请求带 <span class="mono">X-Studio-Token</span> 头（脚本参数 <span class="mono">--studio-user/--studio-pass</span> 自动完成）。会话有效期 7 天（滑动续期），重启工作台后需重新登录。</div>
    </el-card>

    <el-card id="g-terms" class="page-card" shadow="never">
      <template #header><span>术语表（全站统一口径）</span></template>
      <el-table :data="terms" border size="small">
        <el-table-column prop="name" label="术语" width="130" />
        <el-table-column prop="desc" label="含义" min-width="320" />
      </el-table>
    </el-card>

    <el-card id="g-concept" class="page-card" shadow="never">
      <template #header><span>核心概念：四类对象组成一次交付</span></template>
      <el-row :gutter="10">
        <el-col :span="6" v-for="c in concepts" :key="c.title">
          <div class="concept" :style="{ borderTopColor: c.color }">
            <div class="concept-title"><el-icon><component :is="c.icon" /></el-icon>{{ c.title }}</div>
            <div class="concept-desc">{{ c.desc }}</div>
          </div>
        </el-col>
      </el-row>
      <el-alert type="info" :closable="false" class="concept-alert"
        title="一句话：应用 = 若干服务单元（前端、后端、任意镜像都行）+ 变量清单；再按需勾选依赖组件（MySQL/Redis…）；打包产出离线部署包（format:3），交付 deliver-console 安装控制台编排安装。" />
    </el-card>

    <el-card id="g-quick" class="page-card" shadow="never">
      <template #header><span>快速开始（五分钟出第一个包）</span></template>
      <el-steps direction="vertical" :active="6" class="guide-steps">
        <el-step title="载入示例或新建应用" description="首页「载入 / 迁移数据」一键载入示例；或「应用定义」走新建向导（按模板加服务单元：Java 后端 / 静态前端 / 镜像包导入 / 镜像地址导出 / 空白）。" />
        <el-step title="上传制品" description="「制品库」上传应用包（jar / 前端 zip / 任意文件）、镜像包或附加文件；「镜像仓」可从本机 docker 直接导入。" />
        <el-step title="打包向导" description="「套件打包」页四步：① 选应用、勾依赖组件 ② 每个单元选镜像来源 ③ 填打包时变量 ④ 预览拓扑与包结构 → 开始打包。" />
        <el-step title="下载套件" description="打包日志实时滚动，完成后「下载套件」。全部镜像随包离线可用；可按单元勾选「制品随包附带」（artifacts/，供裸机部署）。套件不含租户变量/license，直接安装仅适用于测试环境。" />
        <el-step title="出交付包（交付场景）" description="正式交付给租户：『交付包』页上传签发方附件包（license + 租户数据 + 租户变量 + schema，选附件=选租户）→ 套件打包页套件行「出交付包」（跳转交付包页，弹窗已预选该套件与最新附件）或交付包页「出交付包」按钮 → 开始重打包 → 下载最终交付包（附件逐条目解压落位 delivery/ 与 extras/，console 表单自动预填租户变量）→ 交 deliver-console 安装。" />
        <el-step title="目标环境安装（deliver-console）" description="部署包不内嵌安装器二进制：上传到 deliver-console 安装控制台 → 主机池拖拽分配（agent 出站连接）→ 图形化 DAG 编排 → 一键执行/增量升级。单机全 Docker 场景可在向导第①步勾选「单机部署」随包附带 install.sh 薄壳，解压后一条命令首装。" />
      </el-steps>
    </el-card>

    <el-card id="g-unit" class="page-card" shadow="never">
      <template #header><span>服务单元：三种镜像来源</span></template>
      <el-table :data="unitSources" border size="small">
        <el-table-column prop="label" label="镜像来源" width="150" />
        <el-table-column prop="hint" label="说明" min-width="300" />
        <el-table-column label="需要准备" min-width="240">
          <template #default="{ row }">{{ row.need }}</template>
        </el-table-column>
      </el-table>
      <el-divider content-position="left">单元内置变量（模板里可直接用）</el-divider>
      <el-table :data="builtinVars" border size="small">
        <el-table-column label="变量引用" width="200">
          <template #default="{ row }"><span class="mono">{{ ph(row.name) }}</span></template>
        </el-table-column>
        <el-table-column prop="label" label="含义" />
      </el-table>
    </el-card>

    <el-card id="g-orch" class="page-card" shadow="never">
      <template #header><span>编排：图形与代码双模（谁依赖谁，一目了然）</span></template>
      <div class="guide-p">编排的唯一事实源是 compose 服务块文本（单元与组件各持一份），图形是它的投影——两种模式随时切换、始终一致：</div>
      <div class="guide-p">
        <b>① 服务块双模编辑</b>（应用定义 / 组件目录 · 「编排服务块」）：代码模式带补全（Ctrl+空格）——服务体键位（healthcheck 等整块模板）、
        depends_on 目标（其他单元与组件的服务名，标注来源）、镜像地址、<span class="mono ph">{{ ph('变量') }}</span> 与 <span class="mono">${env}</span>；
        图形模式节点=服务、连线=depends_on——拖线建依赖、点线删依赖、点节点改属性（镜像/端口/环境变量/依赖清单），改名级联同步。
      </div>
      <div class="guide-p">
        <b>② 套件编排画布</b>（应用定义 · 「编排」页签；打包向导第 4 步为只读预览）：应用单元 + 已勾选组件全量服务上图，实线=depends_on
        （拖线「单元 → 组件」= 写入启动依赖并自动勾选组件，点线删除）；虚线=配置接线（配置模板引用组件
        <span class="mono ph">{{ ph('占位符') }}</span> 的只读展示，不会自动改写配置）；红虚线=引用了未勾选组件（一键补勾选）。
      </div>
      <el-alert type="info" :closable="false"
        title="写回口径：依赖增删/改名是保格式文本手术（其余服务段与 {{X}} 原样不动）；属性面板编辑只规范化重排该服务段。depends_on 引用不存在的服务会被打包校验拦截。" />
    </el-card>

    <el-card id="g-var" class="page-card" shadow="never">
      <template #header><span>变量：一处定义，处处可查（变量字典）</span></template>
      <div class="guide-p">
        应用「变量」页签里显式登记变量（名称 / 说明 / 赋值时机 / 来源 / 是否敏感），模板中以 <span class="mono ph">{{ ph('NAME') }}</span> 引用。
        编辑模板时：变量芯片悬停看完整说明，下方「变量字典」列出全部可用变量（含组件端点/凭据与内置变量）并可一键插入；
        未定义的引用会标红，打包时后端再做一次硬校验，拼错直接拦截。
      </div>
      <el-table :data="varRules" border size="small">
        <el-table-column prop="when" label="赋值时机" width="110" />
        <el-table-column prop="source" label="来源方式" width="130" />
        <el-table-column prop="desc" label="行为" min-width="320" />
      </el-table>
      <el-alert type="warning" :closable="false"
        title="编排块（compose）里的运行期值请用 ${VAR:-默认} .env 写法（console/凭据同源）；{{X}} 写法在 Dockerfile 与编排块中仅支持内置变量与打包时变量。" />
    </el-card>

    <el-card id="g-pkg" class="page-card" shadow="never">
      <template #header><span>部署包结构与安装模式</span></template>
      <pre class="guide-pre">{{ layoutTree }}</pre>
      <el-divider content-position="left">安装（deliver-console 安装控制台）</el-divider>
      <div class="guide-p">部署包上传 deliver-console（上传即 manifest 强校验）→ 主机池分配服务到主机（单元可选 docker / systemd 裸机双形态——裸机需勾选「制品随包附带」且主机有 java；未落机组件=复用外部设施，端点手填）→ 自动生成 DAG（装载 → 编排 → 跨机健康门控 → SQL 预置 → 入口确认）→ 执行与增量升级（未变更主机整组跳过）。</div>
      <div class="guide-p">单机安装零 agent：console 所在机器即「本机节点」。目标机多机时拷入 deliver-agent 二进制 <span class="mono">install -server -token</span> 注册，agent 出站长轮询领任务，无需开放入站端口。任务幂等可重试；变量与凭据由 console 统一渲染下发。</div>
      <el-divider content-position="left">单机快捷通道（install.sh，可选附带）</el-divider>
      <div class="guide-p">打包向导第①步勾选「单机部署」即随包附带 <span class="mono">install.sh</span> 薄壳脚本：目标机解压后 <span class="mono">bash install.sh</span> 一条命令完成首装（manifest 逐文件校验 → docker load → 交互收集安装变量与凭据（<span class="mono">delivery/vars.env</span> 自动预填，ask 凭据留空随机）→ 渲染 .env 与 <span class="mono">data/conf/application.yaml</span> → compose up → 逐单元健康门控）。仅覆盖单机全 Docker 首装，可幂等重跑；增量升级/介质回退/多机/配置中心插件链仍走 deliver-console（勾选配置中心组件时打包会拦截该组合）。</div>
      <el-divider content-position="left">交付附加目录 delivery/（可选）</el-divider>
      <div class="guide-p">签发方附件包携带、本工作台「出交付包」重打包时落位 <span class="mono">delivery/</span> 目录（随最终交付包分发）：<span class="mono">vars.env</span> 预填安装变量与凭据（console 表单自动预填），其余文件由 console 拷至入口单元所在主机的配置目录（如随包签名的数据包，供应用启动时自动导入）。应用定义声明同名 install 变量（如 TENANT_*/LICENSE_PUBLIC_KEY）并在配置模板中引用，即可被附件 vars.env 预填命中；未声明的附件键只出一条提示，console 安装表单仍可手动输入（内嵌 demo 预设为完整接线样例）。</div>
    </el-card>

    <el-card id="g-collect" class="page-card" shadow="never">
      <template #header><span>对接 collect-kit（采集 / 镜像备料 → 一站出包；附件 → 出交付包）</span></template>
      <div class="guide-p">collect-kit v2 是通用可配置采集器（Go 单二进制 + recipe 插件，仓库行为全部来自 <span class="mono">recipes/&lt;name&gt;/</span>，ai-mse 仓库行为在 <span class="mono">recipes/ai-mse/</span>），本工作台是唯一打包引擎。动作均为子命令（推送需登录凭据 <span class="mono">--studio-user admin --studio-pass 密码</span>）：</div>
      <pre class="guide-pre"># ① 构建采集：钩子构建 fat-jar / 逐服务镜像 / UI zip，核心推送制品库 + 预设；
#    --package 一站式：推送后直接调打包 API 出套件 zip
collect-kit collect --recipe ai-mse --name x --version 1.0.0 --modules oa --ui \
    --studio http://127.0.0.1:7860 --studio-user admin --studio-pass *** --package

# ② 第三方镜像备料（独立动作，不构建）：recipe 声明式清单入镜像仓（跨机打包前）
collect-kit push-images --recipe ai-mse --middleware rocketmq \
    --studio http://127.0.0.1:7860 --studio-user admin --studio-pass ***

# 仅推组件预设 / 重推既有制品（免重建）
collect-kit upload-preset --recipe ai-mse --studio http://127.0.0.1:7860 --studio-pass ***
collect-kit repush    --recipe ai-mse --name x --version 1.0.0 --ui --studio http://127.0.0.1:7860 --studio-pass *** --package</pre>
      <div class="guide-p">推送完成后既可在「套件打包」页向导里打包，也可由采集器 <span class="mono">--package</span> 经 REST 直接出包（POST /api/packages → 轮询 → 下载）。**最终交付包也由本工作台产出**：签发方（如 ai-mse 平台「交付附件签发」）把附件包直推或上传到「交付包」页交付附件（POST /api/deliveries），经「出交付包」（套件打包页套件行按钮跳转 / 交付包页主按钮，弹窗选套件+附件）重打包即可；套件/交付包也可不带附件直接交付运行（无租户数据/license 叠加）。</div>
      <div class="guide-p">镜像获取顺序 = 本机 docker → 镜像仓 → 报缺：同机打包无需备料；跨机打包先跑 ② 备料，或在「镜像仓」页上传 tar。</div>
      <div class="guide-p">工作台对推送格式向下兼容：旧版预设（单应用混装前后端）导入时自动转换为新单元模型。</div>
    </el-card>

    <el-card id="g-faq" class="page-card" shadow="never">
      <template #header><span>常见问题</span></template>
      <el-collapse>
        <el-collapse-item v-for="f in faqs" :key="f.q" :title="f.q">
          <div class="guide-p">{{ f.a }}</div>
        </el-collapse-item>
      </el-collapse>
    </el-card>
  </div>
</template>

<script setup>
// 文本节点里直接写 {{X}} 字面量会被 Vue 插值提前截断，经 script 函数输出
const ph = (n) => '{{' + n + '}}'
const sections = [
  { id: 'g-login', title: '登录' },
  { id: 'g-terms', title: '术语表' },
  { id: 'g-concept', title: '核心概念' },
  { id: 'g-quick', title: '快速开始' },
  { id: 'g-unit', title: '服务单元' },
  { id: 'g-orch', title: '编排（双模）' },
  { id: 'g-var', title: '变量' },
  { id: 'g-pkg', title: '部署包与安装' },
  { id: 'g-collect', title: '对接 collect-kit' },
  { id: 'g-faq', title: '常见问题' }
]
const terms = [
  { name: '制品', desc: '打包素材总称，下分三类：应用包 / 镜像包 / 附加文件（页面「制品库」）' },
  { name: '应用包', desc: 'jar / 前端 zip / 任意构建产物文件，用于「应用包构建」现场出镜像' },
  { name: '镜像包', desc: 'docker save 出的 tar，直接进包不构建' },
  { name: '镜像地址', desc: 'docker 镜像引用 repo/name:tag（如 nginx:1.27）' },
  { name: '镜像来源', desc: '单元三选一：应用包构建 / 镜像包导入 / 镜像地址导出' },
  { name: '镜像名', desc: '应用包构建的产物镜像 repo（tag=打包版本，如 mse/my-app）' },
  { name: '镜像仓', desc: '本工作台的本地镜像包 tar 仓库（页面「镜像仓」）' },
  { name: '服务单元', desc: '一个可独立部署的容器服务：后端、前端、任意镜像均可，应用内可多个' },
  { name: '主入口', desc: '标记「入口」的单元：对外端口 + 健康检查 + 复用模式 docker run 目标（缺省第一个单元）' },
  { name: '变量', desc: '模板中 {{X}} 引用的显式定义：安装时赋值（询问/固定/随机）或打包时填值烘焙' },
  { name: '变量字典', desc: '模板编辑器下方的变量总表：名称/说明/来源/赋值时机，一键插入' },
  { name: '组件', desc: '应用依赖的第三方设施（MySQL/Redis/…）：声明镜像 + 编排块 + 端点/凭据，勾选即随包编排' },
  { name: '端点', desc: '组件对外服务地址的变量映射，console 按分配回填（同机=服务名 / 跨机=主机地址:端口 / 外部=手填）' },
  { name: '凭据', desc: '密码/账号类变量：console 表单询问（ask）/随机（random）/固定（const），同源渲染 .env 与配置' }
]
const concepts = [
  { icon: 'Monitor', color: '#409eff', title: '服务单元', desc: '一个可独立部署的容器服务：后端、前端、任意镜像均可；定义镜像来源、Dockerfile 模板、编排块与配置模板。' },
  { icon: 'Grid', color: '#e6a23c', title: '组件', desc: '应用依赖的第三方设施（MySQL/Redis/…）：声明镜像 + compose 服务块 + 端点/凭据，勾选即随包编排。' },
  { icon: 'Sell', color: '#67c23a', title: '变量', desc: '模板变量引用的显式定义：安装时询问/固定/随机，或打包时填值烘焙；变量字典处处可查、实时交叉校验。' },
  { icon: 'Files', color: '#909399', title: '制品', desc: '打包素材：应用包（jar/zip/任意文件）、镜像包、附加文件；打包时按单元选用。' }
]
const unitSources = [
  { label: '应用包构建', hint: '上传应用包 + Dockerfile 模板，打包时现场构建镜像（zip 自动解压）', need: '应用包（jar/zip/任意文件）+ Dockerfile 模板 + 镜像名' },
  { label: '镜像包导入', hint: 'docker save 出的镜像包 tar 直接进包，不构建', need: '镜像包（tar）' },
  { label: '镜像地址导出', hint: '如 nginx:1.27——打包时从本机 docker 或镜像仓导出', need: '镜像地址（打包时可改）' }
]
const builtinVars = [
  { name: 'UNIT_IMAGE', label: '本单元镜像（镜像名:版本 或 镜像包/镜像地址）' },
  { name: 'UNIT_PORT', label: '本单元端口' },
  { name: 'ARTIFACT_FILE', label: '应用包文件名（仅应用包构建的 Dockerfile 可用）' }
]
const varRules = [
  { when: '安装时', source: '人工填写（input）', desc: 'console 表单填写，值为默认（delivery/vars.env 自动预填）' },
  { when: '安装时', source: '固定值（const）', desc: '预设写死的值直接生效' },
  { when: '安装时', source: '随机生成（random）', desc: 'console 生成随机值，同源写 .env 与配置（增量升级自动沿用上次值）' },
  { when: '安装时', source: '敏感（sensitive）', desc: '标记敏感的值在界面掩码显示，console 表单密码框提示' },
  { when: '打包时', source: '人工填写 / 固定', desc: '打包向导里填值，直接烘进镜像与编排（如构建参数、目标环境名）' }
]
const layoutTree = `<部署包>/
├── docker-compose.yml       # 全栈编排（服务单元 + 勾选组件，console 按主机切分）
├── images/                  # 全部镜像包（单元镜像 + 组件镜像）
├── artifacts/…              # 随包制品（可选：单元勾选「制品随包附带」，systemd 裸机部署用）
├── components.json          # 驱动声明 format:3（单元端口/健康/制品 + 组件服务/镜像清单 + 变量/端点/凭据）
├── config-template.yaml     # 配置模板（安装时变量渲染，console 按主机视角回填端点）
├── extras/…                 # 附加文件（如 db/schema.sql，组件编排自挂 / console SQL 预置；交付附件 zip 禁入）
├── install.sh               # 单机安装脚本（可选：打包勾选「单机部署」附带，薄壳首装+健康门控）
├── manifest.json            # 逐文件 SHA-256（console 上传即强校验；install.sh 同样先校验再装）
└── delivery/                # 交付附加目录（平台层在包外追加：vars.env 预填 + 数据载荷）`
const faqs = [
  { q: '打包报「引用了未定义变量」怎么办？', a: '模板里写了 {{X}} 但变量清单没有 X——去应用「变量」页签登记同名变量（或检查拼写）；组件提供的变量（端点/凭据）在打包勾选该组件后自动生效。' },
  { q: '前端和后端怎么分开定义？', a: '在「应用定义」编辑器左侧单元列表里各建一个单元：前端单元选「静态前端（zip）」模板，后端单元选「Java 后端」模板，各自有独立的 Dockerfile 与编排块；单元数量不限，顺序即编排顺序。' },
  { q: '主入口是什么？', a: '标了「入口」的单元决定对外端口与健康检查，也是「复用外部设施」安装模式下 docker run 的目标。没有标记时默认第一个单元。' },
  { q: '应用包 / 镜像包 / 附加文件怎么区分？', a: '应用包=构建产物文件（jar/前端 zip/任意文件），配合 Dockerfile 现场构建镜像；镜像包=docker save 出的 tar，直接进包；附加文件=随包分发的普通文件（如 schema.sql），由组件编排自行挂载。交付附件包（attachments-* / tenant-* zip 或根含 attachments.json）会被上传与打包双闸拒绝——请上传到「交付包」页的交付附件，经「出交付包」叠加。旧版 jar/ui_zip 两个类型现统称应用包，历史数据与 collect-kit 推送自动兼容。' },
  { q: '套件和最终交付包有什么区别？', a: '套件=打包向导产物，不含租户变量/license，直接安装仅适用于测试环境；最终交付包=在套件记录行「出交付包」叠加签发附件重打包的产物（附件逐条目解压落位 delivery/ 与 extras/，console 表单自动预填租户变量），是正式交付给租户环境的安装物。' },
  { q: '镜像从哪来？', a: '三种：应用包构建；镜像包导入（制品库或镜像仓）；镜像地址导出（本机 docker）。组件镜像按「本机 docker → 镜像仓 → 报缺」顺序解析。' },
  { q: '安装时还需要联网吗？', a: '不需要——镜像全部随包离线分发，console 所在内网即可（agent→console 出站连接）；docker 主机需装 Docker，systemd 裸机单元需 java。' }
]
</script>

<style scoped>
.guide { max-width: 1080px; }
.guide-nav { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 10px; }
.guide-nav-item { padding: 3px 10px; border: 1px solid var(--el-border-color); border-radius: 12px;
  color: var(--el-text-color-regular); text-decoration: none; font-size: 12px; background: var(--el-bg-color); }
.guide-nav-item:hover { color: var(--el-color-primary); border-color: var(--el-color-primary); }
.concept { border: 1px solid var(--el-border-color-lighter); border-top: 3px solid; border-radius: 6px;
  padding: 10px 12px; height: 100%; box-sizing: border-box; }
.concept-title { font-weight: 600; display: flex; align-items: center; gap: 6px; margin-bottom: 6px; }
.concept-desc { color: var(--el-text-color-secondary); font-size: 12px; line-height: 1.7; }
.concept-alert { margin-top: 10px; }
.guide-steps { margin-top: 6px; }
.guide-p { color: var(--el-text-color-regular); font-size: 13px; line-height: 1.8; margin: 4px 0; }
.guide-pre { background: var(--el-fill-color-darker); padding: 12px; border-radius: 6px; font-size: 12px;
  line-height: 1.6; overflow: auto; color: var(--el-text-color-primary); }
.ph { color: var(--el-color-primary); background: var(--el-color-primary-light-9); border-radius: 2px; padding: 0 3px; }
</style>
