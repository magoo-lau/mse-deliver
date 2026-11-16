#!/bin/sh
# =============================================================================
# 交付工具族全量构建编排：
#   1. deliver-console：ui 构建 → 交叉编译 deliver-agent 双平台 → 交叉编译
#      nacos-config 双平台 → go build 内嵌全部（单文件 deliver-console）
#   2. deliver-studio：ui 构建 → go build（deliver-studio.exe / deliver-studio-linux）
#   3. deliver-agent / deliver-plugins：go build（验证可编译，产物由 console 交叉编译内嵌）
#
# 前置：Go ≥ 1.21、Node ≥ 18。各工具目录另有独立 build.sh/build.ps1/build.cmd。
# 用法：sh build.sh [console|studio|agent|plugins ...]   # 缺省全部
# =============================================================================
set -eu
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

WANTED=${*:-"console studio agent plugins"}

build_console() {
  echo "===== deliver-console ====="
  (cd deliver-console/ui && npm ci --no-audit --no-fund && npm run build)
  (cd deliver-console && GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o agent-bin/deliver-agent-linux        ../deliver-agent)
  (cd deliver-console && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o agent-bin/deliver-agent-windows.exe ../deliver-agent)
  (cd deliver-console && GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o plugin-bin/nacos-config-linux       ../deliver-plugins/nacos-config)
  (cd deliver-console && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o plugin-bin/nacos-config-windows.exe ../deliver-plugins/nacos-config)
  (cd deliver-console && go build ./...)
  echo "--- deliver-console OK"
}

build_studio() {
  echo "===== deliver-studio ====="
  (cd deliver-studio/ui && npm ci --no-audit --no-fund && npm run build)
  (cd deliver-studio && go build ./...)
  echo "--- deliver-studio OK"
}

build_agent() {
  echo "===== deliver-agent（go build 验证）====="
  (cd deliver-agent && go build ./...)
  echo "--- deliver-agent OK"
}

build_plugins() {
  echo "===== deliver-plugins/nacos-config（go build 验证）====="
  (cd deliver-plugins/nacos-config && go build ./...)
  echo "--- deliver-plugins OK"
}

fail=""
for t in $WANTED; do
  case "$t" in
    console) build_console || fail="$fail console" ;;
    studio)  build_studio  || fail="$fail studio" ;;
    agent)   build_agent   || fail="$fail agent" ;;
    plugins) build_plugins || fail="$fail plugins" ;;
    *) echo "未知目标：$t（可选 console studio agent plugins）" >&2; exit 1 ;;
  esac
done

echo ""
if [ -n "$fail" ]; then
  echo "构建失败（$fail）" >&2
  exit 1
fi
echo "交付工具族全部构建通过"
