# AGENTS.md —— mse-deliver 交付工具族协作约定

本文件面向所有在本仓库工作的 AI 助手与开发者。本仓库自 ai-mse 单仓（源 commit `cc5410a3`）拆分独立。

## 一、定位与结构

通用交付工具族，**零仓库知识**（不感知 ai-mse 各业务仓内容），可整体外迁独立维护：

```
mse-deliver/
├── deliver-studio/     # 唯一打包引擎（7860）：套件打包 + 重打包；presets/ 经 go:embed 内嵌随二进制分发
├── deliver-console/    # 唯一安装控制台（7870）：控制面/节点代理模型；go:embed ui/dist + agent-bin + plugin-bin
├── deliver-agent/      # 安装节点代理：出站长轮询领任务；无独立产物，由 console 构建时交叉编译内嵌
└── deliver-plugins/    # nacos-config 插件（stdio JSON 子进程协议）；同上由 console 内嵌
```

模块间 Go 零 import（各自独立 go.mod）；console 构建脚本负责 agent/plugins 的交叉编译与嵌入。与 collect-kit（mse-collect-kit 仓）经 HTTP（studio REST API）集成。

## 二、关键设计约定（改代码前先读 docs/）

- **format:3 套件 / format:1 附件**：manifest 结构见 docs/design.md；套件与交付包均可直接交付运行。
- **重打包审计段**：附件入库全量校验（POST /api/deliveries，sha 幂等）、冲突硬拦、外层 manifest 重建（租户/licenseId/来源套件指纹）。
- **下载票**：GB 级包走 `POST /api/packages/{id}/ticket` 一次性票（crypto/rand，10 分钟）+ 浏览器原生下载；isPublicPath 仅放行带 t 参数的 download GET。
- **双令牌**：console admin 登录令牌 + agent 令牌；pkgSha 审计溯源。
- **presets 同步**：studio `presets/ai-mse.json` 是 collect-kit 仓 `recipes/ai-mse/studio-preset.json` 的随二进制分发同步副本（字节一致由构建流程保障）。

## 三、构建、测试与验证

- `sh build.sh`（编排四工具）或各目录 `build.sh/build.ps1/build.cmd`；console 冒烟 `smoke.sh`（本机真实链路，不入 CI）。
- 验证底线：`go build ./...` 全部通过；`go test ./...`（console/studio）通过；改打包/重打包逻辑必须补/跑对应用例（TestPackageDownloadTicket、TestBuildPlanZipArtifactWithWholeFileRef 等为回归锚点）。
- 前端改动：各工具 ui/ 内 `npm run build` 通过。

## 四、提交纪律

工具间改动分开成体系提交；行为变更同步 docs/（design/studio/console）与工具内 README。
