// 本机节点执行器：console 所在机器作为一个部署主机（单机场景零 agent），与 deliver-agent
// tasks.go 同构的进程内实现（load 从包目录直拷/compose/systemd/cmd/wait），日志直写会话。
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// probeLocalHostInfo 本机能力档案（hosts 表 local=1 行）
func probeLocalHostInfo() HostInfo {
	info := HostInfo{Hostname: localHostname(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUCore: runtime.NumCPU(), AgentVersion: "console-local"}
	info.IPs = localIPs()
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		info.Docker, info.DockerVer = true, strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
		info.Compose, info.ComposeVer = true, strings.TrimSpace(string(out))
	}
	if runtime.GOOS == "linux" {
		if fi, err := os.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
			info.Systemd = true
		}
	}
	if out, err := exec.Command("java", "-version").CombinedOutput(); err == nil {
		if m := javaVerReLocal.FindStringSubmatch(string(out)); m != nil {
			info.JavaVer = m[1]
		}
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
					if f := strings.Fields(rest); len(f) >= 1 {
						var kb int64
						_, _ = fmt.Sscanf(f[0], "%d", &kb)
						info.MemMB = kb / 1024
					}
				}
			}
		}
	}
	return info
}

var javaVerReLocal = regexp.MustCompile(`version "([^"]+)"`)

func localHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "local"
	}
	return strings.SplitN(h, ".", 2)[0]
}

func localIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// appDir 本机应用部署目录（<root>/local/apps/<app>）
func (s *Server) appDir(app string) string {
	return filepath.Join(s.root, "local", "apps", sanitizeSeg(app))
}

// localExec 本机节点任务执行（payload 与 agent 协议同构；报告直写会话）
func (s *Server) localExec(r *sessionRun, host *hostRow, n *planNode) {
	report := func(status, msg string, logs []string) { r.Report(n.Key, status, msg, logs) }
	report("running", "", []string{fmt.Sprintf("[local] %s（%s）", n.Title, n.Kind)})
	var err error
	switch n.Kind {
	case "load":
		var p LoadPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localLoad(r.pkgDir, p, report)
		}
	case "compose":
		var p ComposePayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localCompose(p, report)
		}
	case "systemd":
		var p SystemdPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localSystemd(p, report)
		}
	case "cmd":
		var p CmdPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localCmd(p, nil, report)
		}
	case "wait":
		var p WaitPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localWait(p, report)
		}
	case "plugin":
		var p PluginPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localPlugin(p, report)
		}
	case "docker":
		var p DockerPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localDocker(p, report)
		}
	case "toolkit":
		var p ToolkitPayload
		if err = json.Unmarshal(n.Payload, &p); err == nil {
			err = s.localToolkit(p, report)
		}
	default:
		err = fmt.Errorf("本机节点不支持任务类型 %q", n.Kind)
	}
	if err != nil {
		report("failed", err.Error(), nil)
	} else {
		report("ok", "", nil)
	}
}

// localLoad 包目录 → 本机 app 目录（哈希一致跳过），images/*.tar 自动 docker load
func (s *Server) localLoad(pkgDir string, p LoadPayload, report func(string, string, []string)) error {
	base := s.appDir(p.App)
	loaded := 0
	for _, f := range p.Files {
		src := filepath.Join(pkgDir, filepath.FromSlash(f.Path))
		rel := orStr(f.Dest, f.Path)
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if f.SHA256 != "" {
			if got, err := fileSHA256(dst); err == nil && got == f.SHA256 {
				report("running", "", []string{"已存在且校验一致，跳过: " + rel})
				continue
			}
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("读取包内文件 %s 失败: %w", f.Path, err)
		}
		if f.SHA256 != "" {
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != f.SHA256 {
				return fmt.Errorf("包内文件 %s 校验不符", f.Path)
			}
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return err
		}
		report("running", "", []string{fmt.Sprintf("已就位 %s（%.1fMB）", rel, float64(len(data))/(1<<20))})
		if strings.HasPrefix(f.Path, "images/") && strings.HasSuffix(f.Path, ".tar") {
			if err := localRun(report, "docker", "load", "-i", dst); err != nil {
				return err
			}
			loaded++
		}
	}
	report("running", "", []string{fmt.Sprintf("完成：文件 %d 个，镜像 %d 个", len(p.Files), loaded)})
	return nil
}

var dataBindRe = regexp.MustCompile(`\./data/([A-Za-z0-9_.-]+)`)

