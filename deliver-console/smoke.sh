#!/bin/sh
# deliver-console 真实冒烟（本机节点 + 真 docker）：造 format:3 小包（mse/mse-nginx）→
# 起 console → API 全链路（登录/上传/会话）→ 断言入口 HTTP 200 → 清理。
# 仅本机验收用，不入 CI。用法：sh smoke.sh [控制台端口]
set -e
cd "$(dirname "$0")"
PORT="${1:-17870}"
WORK=$(mktemp -d)
APP=smoke
VER=1.0.0
IMG=nginx:stable-alpine
HPORT=18080

echo "==> 造包 $WORK/pkg"
mkdir -p "$WORK/pkg/images"
docker save "$IMG" -o "$WORK/pkg/images/nginx_stable-alpine.tar"
cat > "$WORK/pkg/docker-compose.yml" <<EOF
name: smoke
services:
  web:
    image: $IMG
    ports:
      - "$HPORT:80"
    restart: unless-stopped
EOF
cat > "$WORK/pkg/components.json" <<EOF
{
  "format": 3, "name": "$APP", "version": "$VER",
  "entry": {"unit": "web", "service": "web", "image": "$IMG", "port": "80", "healthPath": "/"},
  "units": [{"code": "web", "service": "web", "services": ["web"], "image": "$IMG", "port": "80", "healthPath": "/"}],
  "variables": [{"name": "SMOKE_VAR", "label": "冒烟变量", "source": "input", "value": "ok"}],
  "components": []
}
EOF
printf 'smoke:\n  note: {{SMOKE_VAR}}\n' > "$WORK/pkg/config-template.yaml"
# manifest 逐文件 SHA-256（posix sha256sum；Git Bash 可用）
{
  printf '{"files":{'
  first=1
  for f in docker-compose.yml components.json config-template.yaml images/nginx_stable-alpine.tar; do
    [ $first = 1 ] || printf ','
    first=0
    printf '"%s":"%s"' "$f" "$(sha256sum "$WORK/pkg/$f" | cut -d' ' -f1)"
  done
  printf '}}'
} > "$WORK/pkg/manifest.json"
(cd "$WORK/pkg" && python - <<'ZIP'
import os, zipfile
with zipfile.ZipFile('../smoke-1.0.0.zip', 'w', zipfile.ZIP_STORED) as z:
    for root, _, files in os.walk('.'):
        for f in files:
            p = os.path.relpath(os.path.join(root, f), '.').replace(os.sep, '/')
            z.write(p, p)
ZIP
)

echo "==> 构建并启动 console（:$PORT）"
go build -o "$WORK/deliver-console.exe" .
"$WORK/deliver-console.exe" -addr "127.0.0.1:$PORT" -root "$WORK/console-data" > "$WORK/console.log" 2>&1 &
CONSOLE_PID=$!
trap 'kill $CONSOLE_PID 2>/dev/null || true; docker rmi $IMG-smoke-ns 2>/dev/null || true' EXIT
PASS=""
for I in $(seq 1 20); do
  sleep 1
  PASS=$(grep -ao '密码: [A-Za-z0-9]*' "$WORK/console.log" 2>/dev/null | head -1 | cut -d' ' -f2)
  [ -n "$PASS" ] && break
done
if [ -z "$PASS" ]; then echo "FATAL: 未取到首启密码"; cat "$WORK/console.log"; exit 1; fi
BASE="http://127.0.0.1:$PORT"

echo "==> 登录 + 上传"
TOKEN=$(curl -sf -X POST "$BASE/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PASS\"}" | sed 's/.*"token":"\([^"]*\)".*/\1/')
PKG=$(curl -sf -X POST "$BASE/api/packages" -H "X-Console-Token: $TOKEN" -F "file=@$WORK/$APP-$VER.zip" | sed 's/.*"id":\([0-9]*\).*/\1/')
echo "packageId=$PKG"

echo "==> 查主机池拿本机节点 id"
LOCALHOST=$(curl -sf "$BASE/api/hosts" -H "X-Console-Token: $TOKEN" | sed 's/.*"id":\([0-9]*\)[^}]*"local":true.*/\1/')
[ -n "$LOCALHOST" ] || LOCALHOST=$(curl -sf "$BASE/api/hosts" -H "X-Console-Token: $TOKEN" | python -c 'import json,sys; d=json.load(sys.stdin); print([h["id"] for h in d["hosts"] if h["local"]][0])')
echo "localHostId=$LOCALHOST"

echo "==> 发起会话（fresh，全部分配本机）"
SID=$(curl -sf -X POST "$BASE/api/sessions" -H "X-Console-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"packageId\":$PKG,\"mode\":\"fresh\",\"dag\":[],\"placement\":{\"mode\":{},\"host\":{\"web\":$LOCALHOST},\"vars\":{\"SMOKE_VAR\":\"ok\"},\"ext\":{}}}" \
  | python -c 'import json,sys; d=json.load(sys.stdin); print(d["id"])')
echo "sessionId=$SID"

echo "==> 等待完成"
I=0
while [ $I -lt 90 ]; do
  ST=$(curl -sf "$BASE/api/sessions/$SID" -H "X-Console-Token: $TOKEN" | python -c 'import json,sys; d=json.load(sys.stdin); print(d["session"]["status"])')
  [ "$ST" = "done" ] && break
  [ "$ST" = "failed" ] && { echo "会话失败"; curl -sf "$BASE/api/sessions/$SID" -H "X-Console-Token: $TOKEN" | python -m json.tool; exit 1; }
  sleep 2; I=$((I+1))
done
[ "$ST" = "done" ] || { echo "超时未完成"; exit 1; }
echo "会话 done"

echo "==> 入口探活 http://127.0.0.1:$HPORT/"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HPORT/")
echo "HTTP $CODE"
[ "$CODE" = "200" ] || { echo "入口不可达"; exit 1; }

echo "==> 清理容器"
(cd "$WORK/console-data/local/apps/smoke" && docker compose down 2>/dev/null) || true

echo "SMOKE-OK（console 真实链路：上传→分配→load→compose→wait→入口 200）"
