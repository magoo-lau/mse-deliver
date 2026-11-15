// deliver-studio 单机安装脚本薄壳（install.sh）：打包期按包内容烘焙生成，随 format:3 部署包可选附带。
// 边界（deliver-design.md ADR-1 增补）：纯 shell 复用包内既有产物（compose/配置模板/manifest/镜像），
// 只做「单机全 Docker 首装」——校验清单 → docker load → 收集安装期变量（const/random/询问，
// delivery/vars.env 预填）→ 渲染 .env 与 data/conf/application.yaml → compose up → 逐单元健康门控。
// DAG 编排/增量升级/回退/配置中心插件链/审计/多机/systemd 裸机仍归 deliver-console（权威安装链不变）。
package main

import (
	"fmt"
	"regexp"
	"strings"
)

var portItemRe = regexp.MustCompile(`^-\s+"?(?:[0-9.]+:)?(\d+):(\d+)"?$`)

// composeHostPort 解析编排服务块的 ports 发布映射：容器端口 → 宿主端口（未发布返回空，
// 调用方退化为容器状态探测——与 console 未发布端口降级口径一致）。
// 兼容 "host:container" / "ip:host:container"（引号与否均可）；区间/协议后缀写法不识别（返回空走降级）。
func composeHostPort(block, containerPort string) string {
	inPorts := false
	for _, line := range strings.Split(block, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !strings.HasPrefix(t, "-") {
			inPorts = strings.HasPrefix(t, "ports:")
			continue
		}
		if !inPorts {
			continue
		}
		if m := portItemRe.FindStringSubmatch(t); m != nil && m[2] == containerPort {
			return m[1]
		}
	}
	return ""
}

// endpointStandaloneValue 端点占位符的单机全栈取值（console 同机分支口径）：
// from=port→端口 / from=host→服务名 / 缺省→scheme://服务名:容器端口。
func endpointStandaloneValue(ep Endpoint) string {
	if ep.From == "port" {
		return ep.Port
	}
	if ep.From == "host" {
		return ep.Service
	}
	if ep.Scheme != "" {
		return ep.Scheme + "://" + ep.Service + ":" + ep.Port
	}
	return ep.Service + ":" + ep.Port
}

