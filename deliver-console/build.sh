#!/bin/sh
# deliver-console 完整构建：① 前端（npm build → ui/dist）② 交叉编译 deliver-agent 双平台 → agent-bin/
# ③ 交叉编译配置中心插件（deliver-plugins/nacos-config）双平台 → plugin-bin/ ④ go build 内嵌全部
# 产出单文件 deliver-console。前置：node + go（首次需网络拉 npm/go 模块）。
set -e
cd "$(dirname "$0")"

echo "==> [1/4] 前端构建"
(cd ui && [ -d node_modules ] || npm install --no-audit --no-fund)
(cd ui && npm run build)

echo "==> [2/4] 交叉编译 deliver-agent（内嵌用）"
mkdir -p agent-bin
rm -f agent-bin/deliver-agent-linux agent-bin/deliver-agent-windows.exe
AGENT_VER=$(date +%Y%m%d%H%M)
(cd ../deliver-agent && GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w -X main.agentVersion=$AGENT_VER" -o ../deliver-console/agent-bin/deliver-agent-linux .)
(cd ../deliver-agent && GOOS=windows GOARCH=amd64 go build -ldflags="-s -w -X main.agentVersion=$AGENT_VER" -o ../deliver-console/agent-bin/deliver-agent-windows.exe .)

echo "==> [3/4] 交叉编译配置中心插件（内嵌用）"
mkdir -p plugin-bin
rm -f plugin-bin/nacos-config-linux plugin-bin/nacos-config-windows.exe
(cd ../deliver-plugins/nacos-config && GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o ../../deliver-console/plugin-bin/nacos-config-linux .)
(cd ../deliver-plugins/nacos-config && GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o ../../deliver-console/plugin-bin/nacos-config-windows.exe .)

echo "==> [4/4] 控制台二进制（内嵌前端 + agent + 插件；agentStamp=内嵌 agent 版本戳，旧 agent 预警用）"
go build -ldflags="-s -w -X main.agentStamp=$AGENT_VER" -o deliver-console .
echo "完成：./deliver-console（默认 http://0.0.0.0:7870）"