func (s *Server) localCompose(p ComposePayload, report func(string, string, []string)) error {
	base := s.appDir(p.App)
	compFile := filepath.Join(base, "docker-compose.yml")
	if err := os.WriteFile(compFile, []byte(p.Compose), 0644); err != nil {
		return err
	}
	if p.Env != "" {
		if err := os.WriteFile(filepath.Join(base, ".env"), []byte(p.Env), 0600); err != nil {
			return err
		}
	}
	for rel, content := range p.Files {
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return err
		}
	}
	seen := map[string]bool{"data/conf": true, "data/logs": true}
	for _, m := range dataBindRe.FindAllStringSubmatch(p.Compose, -1) {
		seen["data/"+m[1]] = true
	}
	for dir := range seen {
		_ = os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0755)
	}
	args := []string{"compose", "-f", compFile, "up", "-d", "--remove-orphans"}
	if len(p.Services) > 0 {
		args = append(args, p.Services...) // 就绪分段子集 up（空=全部）
	}
	// 端口前置自检（与 agent execCompose 同口径）：异源占用直接失败带归因，不进 up 也不回退 v1
	if len(p.Ports) > 0 {
		if dets := localPortConflicts(p.Ports, p.Project); len(dets) > 0 {
			for _, d := range dets {
				report("running", "", []string{"端口冲突：" + d})
			}
			return fmt.Errorf("宿主端口被占用，已跳过编排拉起：%s（调整分配或清理占用后重试）", strings.Join(dets, "；"))
		}
		report("running", "", []string{"端口预检通过（" + strings.Join(p.Ports, ", ") + "）"})
	}
	if err := localRun(report, "docker", args...); err == nil {
		return nil
	}
	// 失败后即时自检：端口被占（回退 v1 也必然同样失败）直接报归因，不再无意义重演一次
	if dets := localPortConflicts(p.Ports, p.Project); len(dets) > 0 {
		for _, d := range dets {
			report("running", "", []string{"端口冲突：" + d})
		}
		return fmt.Errorf("docker compose 失败（宿主端口被占用）：%s", strings.Join(dets, "；"))
	}
	report("running", "", []string{"docker compose 失败，回退 docker-compose……"})
	return localRun(report, "docker-compose", args...)
}

// localContainerPorts 本机运行容器端口映射（console 进程内即时探测；与 agent ports.go 同口径，
// docker 不可用返回 nil——预检降级）
func localContainerPorts() []ContainerPort {
	out, err := exec.Command("docker", "ps", "--format",
		"{{.Names}}\t{{.Image}}\t{{.Ports}}\t{{.Label \"com.docker.compose.project\"}}").Output()
	if err != nil {
		return nil
	}
	var cs []ContainerPort
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		c := ContainerPort{Name: f[0], Image: f[1]}
		for _, seg := range strings.Split(f[2], ",") {
			seg = strings.TrimSpace(seg)
			head, _, ok := strings.Cut(seg, "->")
			if !ok {
				continue
			}
			if i := strings.LastIndex(head, ":"); i >= 0 && i+1 < len(head) {
				if port := head[i+1:]; port != "" {
					c.HostPorts = append(c.HostPorts, port)
				}
			}
		}
		if len(f) >= 4 {
			c.Project = f[3]
		}
		cs = append(cs, c)
	}
	return cs
}

// localPortConflicts 本机端口冲突明细（本项目容器占用=复用不算冲突；试绑兜底裸进程——只判
// 忙闲不深究进程名，console 端无 ss/netstat 归因依赖）
func localPortConflicts(ports []string, project string) []string {
	if len(ports) == 0 {
		return nil
	}
	cs := localContainerPorts()
	byPort := map[string]ContainerPort{}
	for _, c := range cs {
		for _, p := range c.HostPorts {
			if _, ok := byPort[p]; !ok {
				byPort[p] = c
			}
		}
	}
	var conflicts []string
	for _, p := range ports {
		if c, ok := byPort[p]; ok {
			if project != "" && c.Project == project {
				continue // 本项目容器：重装/升级复用
			}
			conflicts = append(conflicts, fmt.Sprintf("端口 %s 已被容器 %s（镜像 %s）占用", p, c.Name, c.Image))
			continue
		}
		if ln, err := net.Listen("tcp", ":"+p); err != nil {
			conflicts = append(conflicts, fmt.Sprintf("端口 %s 已被非容器进程占用", p))
		} else {
			_ = ln.Close()
		}
	}
	return conflicts
}

