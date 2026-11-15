// compose 解析助手（服务块切分/端口/镜像/依赖/环境引用）——与 deliver-studio model.go 同口径的
// 独立实现（工具族惯例：两工具各自可整体外迁，本文件即 console 侧副本）。
package main

import (
	"fmt"
	"regexp"
	"strings"
)

var composeSvcLineRe = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):`)

// splitServices 把 compose 全文按 2 空格缩进顶层服务键切成 服务名 → 服务块文本。
// 容忍头部 name:/services: 行与非 services 区段（volumes:/networks: 顶层 0 缩进键不属于服务块）。
func splitServices(composeText string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(composeText, "\n")
	inServices := false
	cur := ""
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.Join(buf, "\n")
		}
		cur, buf = "", nil
	}
	for _, line := range lines {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil {
			flush()
			cur, buf = m[1], []string{line}
			continue
		}
		if cur != "" {
			// 更深缩进（≥4 空格）或空行/注释归本服务块；回到 0 缩进即 services 区结束
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
				buf = append(buf, line)
				continue
			}
			flush()
			inServices = false
			continue
		}
		_ = inServices
	}
	flush()
	return out
}

// serviceOrder compose 全文中的服务出现顺序（稳定展示/拼接顺序）
func serviceOrder(composeText string) []string {
	var out []string
	for _, line := range strings.Split(composeText, "\n") {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

var (
	portsKeyRe  = regexp.MustCompile(`^    ports:\s*(\[[^\]]*\])?\s*$`)
	portsItemRe = regexp.MustCompile(`^ {6,}-\s*"?([0-9]+(?::[0-9]+)?(?::/[a-z]+)?|"[^"]+")"?`)
	imageLineRe = regexp.MustCompile(`^\s+image:\s*(\S+)`)
)

// servicePorts 解析服务块 ports: 发布映射：容器端口 → 宿主端口（"宿主:容器" / "容器"同值）。
// 返回 map[containerPort]hostPort；未发布的容器端口不在结果中。
func servicePorts(block string) map[string]string {
	out := map[string]string{}
	inPorts := false
	for _, line := range strings.Split(block, "\n") {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil && inPorts {
			inPorts = false
		}
		if m := portsKeyRe.FindStringSubmatch(line); m != nil {
			inPorts = true
			continue
		}
		if !inPorts {
			continue
		}
		m := portsItemRe.FindStringSubmatch(line)
		if m == nil {
			if line != "" && !strings.HasPrefix(line, "      ") {
				inPorts = false
			}
			continue
		}
		spec := strings.Trim(m[1], `"`)
		if h, c, ok := strings.Cut(spec, ":"); ok {
			out[c] = h
		} else {
			out[spec] = spec // "8080" → 宿主同值
		}
	}
	return out
}

