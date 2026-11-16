# deliver-console（7870）—— 唯一安装控制台

## 职责

控制面/节点代理模型：部署包上传解析 → 图形化编排分配（主机/服务/DAG）→ 执行与增量升级 → 审计观测。

- **双令牌**：admin 账号密码登录 + 引导令牌（agent 首次接入）/ agent 令牌。
- **执行**：DAG 编排、执行组（execgroup posix/windows）、远端执行经 deliver-agent 长轮询领任务（出站免直登节点）。
- **插件链**：配置中心插件族（nacos-config，stdio JSON 子进程协议）内嵌分发；介质回退；pkgSha 审计溯源。

## 构建 / 运行 / 测试

```sh
sh ../build.sh console           # 或本目录 build.sh / build.ps1 / build.cmd：
                                 # ui 构建 → 交叉编译 deliver-agent 双平台 → agent-bin/
                                 #        → 交叉编译 nacos-config 双平台 → plugin-bin/
                                 #        → go build 内嵌全部（单文件 deliver-console）
./deliver-console                # 默认 0.0.0.0:7870
go test ./...                    # e2e/topology/store/engine 等；真实链路另跑 smoke.sh（本机验收用，不入 CI）
```

- 内嵌：`go:embed ui/dist + agent-bin + plugin-bin`（agent/plugins 二进制随仓库跟踪，勿删；ui/dist 构建生成）。
- 运行期数据：console-data/（db/WAL/日志，gitignore）。
- agent / plugins 无独立产物：由 console 构建时代编；单独验证可 `go build ./...`。

## 冒烟

`sh smoke.sh` / `smoke-inc.sh`：本机造 format:3 小包 → 起 console → API 全链路断言（仅本机验收用）。