// shq 单引号 shell 字面量（烘焙常量安全引用）
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// renderInstallSh 生成 install.sh：包专属数据（变量/凭据/端点/健康清单/入口）在打包期烘焙，
// 运行期只做交互与执行——零 JSON 解析、零第二套安装逻辑。hasConf 表示包内是否有 config-template.yaml。
// buildVals 为打包期变量终值：编排块先按打包口径渲染（ports 可能用 {{UNIT_PORT}} 写法）再解析服务名与端口映射。
func renderInstallSh(proj, version string, app *App, comps []*Component, entry *Unit, hasConf bool, buildVals map[string]string) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	// ---- 头部与公共函数 ----
	w("#!/usr/bin/env bash")
	w("# %s v%s 单机安装脚本 —— deliver-studio 打包期烘焙生成", proj, version)
	w("#")
	w("# 边界：单机全 Docker 首装便捷通道（校验清单→导入镜像→收集变量→渲染配置→compose up→健康门控）。")
	w("#   增量升级/介质回退/多机分配/配置中心插件链/审计溯源/systemd 裸机 → 使用 deliver-console 安装控制台。")
	w("#   脚本可重复执行（幂等重跑=按当前输入重新渲染并 up）；docker 数据卷/挂载目录内容不受影响。")
	w("")
	w("set -euo pipefail")
	w("")
	w("PKG_DIR=$(cd \"$(dirname \"$0\")\" && pwd)")
	w("cd \"$PKG_DIR\"")
	w("SKIP_VERIFY=0")
	w("[ \"${1:-}\" = \"--skip-verify\" ] && SKIP_VERIFY=1 || true")
	w("FAIL=0")
	w("")
	w("log()    { printf '\\033[1;32m==>\\033[0m %%s\\n' \"$*\"; }")
	w("ok()     { printf '  ✔ %%s\\n' \"$*\"; }")
	w("bad()    { printf '  ✘ %%s\\n' \"$*\"; FAIL=1; }")
	w("rand16() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16; }")
	w("getv()   { local t=\"V_$1\"; printf '%%s' \"${!t:-}\"; }")
	w("")
	w("# 预填：delivery/vars.env（KEY=VALUE 行，平台/交付层在包外追加）")
	w("load_prefill() {")
	w("  [ -f delivery/vars.env ] || return 0")
	w("  local line k v")
	w("  while IFS= read -r line; do")
	w("    case \"$line\" in ''|'#'*) continue ;; esac")
	w("    k=${line%%=*}; v=${line#*=}")
	w("    case \"$k\" in ''|*[!A-Z0-9_]*) continue ;; esac")
	w("    printf -v \"V_$k\" '%%s' \"$v\"")
	w("  done < delivery/vars.env")
	w("}")
	w("")
	w("# 询问变量：预填值优先 > 交互输入（回车取默认；sensitive=1 输入隐藏）")
	w("prompt_var() { # name label default sensitive")
	w("  local name=$1 label=$2 def=$3 sens=$4 cur input=''")
	w("  cur=$(getv \"$name\")")
	w("  if [ -n \"$cur\" ]; then printf '  %%s=***（delivery/vars.env 预填）\\n' \"$name\"; return 0; fi")
	w("  if [ \"$sens\" = 1 ]; then")
	w("    read -rs -p \"  $label [$name]（回车取默认，输入隐藏）: \" input </dev/tty || true; printf '\\n'")
	w("  else")
	w("    read -r -p \"  $label [$name]（默认 $def）: \" input </dev/tty || true")
	w("  fi")
	w("  printf -v \"V_$name\" '%%s' \"${input:-$def}\"")
	w("}")
	w("")
	w("# 完整性校验：manifest.json 逐文件 SHA-256（大包校验耗时，--skip-verify 可跳过）")
	w("verify_manifest() {")
	w("  [ -f manifest.json ] || { bad \"缺少 manifest.json\"; return 0; }")
	w("  local line f h got n=0 badcnt=0")
	w("  while IFS= read -r line; do")
	w("    f=${line%%:*}; f=${f//[^a-zA-Z0-9._\\/-]/}")
	w("    h=${line##*:}; h=${h//[^0-9a-f]/}")
	w("    [ -n \"$f\" ] && [ -n \"$h\" ] || continue")
	w("    if [ ! -f \"$f\" ]; then bad \"缺文件 $f\"; badcnt=$((badcnt+1)); continue; fi")
	w("    got=$(sha256sum \"$f\" 2>/dev/null | awk '{print $1}')")
	w("    if [ \"$got\" != \"$h\" ]; then bad \"校验失败 $f\"; badcnt=$((badcnt+1)); fi")
	w("    n=$((n+1))")
	w("  done < <(grep -oE '\"[^\"]+\"[[:space:]]*:[[:space:]]*\"[0-9a-f]{64}\"' manifest.json)")
	w("  [ \"$badcnt\" -gt 0 ] && return 0")
	w("  ok \"完整性校验通过（$n 个文件）\"")
	w("}")
	w("")
	w("# 健康门控：HTTP 轮询（无 curl 退化为容器状态探测）")
	w("wait_http() { # name url timeout_sec")
	w("  local name=$1 url=$2 timeout=$3 elapsed=0")
	w("  if ! command -v curl >/dev/null 2>&1; then printf '  等待 %%s：无 curl，退化为容器状态探测\\n' \"$name\"; wait_state \"$name\"; return 0; fi")
	w("  printf '  等待 %%s 健康（%%s）' \"$name\" \"$url\"")
	w("  until curl -fsS -o /dev/null --max-time 3 \"$url\" 2>/dev/null; do")
	w("    sleep 3; elapsed=$((elapsed+3))")
	w("    if [ \"$elapsed\" -ge \"$timeout\" ]; then printf ' 超时\\n'; bad \"$name 健康等待超时（${timeout}s）\"; return 0; fi")
	w("    printf '.'")
	w("  done")
	w("  printf ' ✔\\n'")
	w("}")
	w("wait_state() { # name —— 容器状态探测（compose depends_on healthcheck 之外的兜底）")
	w("  local name=$1 cid st")
	w("  cid=$(docker compose ps --quiet \"$name\" 2>/dev/null || true)")
	w("  if [ -z \"$cid\" ]; then bad \"服务 $name 无运行容器\"; return 0; fi")
	w("  st=$(docker inspect -f '{{.State.Status}}' \"$cid\" 2>/dev/null || true)")
	w("  [ \"$st\" = running ] && ok \"服务 $name 运行中（容器状态探测）\" || bad \"服务 $name 状态异常: $st\"")
	w("}")
	w("")
	w("# ---- 1. 预检 ----")
	w("log \"单机安装预检：$PKG_DIR\"")
	w("command -v docker >/dev/null 2>&1 || { echo '✘ 未安装 docker（本脚本仅覆盖单机全 Docker 场景）'; exit 1; }")
	w("docker compose version >/dev/null 2>&1 || { echo '✘ 缺少 docker compose v2 插件'; exit 1; }")
	w("ok \"docker / compose v2 在位\"")
	w("")

	// ---- 2. 变量收集（清单打包期烘焙：const 直接赋值 / random 缺省生成 / input·ask 交互询问） ----
	w("# ---- 2. 安装期变量与凭据（预填 > 交互/固定/随机） ----")
	w("load_prefill")
	hasPrompt := false
	for i := range app.Variables {
		v := &app.Variables[i]
		if v.When != "install" {
			continue
		}
		hasPrompt = true
		label := orDefault(v.Label, v.Name)
		switch orDefault(v.Source, "input") {
		case "const":
			w("V_%s=%s  # %s（固定值）", v.Name, shq(v.Value), label)
		case "random":
			w("eval 'V_%s=${V_%s:-$(rand16)}'  # %s（随机，预填可覆盖）", v.Name, v.Name, label)
		default:
			w("prompt_var %s %s %s %d  # %s", v.Name, shq(label), shq(v.Value), boolInt(v.Sensitive), label)
		}
	}
	for _, c := range comps {
		for i := range c.Credentials {
			cr := &c.Credentials[i]
			hasPrompt = true
			label := orDefault(cr.Label, cr.Placeholder)
			switch cr.Generate {
			case "random":
				w("eval 'V_%s=${V_%s:-$(rand16)}'  # %s（随机，预填可覆盖）", cr.Placeholder, cr.Placeholder, label)
			case "const":
				w("V_%s=%s  # %s（固定值）", cr.Placeholder, shq(cr.Value), label)
			default: // ask：留空随机生成（口径同 console「或留空随机生成」）
				w("prompt_var %s %s '' 1  # %s（凭据）", cr.Placeholder, shq(label), label)
				w("eval 'V_%s=${V_%s:-$(rand16)}'  # %s（留空随机）", cr.Placeholder, cr.Placeholder, label)
			}
		}
	}
	if !hasPrompt {
		w("printf '  （无安装期变量与凭据）\\n'")
	}
	// 端点常量（单机全栈口径，打包期算死：容器网络互访）
	for _, c := range comps {
		for _, ep := range c.Endpoints {
			w("V_%s=%s  # 组件 %s 端点（单机=容器网络互访）", ep.Placeholder, shq(endpointStandaloneValue(ep)), c.Code)
		}
	}
	w("")

	// ---- 3. 完整性校验 ----
	w("# ---- 3. 完整性校验 ----")
	w("if [ \"$SKIP_VERIFY\" = 1 ]; then printf '  跳过（--skip-verify）\\n'; else verify_manifest; fi")
	w("[ \"$FAIL\" = 0 ] || { echo '✘ 完整性校验未通过，终止（确认介质完好或 --skip-verify 强制继续）'; exit 1; }")
	w("")

	// ---- 4. 镜像导入 ----
	w("# ---- 4. 导入镜像（images/*.tar） ----")
	w("log \"导入镜像\"")
	w("shopt -s nullglob")
	w("for img in images/*.tar; do")
	w("  printf '  docker load %%s ... ' \"$img\"")
	w("  if docker load -q -i \"$img\" >/dev/null 2>&1; then printf '✔\\n'; else bad \"镜像导入失败: $img\"; fi")
	w("done")
	w("shopt -u nullglob")
	w("")

	// ---- 5. 配置物化 ----
	w("# ---- 5. 配置物化（.env + data/conf/ + delivery 载荷） ----")
	w("log \"渲染配置\"")
	// .env：组件凭据（env 缺省=占位符名）——与 console resolveVars 同源（compose ${VAR} 引用同一来源）
	w("{")
	for _, c := range comps {
		for i := range c.Credentials {
			cr := &c.Credentials[i]
			w("  printf '%%s=\"%%s\"\\n' %s \"$(getv %s)\"", shq(orDefault(cr.Env, cr.Placeholder)), cr.Placeholder)
		}
	}
	w("} > .env")
	w("ok '.env（组件凭据 → compose 变量同源）'")
	// config-template.yaml → data/conf/application.yaml（占位符用 bash 子串替换，免 sed 转义）
	if hasConf {
		w("mkdir -p data/conf")
		w("CONF=$(cat config-template.yaml)")
		for i := range app.Variables {
			v := &app.Variables[i]
			if v.When == "install" {
				w("CONF=${CONF//\\{\\{%s\\}\\}/$(getv %s)}", v.Name, v.Name)
			}
		}
		for _, c := range comps {
			for i := range c.Credentials {
				w("CONF=${CONF//\\{\\{%s\\}\\}/$(getv %s)}", c.Credentials[i].Placeholder, c.Credentials[i].Placeholder)
			}
			for _, ep := range c.Endpoints {
				w("CONF=${CONF//\\{\\{%s\\}\\}/$(getv %s)}", ep.Placeholder, ep.Placeholder)
			}
		}
		w("printf '%%s\\n' \"$CONF\" > data/conf/application.yaml")
		w("ok 'data/conf/application.yaml'")
	}
	// delivery 载荷 → data/conf/（对齐 console：入口主机配置目录，vars.env 除外）
	w("if [ -d delivery ]; then")
	w("  mkdir -p data/conf")
	w("  (cd delivery && find . -type f ! -name vars.env -exec cp -f --parents {} ../data/conf/ \\;)")
	w("  ok 'delivery/ 载荷 → data/conf/'")
	w("fi")
	w("")

	// ---- 6. 编排拉起 ----
	w("# ---- 6. 拉起全栈（docker compose up） ----")
	w("log \"docker compose up -d --remove-orphans\"")
	w("docker compose up -d --remove-orphans || { echo '✘ 编排拉起失败（docker compose logs 排查）'; exit 1; }")
	w("")

	// ---- 7. 健康门控（逐单元：有健康路径的轮询 HTTP；未发布端口退化容器状态；聚合超时 600s 同 console） ----
	w("# ---- 7. 健康门控 ----")
	w("log \"健康门控\"")
	for i := range app.Units {
		u := &app.Units[i]
		if u.HealthPath == "" || u.Port == "" {
			continue
		}
		block := renderPlaceholders(u.ComposeTemplate, unitRenderVals(u, buildVals))
		svc := firstComposeServiceName(block)
		if svc == "" {
			continue
		}
		if hp := composeHostPort(block, u.Port); hp != "" {
			w("wait_http %s %s 600", shq(svc), shq("http://127.0.0.1:"+hp+u.HealthPath))
		} else {
			w("printf '  单元 %s 有健康路径但未发布端口 %s，退化为容器状态探测\\n' %s", u.Code, u.Port, shq(svc))
			w("wait_state %s", shq(svc))
		}
	}
	w("")

	// ---- 8. 汇总 ----
	w("# ---- 8. 安装汇总 ----")
	w("if [ \"$FAIL\" = 0 ]; then log '安装完成 ✔'; else echo '⚠ 存在失败项，请检查上方日志'; fi")
	w("HOST_IP=$(hostname -I 2>/dev/null | awk '{print $1}')")
	w("HOST_IP=${HOST_IP:-127.0.0.1}")
	if entry != nil {
		entryBlock := renderPlaceholders(entry.ComposeTemplate, unitRenderVals(entry, buildVals))
		entrySvc := firstComposeServiceName(entryBlock)
		if hp := composeHostPort(entryBlock, orDefault(entry.Port, "8080")); entrySvc != "" && hp != "" {
			w("printf '  入口服务：%%s  访问地址：http://%%s:%s/\\n' %s \"$HOST_IP\"", hp, shq(entrySvc))
		} else if entrySvc != "" {
			w("printf '  入口服务：%%s（未发布宿主端口——容器网络内以服务名访问）\\n' %s", shq(entrySvc))
		}
	}
	w("printf '  安装目录：%%s（.env 与 data/conf/ 调整后重跑本脚本生效）\\n' \"$PKG_DIR\"")
	w("printf '  升级/回退/多机/审计 → deliver-console 安装控制台\\n'")
	w("exit $FAIL")
	return b.String()
}

// unitRenderVals 编排块渲染取值（打包期口径）：单元内置 + 打包时变量（镜像引用与端口/健康解析无关，
// UNIT_IMAGE 置空即可——渲染只为把 ports 的 {{UNIT_PORT}} 等占位符落成具体值）
func unitRenderVals(u *Unit, buildVals map[string]string) map[string]string {
	vals := map[string]string{builtinUnitImage: "", builtinUnitPort: orDefault(u.Port, "8080"), builtinArtifactFile: ""}
	for k, v := range buildVals {
		vals[k] = v
	}
	return vals
}
