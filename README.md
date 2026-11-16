# mse-deliver —— 通用交付工具族

自 ai-mse 单仓（源 commit `cc5410a3`）拆分独立。零仓库知识、可整体外迁的交付工具族：**deliver-studio（唯一打包引擎）+ deliver-console（唯一安装控制台）+ deliver-agent（安装节点代理）+ deliver-plugins（配置中心插件族）**。全部 Go 单二进制，零外部运行时依赖（SQLite 内嵌）。

| 工具 | 定位 | 默认端口 | 关键产物 |
|---|---|---|---|
| [deliver-studio](deliver-studio/) | 唯一打包引擎：①制品+预设→format:3 套件；②套件+交付附件→重打包最终交付包（附件 sha 幂等入库、冲突硬拦、重建外层 manifest 审计段）；GB 级包一次性下载票走浏览器原生下载 | 7860 | deliver-studio(.exe/.linux) |
| [deliver-console](deliver-console/) | 唯一安装控制台：控制面/节点代理模型，部署包上传解析+图形化编排分配+DAG 执行+增量升级；admin 登录+引导令牌/agent 令牌双令牌；配置中心插件链+介质回退+pkgSha 审计 | 7870 | deliver-console 单文件（内嵌 UI+agent-bin+plugin-bin） |
| [deliver-agent](deliver-agent/) | 安装节点代理：出站长轮询领任务，免直登节点；纯 Go 零依赖 | - | 由 console 构建时交叉编译内嵌 agent-bin/ |
| [deliver-plugins](deliver-plugins/nacos-config/) | 配置中心插件族：nacos-config（stdio JSON 子进程协议，console 内嵌分发） | - | 由 console 构建时交叉编译内嵌 plugin-bin/ |

平台「交付附件签发」只签发附件包（license + 租户数据 JSONL + 租户变量 + schema，RS256 签名内层）；最终交付包由 studio「套件+附件」重打包产出。详细设计见 [docs/](docs/README.md)。

## 构建

```sh
sh build.sh          # 全量编排：console（UI→agent 交叉编译→plugins 交叉编译→内嵌 go build）
                     #           + studio（UI→go build）+ agent / plugins 单独 go build
```

各工具目录另有独立 `build.sh / build.ps1 / build.cmd`（Windows 双击入口）与 `smoke.sh`（console 本机真实冒烟，不入 CI）。Go ≥ 1.21、Node ≥ 18。

## 与上游仓的关系

- 上游制品与预设来自各业务仓的采集器 **collect-kit（独立仓 mse-collect-kit）**，经 HTTP（`--studio` 参数指向本仓 studio）推送制品/镜像/预设——Go 模块间零 import，仓库级解耦。
- 租户私有部署（tenant-starter + 离线交付导入端点）语义见 mse-framework 仓 `docs/tenancy.md`。

## 测试

```sh
cd deliver-studio && go test ./...
cd deliver-console && go test ./...      # 含 e2e/topology/store 等；冒烟另跑 smoke.sh
```