func (s *Server) localSystemd(p SystemdPayload, report func(string, string, []string)) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd 任务仅支持 Linux 主机")
	}
	base := s.appDir(p.App)
	for rel, content := range p.ConfigFiles {
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return err
		}
	}
	if p.EnvFile != "" {
		if err := os.WriteFile(filepath.Join(base, ".env"), []byte(p.EnvFile), 0600); err != nil {
			return err
		}
	}
	unit := strings.ReplaceAll(p.UnitFile, "{{APP_DIR}}", base)
	unitPath := filepath.Join("/etc/systemd/system", filepath.Base(p.Unit))
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("写 unit 失败（需要 root）: %w", err)
	}
	if err := localRun(report, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := localRun(report, "systemctl", "enable", p.Unit); err != nil {
		return err
	}
	return localRun(report, "systemctl", "restart", p.Unit)
}

// localCmd 本机一次性命令（payload 与 agent execCmd 同构）。stop 非空时支持终端中断。
// 不用 exec.CommandContext（超时只杀主进程，Windows 树杀因根进程已死失效）——超时/中断统一由
// 杀手 goroutine 整组终止，防孤儿子进程握管道令收尾永久挂起；中断返回「已人工中断」。
func (s *Server) localCmd(p CmdPayload, stop <-chan struct{}, report func(string, string, []string)) error {
	cwd := s.appDir(p.App)
	if p.Cwd != "" { // 终端会话记忆的工作目录（cd 跨命令生效的落点）
		cwd = p.Cwd
	}
	if err := os.MkdirAll(cwd, 0755); err != nil {
		return err
	}
	timeout := time.Duration(orInt(p.TimeoutSec, 600)) * time.Second
	script := p.Script
	if p.TrackCwd { // 终端会话：末尾追加 PWD 哨兵行（落库前剥离并记忆，cd 跨命令生效）
		script += cwdSentinelSuffix()
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", script)
	} else {
		cmd = exec.Command("sh", "-c", script)
	}
	setPgroup(cmd) // 独立进程组：终端中断/超时可整组终止（sh 与其子进程，如 docker logs -f）
	cmd.Dir = cwd
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	procDone := make(chan struct{})
	defer close(procDone)
	killerFired := make(chan string, 1) // 杀手动武原因（stop|timeout），先发后杀保证返回时可见
	if stop != nil {
		go func() {
			reason := ""
			select {
			case <-stop:
				reason = "stop"
			case <-timer.C:
				reason = "timeout"
			case <-procDone:
				return
			}
			killerFired <- reason
			killGroup(cmd)
		}()
	}
	report("running", "", []string{"$ " + p.Script})
	err := localRunIO(report, cmd)
	if stop != nil {
		select {
		case <-stop:
			return fmt.Errorf("已人工中断")
		default:
		}
		select {
		case <-killerFired:
			return fmt.Errorf("执行超时（上限 %d 秒）", int(timeout.Seconds()))
		default:
		}
	}
	return err
}

// cwdSentinelSuffix 终端会话 PWD 哨兵行（与 deliver-agent/tasks.go 同构，双模块各自维护）
func cwdSentinelSuffix() string {
	if runtime.GOOS == "windows" {
		return "\nWrite-Output \"$([char]1)CWD:$($PWD.ProviderPath)$([char]2)\""
	}
	return "\nprintf '\\001CWD:%s\\002\\n' \"$PWD\""
}

var localProbeClient = &http.Client{Timeout: 3 * time.Second}

// localPluginBin 内嵌插件二进制物化到 <root>/local/plugins/（内容变化时重写），返回可执行路径。
// pluginDirOverride 非空时优先直用该目录（测试/插件开发，跳过内嵌与大小判别）。
func (s *Server) localPluginBin(plugin, goos string) (string, error) {
	file := pluginFileName(plugin, goos)
	if s.pluginDirOverride != "" {
		p := filepath.Join(s.pluginDirOverride, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("插件目录覆盖下未找到 %s", p)
	}
	data, err := pluginBinFS(file)
	if err != nil {
		return "", err
	}
	if len(data) < 100*1024 {
		return "", fmt.Errorf("插件 %s 是占位文件（<100KB）——请先执行 build.sh 完整构建 console", file)
	}
	dir := filepath.Join(s.root, "local", "plugins")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, file)
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if got, err := fileSHA256(dst); err != nil || got != want {
		if err := os.WriteFile(dst, data, 0755); err != nil {
			return "", err
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dst, 0755)
	}
	return dst, nil
}

// pluginRequest 组装 stdio JSON 协议请求（op + args：连接参数并入 mode/items）
func pluginRequest(p PluginPayload) []byte {
	args := map[string]any{}
	for k, v := range p.Config {
		args[k] = v
	}
	if p.Mode != "" {
		args["mode"] = p.Mode
	}
	if len(p.Items) > 0 {
		args["items"] = p.Items
	}
	b, _ := json.Marshal(map[string]any{"op": p.Op, "args": args})
	return b
}