// serviceImage 服务块 image: 引用原文（可能含 ${VAR} 引用，调用方负责 resolveEnvRefs）
func serviceImage(block string) string {
	for _, line := range strings.Split(block, "\n") {
		if m := imageLineRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

var (
	depKeyRe  = regexp.MustCompile(`^    depends_on:\s*(\[[^\]]*\])?\s*$`)
	depItemRe = regexp.MustCompile(`^ {6,}-\s*([A-Za-z0-9_.-]+)`)
)

// serviceDeps 服务块 depends_on 目标（兼容块列表与 flow 列表）
func serviceDeps(block string) []string {
	var out []string
	inDeps := false
	for _, line := range strings.Split(block, "\n") {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil {
			inDeps = false
			continue
		}
		if m := depKeyRe.FindStringSubmatch(line); m != nil {
			inDeps = true
			if m[1] != "" {
				for _, item := range strings.Split(strings.Trim(m[1], "[]"), ",") {
					item = strings.Trim(strings.TrimSpace(item), `'"`)
					if item != "" {
						out = append(out, item)
					}
				}
				inDeps = false
			}
			continue
		}
		if inDeps {
			if m := depItemRe.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
				continue
			}
			if line != "" && !strings.HasPrefix(line, "      ") {
				inDeps = false
			}
		}
	}
	return out
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// stripDanglingDeps 剥离服务块 depends_on 中 keep 为 false 的依赖项：拼主机子集 compose 时，
// 指向「不在本机的服务」（跨机 / 外部复用 / systemd 裸机）的悬空引用会让 docker compose up 直接报
// undefined service。保留项重新写回（块列表/flow 列表两种语法与 serviceDeps 对齐）；全空则整段删除。
func stripDanglingDeps(block string, keep func(dep string) bool) string {
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		m := depKeyRe.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		if m[1] != "" { // flow 列表：depends_on: [a, b]
			var kept []string
			for _, item := range strings.Split(strings.Trim(m[1], "[]"), ",") {
				item = strings.Trim(strings.TrimSpace(item), `'"`)
				if item != "" && keep(item) {
					kept = append(kept, item)
				}
			}
			if len(kept) > 0 {
				out = append(out, "    depends_on: ["+strings.Join(kept, ", ")+"]")
			}
			continue
		}
		var kept []string // 块列表：吞掉后续 6+ 空格 "- x" 行
		j := i + 1
		for j < len(lines) {
			im := depItemRe.FindStringSubmatch(lines[j])
			if im == nil {
				break
			}
			if keep(im[1]) {
				kept = append(kept, im[1])
			}
			j++
		}
		if len(kept) > 0 {
			out = append(out, "    depends_on:")
			for _, d := range kept {
				out = append(out, "      - "+d)
			}
		}
		i = j - 1
	}
	return strings.Join(out, "\n")
}

// injectConfLabel 服务块注入配置指纹 label（K8s checksum annotation 模式）：docker compose 感知
// 不到 bind 挂载的内容变化，label 值随配置哈希变 → up 判定配置变更 recreate 容器，新配置真正生效
// （否则升级只写盘不重启，配置落空）。已有 labels 段则其后追加，否则按块内既有缩进块尾新建；
// 注意 inventory 基线的 blockHash 基于包内原文块（不含本注入），存量增量基线零失效。
func injectConfLabel(blk, hash string) string {
	lines := strings.Split(strings.TrimRight(blk, "\n"), "\n")
	indentOf := func(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }
	for i, l := range lines {
		if strings.TrimSpace(l) == "labels:" && indentOf(l) != "" {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, indentOf(l)+"  "+fmt.Sprintf("mse.conf-hash: %q", hash))
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	prop := "    " // 缺省属性缩进（服务键 2 空格、属性 4 空格的标准层级）
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) != "" {
			prop = indentOf(l)
			break
		}
	}
	if len(prop) < 2 {
		prop = "    "
	}
	// labels 与 image 等属性行同级（prop），mse.conf-hash 再进一层（prop+2）——labels 若落在
	// 服务键层级（2 空格）会被 compose 解析成另一个服务
	return strings.TrimRight(blk, "\n") + "\n" + prop + "labels:\n" +
		prop + "  " + fmt.Sprintf("mse.conf-hash: %q", hash)
}

// injectResourceLimits 服务块注入资源限额（K8s resources.limits 对应物）：物化为
// deploy.resources.limits（docker compose v2 up 生效；v1 回退忽略无害）。块内已有 deploy: 段则
// 跳过（文本级无合并能力，防重复键坏文件）。inventory 侧以 resHash 参与 diff（不进 blockHash）。
func injectResourceLimits(blk string, res ResourceSpec) string {
	if res.Cpus == "" && res.MemMB <= 0 {
		return blk
	}
	lines := strings.Split(strings.TrimRight(blk, "\n"), "\n")
	prop := "    " // 缺省属性缩进（服务键 2 空格、属性 4 空格的标准层级）
	indentOf := func(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		prop = indentOf(l)
		break
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "deploy:" {
			return blk // 已有 deploy 段：不注入（防 YAML 重复键）
		}
	}
	var out strings.Builder
	out.WriteString(strings.TrimRight(blk, "\n") + "\n")
	out.WriteString(prop + "deploy:\n")
	out.WriteString(prop + "  resources:\n")
	out.WriteString(prop + "    limits:\n")
	if res.Cpus != "" {
		fmt.Fprintf(&out, "%s      cpus: %q\n", prop, res.Cpus)
	}
	if res.MemMB > 0 {
		fmt.Fprintf(&out, "%s      memory: %dM\n", prop, res.MemMB)
	}
	return out.String()
}

// resolveEnvRefs 把 ${VAR} / ${VAR:-默认} 按已知变量值展开（未知且无默认 → 原样保留）
func resolveEnvRefs(text string, vars map[string]string) string {
	return envRefRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := envRefRe.FindStringSubmatch(m)
		name, def := sub[1], sub[3]
		if v, ok := vars[name]; ok && v != "" {
			return v
		}
		if sub[2] != "" { // 带 :- 默认段
			return def
		}
		return m
	})
}

// safeImageTarName 镜像引用 → 包内 tar 文件名（与 deliver-studio engine.go safeImageName 同规则：
// ref 中 / 与 : → _）
func safeImageTarName(ref string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(ref) + ".tar"
}
