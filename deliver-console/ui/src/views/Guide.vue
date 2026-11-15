<template>
  <div class="page">
    <div class="page-head"><h2>使用指南</h2></div>

    <el-card shadow="never" class="card">
      <template #header><b>架构（控制面 + 节点代理）</b></template>
      <div class="md">
        <p>deliver-console 是安装<b>控制面</b>，deliver-agent 是目标机上的<b>节点代理</b>。控制面从不主动登录目标机——agent 出站长轮询领任务，因此<b>目标机不需要对 console 开放任何入站端口、也不需要账号密码直登</b>（只需一次性把 agent 二进制拷过去启动）。console 所在机器自动作为「本机节点」（单机安装零 agent）。</p>
        <p>安装素材来自 deliver-studio 打包的 <b>format:3 部署包</b>（或平台叠加 delivery/ 后的最终交付包）。</p>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header><b>五步安装</b></template>
      <div class="md">
        <ol>
          <li><b>接主机</b>（主机池页）：下载双平台 agent → 拷到目标机 → <code>./deliver-agent-linux install -server http://console:7870 -token &lt;引导令牌&gt;</code> → 主机自动上线（能力档案：docker/compose/systemd/java/资源）。上线后行内「执行」打开<b>主机终端</b>——左侧快捷命令点击即执行、右侧命令行实时交互（一次一条排队执行，可停止/关窗中断，整个窗口记为一条执行记录），「日志」看该主机执行历史。</li>
          <li><b>备基础工具</b>（主机池页，可选）：Docker / docker-compose 安装源按<b>版本</b>上传离线介质（或留空走在线通道）；主机行「装 Docker / 装 Compose」弹窗选定版本一键安装，已装主机可换版升级（Docker 指定版本仅离线介质可承载）。<b>仅 Linux</b>——Windows 主机请人工预装 Docker Desktop（桌面版自带 compose 插件），主机池能力列有提示。</li>
          <li><b>传部署包</b>（部署包页）：上传 zip，上传即完成 manifest 逐文件 SHA-256 强校验；同应用多版本时给出与已装清单的 diff 预览。</li>
          <li><b>编排</b>（编排安装页）：拖拽分配服务到主机（单元可选 docker/systemd 双形态；未落机组件=复用外部设施，端点手填）→ 填变量与凭据（delivery/vars.env 自动预填）→ 自动生成 DAG（装载→编排→跨机健康门控→SQL 预置→入口确认），可增删节点、插入 cmd/gate。</li>
          <li><b>执行</b>（安装记录页）：节点染色进度、逐节点日志抽屉、失败重试/跳过、gate 人工确认；agent 失联由看门狗超时兜底；console 重启自动恢复调度。</li>
          <li><b>升级</b>：换新版本包重复 ②-④，选「增量安装」——未变更的主机整组跳过（apply 语义），仅变更服务重建。</li>
          <li><b>回退</b>（应急）：安装记录页成功会话「从此会话回退」= 用该会话的包+分配+变量重放一次增量安装（自动重建旧介质）；库结构与数据不回滚，恢复后请尽快重新升级。</li>
        </ol>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header><b>安全模型</b></template>
      <div class="md">
        <ul>
          <li><b>console 登录</b>：单管理员 admin，首启随机密码打印在启动控制台（-reset-pass 重置）；UI 会话 7 天滑动续期。</li>
          <li><b>双令牌</b>：引导令牌（enroll）仅用于 agent 首次注册，可在主机池页轮换；注册成功签发 agent 令牌（落盘目标机，重启免重注），poll/上报/文件下载全凭它，可按主机吊销。</li>
          <li><b>完整性</b>：包上传即 manifest 强校验；agent 下载文件逐个 SHA-256 复核；任务路径防逃逸。</li>
          <li>v1 面向离线内网，明文 HTTP；如需 TLS 前置反代承载。</li>
        </ul>
      </div>
    </el-card>

    <el-card shadow="never" class="card">
      <template #header><b>常量速查</b></template>
      <div class="md">
        <ul>
          <li>agent 数据目录：Linux <code>/opt/mse/deliver</code>，Windows <code>C:\ProgramData\mse\deliver</code>（-data-dir 可改）；应用部署在其 <code>apps/&lt;app&gt;/</code>。</li>
          <li>任务九类：load（拉文件+docker load）/ compose（per-host 编排 up）/ systemd（裸机 unit）/ cmd（一次性 shell）/ wait（健康等待）/ plugin（配置中心插件：plan 比对→gate 审差异→apply 幂等发布〔可改 force 覆盖〕→health）/ docker（安装/换版 Docker，仅 Linux）/ toolkit（docker compose 插件维护，仅 Linux）/ gate（人工确认，console 侧不派发），全部幂等可重试。</li>
          <li>配置中心插件：部署包组件声明 configCenter（nacos）时自动生成插件链；插件二进制内嵌 console、按执行主机 OS 分发（与工具版本绑定，不随包走）。</li>
          <li>审计溯源：上传即算包指纹（sha256(manifest.json)）随会话落库，安装记录页可查。</li>
          <li>增量基线：inventory 按服务记 block/image/env/conf 哈希；任一变更即该主机整组重跑（load 幂等跳过未变文件）。</li>
        </ul>
      </div>
    </el-card>
  </div>
</template>

<script setup>
</script>

<style scoped>
.page { max-width: 1080px; margin: 0 auto; }
.page-head h2 { margin: 0 0 12px; font-size: 17px; }
.card { margin-bottom: 14px; }
.md { font-size: 13px; color: var(--el-text-color-regular); line-height: 1.8; }
.md code { background: var(--el-fill-color); padding: 1px 5px; border-radius: 3px; font-size: 12px; }
.md ol, .md ul { padding-left: 20px; margin: 0; }
.md li { margin-bottom: 6px; }
.md p { margin: 0 0 8px; }
</style>