// runPlugin spawn 插件子进程执行单次调用：stdin 一行请求 → stdout 一行响应（stderr 逐行入日志）。
// 与 deliver-agent execPlugin 同构（协议见 docs/tech/deliver-design.md ADR-4）。
func runPlugin(report func(string, string, []string), bin string, p PluginPayload) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(orInt(p.TimeoutSec, 300))*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	stderr, err3 := cmd.StderrPipe()
	if err1 != nil || err2 != nil || err3 != nil {
		return fmt.Errorf("插件进程管道创建失败")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动插件 %s 失败: %w", bin, err)
	}
	if _, err := stdin.Write(append(pluginRequest(p), '\n')); err != nil {
		return err
	}
	_ = stdin.Close()
	go func() { // 插件进度（stderr）逐行上行
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			report("running", "", []string{"[plugin] " + sc.Text()})
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	werr := cmd.Wait()
	_ = stderr.Close()
	if err != nil {
		return fmt.Errorf("读取插件响应失败: %v（wait: %v）", err, werr)
	}
	var resp struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data,omitempty"`
		Error string          `json:"error,omitempty"`
	}
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); jerr != nil {
		return fmt.Errorf("插件响应解析失败: %v（原文: %.200s）", jerr, line)
	}
	if !resp.OK {
		return fmt.Errorf("插件执行失败: %s", orStr(resp.Error, "未知错误"))
	}
	if len(resp.Data) > 0 {
		report("running", "", []string{"结果: " + string(resp.Data)})
	}
	return nil
}

func (s *Server) localPlugin(p PluginPayload, report func(string, string, []string)) error {
	if p.Plugin == "" || p.Op == "" {
		return fmt.Errorf("plugin 任务缺少 plugin/op")
	}
	bin, err := s.localPluginBin(p.Plugin, p.OS)
	if err != nil {
		return err
	}
	report("running", "", []string{fmt.Sprintf("$ %s %s（%s）", filepath.Base(bin), p.Op, p.Plugin)})
	return runPlugin(report, bin, p)
}

func (s *Server) localWait(p WaitPayload, report func(string, string, []string)) error {
	probe := func() (bool, string) {
		switch p.Type {
		case "http":
			resp, err := localProbeClient.Get(p.URL)
			if err != nil {
				return false, "连接失败"
			}
			defer resp.Body.Close()
			return resp.StatusCode == http.StatusOK, fmt.Sprintf("HTTP %d", resp.StatusCode)
		case "docker":
			for _, name := range []string{p.Project + "-" + p.Service + "-1", p.Project + "_" + p.Service + "_1"} {
				out, err := exec.Command("docker", "inspect", "--format",
					`{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}`, name).Output()
				if err == nil {
					st := strings.TrimSpace(string(out))
					return st == "healthy" || st == "running", st
				}
			}
			return false, "容器未找到"
		case "systemd":
			if err := exec.Command("systemctl", "is-active", p.Unit).Run(); err == nil {
				return true, "active"
			}
			return false, "inactive"
		case "tcp":
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(p.Host, p.Port), 3*time.Second)
			if err != nil {
				return false, "连接失败"
			}
			_ = conn.Close()
			return true, "TCP 可连"
		}
		return false, "未知探测类型"
	}
	timeout := time.Duration(orInt(p.TimeoutSec, 600)) * time.Second
	deadline := time.Now().Add(timeout)
	for {
		if ok, detail := probe(); ok {
			report("running", "", []string{"健康检查通过（" + p.Type + "）"})
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("健康等待超时（%s，%s）", p.Type, detail)
		}
		time.Sleep(2 * time.Second)
	}
}

// ---- 执行与日志（本机直采，逐行作为报告日志上行） ----

func localRun(report func(string, string, []string), name string, args ...string) error {
	report("running", "", []string{fmt.Sprintf("$ %s %s", name, strings.Join(args, " "))})
	return localRunIO(report, exec.Command(name, args...))
}

func localRunIO(report func(string, string, []string), cmd *exec.Cmd) error {
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err1 != nil || err2 != nil {
		return fmt.Errorf("管道创建失败: %v / %v", err1, err2)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(rd io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var batch []string
		for sc.Scan() {
			batch = append(batch, sc.Text())
			if len(batch) >= 20 {
				report("running", "", batch)
				batch = nil
			}
		}
		if len(batch) > 0 {
			report("running", "", batch)
		}
	}
	go scan(stdout)
	go scan(stderr)
	wg.Wait()
	return cmd.Wait()
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
