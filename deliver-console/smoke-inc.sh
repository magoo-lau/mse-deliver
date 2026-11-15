#!/bin/sh
# 增量安装冒烟：fresh 安装 → 同包无变更增量（空计划即 done）→ 改变量增量（重建该主机）。
# 前置：本机 docker；镜像 nginx:stable-alpine（缺失会 docker load 失败即报错）。仅本机验收用。
set -e
cd "$(dirname "$0")"
PORT="${1:-17890}"
WORK=$(mktemp -d)
IMG=nginx:stable-alpine
TAR=nginx_stable-alpine.tar
HPORT=18090

mkdir -p "$WORK/pkg/images"
docker save "$IMG" -o "$WORK/pkg/images/$TAR"
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
{"format":3,"name":"smoke","version":"1.0.0",
 "entry":{"unit":"web","service":"web","image":"$IMG","port":"80","healthPath":"/"},
 "units":[{"code":"web","service":"web","services":["web"],"image":"$IMG","port":"80","healthPath":"/"}],
 "variables":[{"name":"SMOKE_VAR","label":"冒烟变量","source":"input","value":"ok"}],
 "components":[]}
EOF
printf 'smoke:\n  note: {{SMOKE_VAR}}\n' > "$WORK/pkg/config-template.yaml"
{
  printf '{"files":{'
  first=1
  for f in docker-compose.yml components.json config-template.yaml "images/$TAR"; do
    [ $first = 1 ] || printf ','
    first=0
    printf '"%s":"%s"' "$f" "$(sha256sum "$WORK/pkg/$f" | cut -d' ' -f1)"
  done
  printf '}}'
} > "$WORK/pkg/manifest.json"
(cd "$WORK/pkg" && python -c "
import os, zipfile
with zipfile.ZipFile('../smoke.zip', 'w', zipfile.ZIP_STORED) as z:
    for root, _, files in os.walk('.'):
        for f in files:
            p = os.path.relpath(os.path.join(root, f), '.').replace(os.sep, '/')
            z.write(p, p)
")

go build -o "$WORK/deliver-console.exe" .
"$WORK/deliver-console.exe" -addr "127.0.0.1:$PORT" -root "$WORK/console-data" > "$WORK/console.log" 2>&1 &
CONSOLE_PID=$!
trap 'kill $CONSOLE_PID 2>/dev/null || true' EXIT
PASS=""
for I in $(seq 1 20); do
  sleep 1
  PASS=$(grep -ao '密码: [A-Za-z0-9]*' "$WORK/console.log" 2>/dev/null | head -1 | cut -d' ' -f2)
  [ -n "$PASS" ] && break
done
[ -n "$PASS" ] || { echo "FATAL: 未取到密码"; cat "$WORK/console.log"; exit 1; }
BASE="http://127.0.0.1:$PORT"
TK=$(curl -sf -X POST "$BASE/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PASS\"}" | sed 's/.*"token":"\([^"]*\)".*/\1/')
PKG=$(curl -sf -X POST "$BASE/api/packages" -H "X-Console-Token: $TK" -F "file=@$WORK/smoke.zip" | sed 's/.*"id":\([0-9]*\).*/\1/')

new_session() { # $1=mode $2=var
  curl -sf -X POST "$BASE/api/sessions" -H "X-Console-Token: $TK" -H 'Content-Type: application/json' \
    -d "{\"packageId\":$PKG,\"mode\":\"$1\",\"dag\":[],\"placement\":{\"mode\":{},\"host\":{\"web\":1},\"vars\":{\"SMOKE_VAR\":\"$2\"},\"ext\":{}}}" \
    | python -c 'import json,sys; d=json.load(sys.stdin); print(d["id"], d["mode"], d["nodes"])'
}
wait_done() { # $1=sid
  for I in $(seq 1 60); do
    ST=$(curl -sf "$BASE/api/sessions/$1" -H "X-Console-Token: $TK" | python -c 'import json,sys; print(json.load(sys.stdin)["session"]["status"])')
    [ "$ST" = "done" ] || [ "$ST" = "failed" ] && break
    sleep 2
  done
  echo "$ST"
}

echo "==> ① fresh 安装"
read SID1 M1 N1 <<EOF
$(new_session fresh ok)
EOF
echo "session=$SID1 mode=$M1 nodes=$N1 → $(wait_done "$SID1")"
curl -s -o /dev/null -w "入口 HTTP %{http_code}\n" "http://127.0.0.1:$HPORT/"

echo "==> ② 增量（无变更）"
read SID2 M2 N2 <<EOF
$(new_session incremental ok)
EOF
echo "session=$SID2 mode=$M2 nodes=$N2 → $(wait_done "$SID2")"
[ "$M2" = "incremental" ] && [ "$N2" = "0" ] || { echo "FAIL: 无变更增量应为空计划"; exit 1; }

echo "==> ③ 增量（变量变更 → 该主机整组重跑）"
read SID3 M3 N3 <<EOF
$(new_session incremental changed)
EOF
echo "session=$SID3 mode=$M3 nodes=$N3 → $(wait_done "$SID3")"
[ "$M3" = "incremental" ] && [ "$N3" = "3" ] || { echo "FAIL: 变更增量应保留 3 节点"; exit 1; }
sleep 3
curl -s -o /dev/null -w "重建后入口 HTTP %{http_code}\n" "http://127.0.0.1:$HPORT/"

(cd "$WORK/console-data/local/apps/smoke" && docker compose down) >/dev/null 2>&1 || true
echo "INC-SMOKE-OK（fresh 安装 → 空增量跳过 → 变更增量重建）"
