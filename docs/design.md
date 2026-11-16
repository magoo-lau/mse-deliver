# 交付管线设计（最终态）

## 一、两阶段交付模型

```
业务仓/采集器（mse-collect-kit）                mse-deliver
┌──────────────────────────┐    HTTP    ┌──────────────────────────────┐
│ collect：钩子构建 fat-jar/ │ --studio▶ │ deliver-studio（7860）        │
│ 镜像/UI zip → 推送制品库+  │           │  ①制品+预设 → format:3 套件   │
│ 镜像仓+预设（一键打包出套件）│           │  ②套件+交付附件 → 重打包最终包 │
└──────────────────────────┘           │ deliver-console（7870）       │
                                        │  部署包上传解析+编排分配+DAG执行 │
                                        └──────────────────────────────┘
```

- **套件（format:3）**：可直接交付运行的完整部署包（镜像目录 + 制品 + 预设 + manifest）。
- **交付附件（format:1）**：平台签发的附件包——license + 租户数据 JSONL + 租户变量 + schema，RS256 签名内层；归档可再下载或配置 `mse.deliver.studio.*` 直推 studio。
- **最终交付包 = 套件 + 附件重打包**：附件入库全量校验（POST /api/deliveries，sha 幂等）→ 套件 manifest 硬校验 + 附件落位 delivery//extras/（冲突硬拦）→ 重建外层 manifest（审计段：租户/licenseId/来源套件指纹）→ zip 执行位保留。套件与交付包均可直接交付运行。

## 二、关键机制（ADR 摘要）

| 机制 | 口径 |
|---|---|
| 单机部署（ADR-1 增补） | 打包可选「单机部署」随包附带 install.sh 薄壳——单机全 Docker 首选便捷通道；configCenter 组合打包硬拦截 |
| 配置中心插件（ADR-4） | deliver-plugins/nacos-config：stdio JSON 子进程协议，console 内嵌分发；与 framework 仓 init-configs.sh 同口径 |
| 打包引擎唯一（ADR-9） | 职责归位：collect-kit 只采集推送，studio 唯一打包，console 唯一安装；零仓库知识可整体外迁 |
| zip×整包引用前置拦截 | 打包期：材料 .zip 且 Dockerfile 含 {{ARTIFACT_FILE}} 即拒（后端选 jar/fat-jar、UI zip 配给 COPY . 前端单元）；向导第②步同步红字防错 |
| 下载票 | GB 级包：登录态 `POST /api/packages/{id}/ticket` 签一次性票（crypto/rand 16B hex，10 分钟，签发时清扫过期），浏览器原生 `<a href>` 下载（流式落盘零内存压力）；isPublicPath 仅放行带 t 的 download GET，错包/过期票 401 并作废防重放 |
| 双令牌 | admin 登录令牌 + agent 引导令牌/agent 令牌；pkgSha 审计溯源、介质回退 |
| 推送去重 | 同身份（类型+文件名+应用+版本）服务端保留最新一条，重复推送不堆积、启动对账收敛历史重复 |

## 三、repoRoot / preset 契约

- 采集侧 recipe（mse-collect-kit 仓 `recipes/ai-mse/`）产 **build-manifest.json**——核心与 studio 之间的唯一数据合同。
- `studio-preset.json` 与 studio `presets/ai-mse.json` **字节一致**（双份维护，改一侧必同步另一侧）；preset 内 configTemplate 头注释记录 Nacos 基线的源头（mse-framework 仓 docker/nacos/config/*.yaml）。
