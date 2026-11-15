#!/usr/bin/env bash
# deliver-studio 完整构建：前端 → 工作台单文件二进制（内嵌前端与预设；安装由 deliver-console 承担）。
# 产物：deliver-studio.exe / deliver-studio-linux（本目录，gitignore）。
# 前置：node/npm + go；首次需网络拉 npm 与 go 模块（modernc.org/sqlite）。
# Windows 原生等价入口：build.ps1（或双击 build.cmd）；Git Bash 下本脚本亦可直接运行。
set -euo pipefail
cd "$(dirname "$0")"

echo "==> [1/2] 前端构建（ui → ui/dist）"
(cd ui && npm install --no-audit --no-fund && npm run build)

echo "==> [2/2] 工作台构建（内嵌前端 dist）"
go build -ldflags="-s -w" -o deliver-studio.exe .
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o deliver-studio-linux .

echo "构建完成 ✔ deliver-studio.exe / deliver-studio-linux（双击或运行即起，数据目录 deliver-data/）"
