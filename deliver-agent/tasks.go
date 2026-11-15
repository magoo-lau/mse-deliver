package main

// 任务执行器（八类节点，全部幂等可重试；docker 见 dockertask.go、compose 工具维护见 toolkittask.go）
// 与日志上报器。

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

// appDir 主机上的应用部署目录：<data-dir>/apps/<app>/
func (a *agent) appDir(app string) string {
	return filepath.Join(a.dataDir, "apps", sanitizeApp(app))
}

func sanitizeApp(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}

// execute 任务分发：终态上报唯一出口（执行体只返回 error）
func (a *agent) execute(t *Task) {
	fmt.Printf("收到任务 %s/%s（%s）\n", t.SessionID, t.NodeID, t.Kind)
	rep := newReporter(a, t)
	defer rep.close()
	rep.post("running", "")
	done := make(chan struct{})
	defer close(done)
	go rep.keepalive(done)
	var err error
	switch t.Kind {
	case "load":
		err = decodeRun(t.Payload, func(p LoadPayload) error { return a.execLoad(p, rep) })
	case "compose":
		err = decodeRun(t.Payload, func(p ComposePayload) error { return a.execCompose(p, rep) })
	case "systemd":
		err = decodeRun(t.Payload, func(p SystemdPayload) error { return a.execSystemd(p, rep) })
	case "cmd":
		err = decodeRun(t.Payload, func(p CmdPayload) error { return a.execCmd(p, rep) })
	case "wait":
		err = decodeRun(t.Payload, func(p WaitPayload) error { return a.execWait(p, rep) })
	case "plugin":
		err = decodeRun(t.Payload, func(p PluginPayload) error { return a.execPlugin(p, rep) })
	case "docker":
		err = decodeRun(t.Payload, func(p DockerPayload) error { return a.execDocker(p, rep) })
	case "toolkit":
		err = decodeRun(t.Payload, func(p ToolkitPayload) error { return a.execToolkit(p, rep) })
	default:
		err = fmt.Errorf("未知任务类型 %q", t.Kind)
	}
	if err != nil {
		rep.post("failed", err.Error())
	} else {
		rep.post("ok", "")
	}
}

func decodeRun[T any](raw json.RawMessage, fn func(T) error) error {
	var p T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("任务载荷解析失败: %w", err)
		}
	}
	return fn(p)
}

// ---- load：拉文件（images/*.tar 自动 docker load，其余落应用目录相对路径） ----

func (a *agent) execLoad(p LoadPayload, rep *reporter) error {
	if p.App == "" || len(p.Files) == 0 {
		return fmt.Errorf("load 任务缺少 app/files")
	}
	base := a.appDir(p.App)
	loaded := 0
	for _, f := range p.Files {
		rel, err := safeRelPath(f.Path)
		if err != nil {
			return err
		}
		dest := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		if err := a.downloadFile(f, dest, rep); err != nil {
			return err
		}
		if strings.HasPrefix(rel, "images/") && strings.HasSuffix(rel, ".tar") {
			if f.Size > 0 {
				rep.logf("docker load 开始：%s（%s，大镜像解压需数分钟）", rel, humanSize(f.Size))
			} else {
				rep.logf("docker load 开始：%s", rel)
			}
			if err := runLogged(rep, "docker", "load", "-i", dest); err != nil {
				return fmt.Errorf("docker load %s 失败: %w", rel, err)
			}
			loaded++
		}
	}
	rep.logf("完成：下载 %d 个文件，装载镜像 %d 个", len(p.Files), loaded)
	return nil
}

// downloadFile 从 console 拉文件到 dest（.part 临时落盘 + sha256 校验 + 原子改名）；
// 已存在且哈希一致则跳过（幂等重试快速路径）。
// URL 契约：/api/file/{sessionId}/{包内相对路径}（console 侧 agentapi.go 以首段定位会话与包目录）
func (a *agent) downloadFile(f FileSpec, dest string, rep *reporter) error {
	if f.SHA256 != "" {
		if got, err := fileSHA256(dest); err == nil && got == f.SHA256 {
			rep.logf("已存在且校验一致，跳过下载: %s", f.Path)
			return nil
		}
	}
	req, _ := http.NewRequest(http.MethodGet, a.server+"/api/file/"+rep.t.SessionID+"/"+f.Path, nil)
	req.Header.Set("X-Agent-Token", a.state.AgentToken)
	resp, err := a.dl.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s 失败: %w", f.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败（%s）", f.Path, resp.Status)
	}
	if f.Size > 0 {
		rep.logf("开始下载 %s（%s）", f.Path, humanSize(f.Size))
	} else {
		rep.logf("开始下载 %s", f.Path)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash, &downloadProgress{rep: rep, path: f.Path, total: f.Size}), resp.Body)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if f.SHA256 != "" && hex.EncodeToString(hash.Sum(nil)) != f.SHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("文件 %s 校验不符（sha256）", f.Path)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	rep.logf("已下载 %s（%s）", f.Path, humanSize(size))
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// downloadProgress 下载进度日志：每 64MB 或 20s 打一行（几百 MB 镜像 tar 下载数分钟，
// 无进度行则控制台节点全程零反馈）。io.Copy 单流顺序调用，无需加锁
type downloadProgress struct {
	rep     *reporter
	path    string
	total   int64
	got     int64
	lastAt  int64
	lastLog int64
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.got += int64(len(b))
	if p.got-p.lastLog >= 64<<20 || time.Since(time.Unix(p.lastAt, 0)) > 20*time.Second {
		if p.got > 0 {
			rate := float64(p.got) / time.Since(time.Unix(p.lastAt, 0)).Seconds()
			if p.total > 0 {
				p.rep.logf("下载 %s %d%%（%s/%s，%s/s）", p.path, p.got*100/p.total, humanSize(p.got), humanSize(p.total), humanSize(int64(rate)))
			} else {
				p.rep.logf("下载 %s（已 %s，%s/s）", p.path, humanSize(p.got), humanSize(int64(rate)))
			}
		}
		p.lastLog, p.lastAt = p.got, time.Now().Unix()
	}
	return len(b), nil
}

// ---- compose：写 per-host 编排文件并 up -d ----

var dataBindRe = regexp.MustCompile(`\./data/([A-Za-z0-9_.-]+)`)

func (a *agent) execCompose(p ComposePayload, rep *reporter) error {
	if p.App == "" || p.Compose == "" {
		return fmt.Errorf("compose 任务缺少 app/compose")
	}
	base := a.appDir(p.App)
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
		r, err := safeRelPath(rel)
		if err != nil {
			return err
		}
		dst := filepath.Join(base, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return err
		}
		rep.logf("写入 %s", r)
	}
	ensureDataDirs(base, p.Compose)
	// 端口前置自检（兜底心跳档案与执行瞬间的时差）：异源占用直接失败带归因，
	// 不进 up 也不回退 v1 白跑；本项目容器占用视为复用（重装/升级幂等）不拦
	if len(p.Ports) > 0 {
		if dets := portConflictDetail(p.Ports, p.Project, probeContainerPorts()); len(dets) > 0 {
			for _, d := range dets {
				rep.logf("端口冲突：%s", d)
			}
			return fmt.Errorf("宿主端口被占用，已跳过编排拉起：%s（调整分配或清理占用后重试）", strings.Join(dets, "；"))
		}
		rep.logf("端口预检通过（%s）", strings.Join(p.Ports, ", "))
	}
	args := []string{"compose", "-f", compFile, "up", "-d", "--remove-orphans"}
	if len(p.Services) > 0 {
		// 就绪分段子集 up（compose 文件含全部服务定义，up 只拉指定服务；已运行服务幂等跳过）
		rep.logf("拉起编排（项目 %s，%d 个服务：%s）……", p.Project, len(p.Services), strings.Join(p.Services, ","))
		args = append(args, p.Services...)
	} else {
		rep.logf("拉起编排（项目 %s）……", p.Project)
	}
	if err := runLogged(rep, "docker", args...); err == nil {
		return nil
	}
	// 失败后即时自检：端口被占（回退 v1 也必然同样失败）则直接报归因，不再无意义重演一次；
	// Ports 为空（旧 console 载荷）时自检恒空，照旧走回退链路
	if dets := portConflictDetail(p.Ports, p.Project, probeContainerPorts()); len(dets) > 0 {
		for _, d := range dets {
			rep.logf("端口冲突：%s", d)
		}
		return fmt.Errorf("docker compose 失败（宿主端口被占用）：%s", strings.Join(dets, "；"))
	}
	rep.logf("docker compose 失败，回退 docker-compose 旧客户端……")
	return runLogged(rep, "docker-compose", args...)
}

// ensureDataDirs 按 compose bind 引用预建 ./data/<名>（外加 conf/logs），免 docker 以 root 自动建目录
func ensureDataDirs(base, composeText string) {
	seen := map[string]bool{"data/conf": true, "data/logs": true}
	for _, m := range dataBindRe.FindAllStringSubmatch(composeText, -1) {
		seen[filepath.Join("data", m[1])] = true
	}
	for dir := range seen {
		_ = os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0755)
	}
}

// ---- systemd：裸机服务（仅 Linux；unit 文件内 {{APP_DIR}} 替换为本机 app 目录） ----

func (a *agent) execSystemd(p SystemdPayload, rep *reporter) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd 任务仅支持 Linux 主机")
	}
	if p.App == "" || p.Unit == "" || p.UnitFile == "" {
		return fmt.Errorf("systemd 任务缺少 app/unit/unitFile")
	}
	base := a.appDir(p.App)
	for rel, content := range p.ConfigFiles {
		r, err := safeRelPath(rel)
		if err != nil {
			return err
		}
		dst := filepath.Join(base, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
			return err
		}
		rep.logf("写入 %s", r)
	}
	if p.EnvFile != "" {
		if err := os.WriteFile(filepath.Join(base, ".env"), []byte(p.EnvFile), 0600); err != nil {
			return err
		}
	}
	unit := strings.ReplaceAll(p.UnitFile, "{{APP_DIR}}", base)
	unitPath := filepath.Join("/etc/systemd/system", filepath.Base(p.Unit))
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("写 unit 文件失败（需要 root）: %w", err)
	}
	rep.logf("已写 %s", unitPath)
	if err := runLogged(rep, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runLogged(rep, "systemctl", "enable", p.Unit); err != nil {
		return err
	}
	return runLogged(rep, "systemctl", "restart", p.Unit)
}

// ---- cmd：一次性 shell ----

func (a *agent) execCmd(p CmdPayload, rep *reporter) error {
	if p.Script == "" {
		return fmt.Errorf("cmd 任务缺少 script")
	}
	cwd := a.dataDir
	if p.App != "" {
		cwd = a.appDir(p.App)
	}
	if p.Cwd != "" { // 终端会话记忆的工作目录（cd 跨命令生效的落点）
		cwd = p.Cwd
	}
	if err := os.MkdirAll(cwd, 0755); err != nil {
		return err
	}
	timeout := time.Duration(orInt(p.TimeoutSec, 600)) * time.Second
	script := p.Script
	if p.TrackCwd { // 终端会话：末尾追加 PWD 哨兵行（console 落库前剥离并记忆，cd 跨命令生效）
		script += cwdSentinelSuffix()
	}
	// 不用 exec.CommandContext：它在超时瞬间只杀主进程，随后 taskkill /T 因根进程已死无法
	// 枚举后代树，孤儿子进程握住管道令收尾永久挂起——超时/中断统一由杀手 goroutine 整组终止
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", script)
	} else {
		cmd = exec.Command("sh", "-c", script)
	}
	setPgroup(cmd) // 独立进程组：中断/超时可整组终止（sh 与其子进程，如 docker logs -f）
	cmd.Dir = cwd
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	procDone := make(chan struct{})
	defer close(procDone)
	killerFired := make(chan string, 1) // 杀手动武原因（cancel|timeout），先发后杀保证 runCmdIO 返回时可见
	go func() {
		reason := ""
		select {
		case <-rep.cancelled():
			reason = "cancel"
		case <-timer.C:
			reason = "timeout"
		case <-procDone:
			return
		}
		killerFired <- reason
		killGroup(cmd)
	}()
	err := runCmdIO(rep, cmd)
	select {
	case <-rep.cancelled():
		return fmt.Errorf("已人工中断")
	default:
	}
	select {
	case <-killerFired:
		return fmt.Errorf("执行超时（上限 %d 秒）", int(timeout.Seconds()))
	default:
	}
	return err
}

// ---- wait：本地健康等待 ----

func (a *agent) execWait(p WaitPayload, rep *reporter) error {
	var probe func() (bool, string)
	switch p.Type {
	case "http":
		if p.URL == "" {
			return fmt.Errorf("wait(http) 缺少 url")
		}
		probe = func() (bool, string) { return httpOK(p.URL) }
	case "docker":
		if p.Project == "" || p.Service == "" {
			return fmt.Errorf("wait(docker) 缺少 project/service")
		}
		probe = func() (bool, string) { return dockerServiceUp(p.Project, p.Service) }
	case "systemd":
		if p.Unit == "" {
			return fmt.Errorf("wait(systemd) 缺少 unit")
		}
		probe = func() (bool, string) { return unitActive(p.Unit) }
	case "tcp":
		if p.Host == "" || p.Port == "" {
			return fmt.Errorf("wait(tcp) 缺少 host/port")
		}
		addr := net.JoinHostPort(p.Host, p.Port)
		probe = func() (bool, string) { return tcpOK(addr) }
	default:
		return fmt.Errorf("wait 任务类型未知: %q", p.Type)
	}
	timeout := time.Duration(orInt(p.TimeoutSec, 600)) * time.Second
	deadline := time.Now().Add(timeout)
	waited := 0
	for {
		ok, detail := probe()
		if ok {
			rep.logf("健康检查通过（%s，等待 %ds）", p.Type, waited)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("健康等待超时（%s，%s）", p.Type, detail)
		}
		if waited%30 == 0 { // 周期心跳行：长等待（最长 600s）期间消息列不静默
			rep.logf("等待中（%s，已 %ds，当前：%s）", p.Type, waited, detail)
		}
		time.Sleep(2 * time.Second)
		waited += 2
	}
}

var probeClient = &http.Client{Timeout: 3 * time.Second}

func httpOK(url string) (bool, string) {
	resp, err := probeClient.Get(url)
	if err != nil {
		return false, "连接失败"
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// tcpOK 端口监听探测：无 HTTP 健康路径的组件（mysql/redis 等）端口开始监听 ≈ 初始化完成
func tcpOK(addr string) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false, "连接失败"
	}
	_ = conn.Close()
	return true, "TCP 可连"
}

// dockerServiceUp compose 容器健康（容器名兼容 v2 连字符 / v1 下划线两种命名）：
// 有 healthcheck 取健康态，否则取运行态
func dockerServiceUp(project, service string) (bool, string) {
	for _, name := range []string{project + "-" + service + "-1", project + "_" + service + "_1"} {
		out, err := exec.Command("docker", "inspect", "--format",
			`{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}`, name).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			return s == "healthy" || s == "running", s
		}
	}
	return false, "容器未找到"
}

func unitActive(unit string) (bool, string) {
	if err := exec.Command("systemctl", "is-active", unit).Run(); err == nil {
		return true, "active"
	}
	return false, "inactive"
}

// ---- plugin：配置中心插件（spawn 子进程 + stdio JSON 协议，ADR-4） ----

// pluginFileName 插件名 + OS → 本地缓存文件名（与 console plugin-bin 命名一致）
func pluginFileName(plugin, goos string) string {
	if goos == "windows" {
		return plugin + "-windows.exe"
	}
	return plugin + "-linux"
}

// pluginBin 确保插件二进制就位：缓存命中（sha256 一致）复用，否则从 console 下载（.part + 校验 + 原子改名）
func (a *agent) pluginBin(p PluginPayload, rep *reporter) (string, error) {
	if p.Plugin == "" {
		return "", fmt.Errorf("plugin 任务缺少 plugin 名")
	}
	if p.OS != "" && p.OS != runtime.GOOS {
		return "", fmt.Errorf("插件任务目标 OS=%s 与本机 %s 不符", p.OS, runtime.GOOS)
	}
	dir := filepath.Join(a.dataDir, "plugins")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	file := pluginFileName(p.Plugin, runtime.GOOS)
	dst := filepath.Join(dir, file)
	if p.SHA256 != "" {
		if got, err := fileSHA256(dst); err == nil && got == p.SHA256 {
			rep.logf("插件 %s 已缓存且校验一致，复用", file)
			return dst, nil
		}
	}
	// console 下载（agent 令牌鉴权；占位文件端点侧拒绝）
	req, _ := http.NewRequest(http.MethodGet, a.server+"/api/plugin/"+p.Plugin+"/"+runtime.GOOS, nil)
	req.Header.Set("X-Agent-Token", a.state.AgentToken)
	resp, err := a.dl.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载插件 %s 失败: %w", file, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载插件 %s 失败（%s）", file, resp.Status)
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hash), resp.Body)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if p.SHA256 != "" && got != p.SHA256 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("插件 %s 校验不符（sha256）", file)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dst, 0755)
	}
	rep.logf("插件 %s 已就位（%s）", file, got[:12])
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

func (a *agent) execPlugin(p PluginPayload, rep *reporter) error {
	if p.Op == "" {
		return fmt.Errorf("plugin 任务缺少 op")
	}
	bin, err := a.pluginBin(p, rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(orInt(p.TimeoutSec, 300))*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	stderr, err3 := cmd.StderrPipe()
	if err1 != nil || err2 != nil || err3 != nil {
		return fmt.Errorf("插件进程管道创建失败")
	}
	rep.logf("$ %s %s（%s）", filepath.Base(bin), p.Op, p.Plugin)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动插件失败: %w", err)
	}
	if _, err := stdin.Write(append(pluginRequest(p), '\n')); err != nil {
		return err
	}
	_ = stdin.Close()
	go rep.scan(stderr) // 插件进度（stderr）逐行入上报缓冲
	line, rerr := bufio.NewReader(stdout).ReadString('\n')
	werr := cmd.Wait()
	if rerr != nil {
		return fmt.Errorf("读取插件响应失败: %v（wait: %v）", rerr, werr)
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
		return fmt.Errorf("插件执行失败: %s", resp.Error)
	}
	if len(resp.Data) > 0 {
		rep.logf("结果: %s", string(resp.Data))
	}
	return nil
}

// ---- 命令执行与日志 ----

// runLogged 执行命令并把 stdout/stderr 逐行送入上报器（先回显 $ 命令行）
func runLogged(rep *reporter, name string, args ...string) error {
	rep.logf("$ %s %s", name, strings.Join(args, " "))
	return runCmdIO(rep, exec.Command(name, args...))
}

func runCmdIO(rep *reporter, cmd *exec.Cmd) error {
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err := errorsJoin(err1, err2); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); rep.scan(stdout) }()
	go func() { defer wg.Done(); rep.scan(stderr) }()
	wg.Wait()
	return cmd.Wait()
}

func errorsJoin(errs ...error) error {
	var msg []string
	for _, e := range errs {
		if e != nil {
			msg = append(msg, e.Error())
		}
	}
	if len(msg) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msg, "; "))
}

// reporter 节点日志与状态上报：行缓冲批量推送（满 40 行或距上次推送 >3s——长时间静默的任务
// （大文件下载/docker load）也有心跳感），终态可靠送达（重试 5 次）。
// 上报响应同时是控制通道：console 对终端会话的中断指令经 {"cancel":true} 下发（旧 console
// 不回该字段、旧 agent 不解析，双向兼容）。
type reporter struct {
	a          *agent
	t          *Task
	mu         sync.Mutex
	lines      []string
	sent       int
	file       *os.File
	lastPush   time.Time
	cancelCh   chan struct{}
	cancelOnce sync.Once
}

func newReporter(a *agent, t *Task) *reporter {
	r := &reporter{a: a, t: t, cancelCh: make(chan struct{})}
	_ = os.MkdirAll(filepath.Join(a.dataDir, "logs"), 0755)
	f, err := os.OpenFile(filepath.Join(a.dataDir, "logs", t.SessionID+"-"+t.NodeID+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		r.file = f
	}
	return r
}

// requestCancel console 下达中断（幂等；close-once channel）
func (r *reporter) requestCancel() {
	r.cancelOnce.Do(func() {
		close(r.cancelCh)
		fmt.Fprintf(os.Stderr, "[task] %s/%s 收到中断指令\n", r.t.SessionID, r.t.NodeID)
	})
}

// cancelled 中断信号（cmd 等执行体监听后杀进程组；interrupted 报文由执行体生成）
func (r *reporter) cancelled() <-chan struct{} { return r.cancelCh }

// close 任务终态后释放本地日志句柄（防长跑 agent 逐任务泄漏）
func (r *reporter) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}

// keepalive 执行中周期推送（2s）：静默任务（docker logs -f 无新行）也持续上报空 running——
// ①保持中断控制通道可达（cancel 随 report 响应回传）②看门狗活性心跳。done 在任务终态时关闭。
func (r *reporter) keepalive(done <-chan struct{}) {
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			r.mu.Lock()
			if r.sent < len(r.lines) || time.Since(r.lastPush) >= 2*time.Second {
				r.postLocked("running", "")
			}
			r.mu.Unlock()
		}
	}
}

func (r *reporter) logf(format string, args ...any) {
	r.append(fmt.Sprintf(format, args...))
}

func (r *reporter) append(lines ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, lines...)
	if r.file != nil {
		for _, l := range lines {
			_, _ = r.file.WriteString(time.Now().Format("15:04:05 ") + l + "\n")
		}
	}
	// 时间维度触发：静默任务的零星日志（如下载进度行）也及时上屏；首轮（lastPush 零值）立即推一次
	if r.sent < len(r.lines) && (len(r.lines)-r.sent >= 40 || time.Since(r.lastPush) > 3*time.Second) {
		r.postLocked("running", "")
	}
}

// scan 命令输出逐行入缓冲（bufio.Scanner 挂到 reporter）
func (r *reporter) scan(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		r.append(sc.Text())
	}
}

// post 状态上报（含未发日志）；终态（ok/failed）重试确保送达
func (r *reporter) post(status, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.postLocked(status, msg)
}

// postLocked 须持 r.mu；增量日志随 running 状态捎带。响应解析 cancel 控制字段（持锁安全——
// requestCancel 只用 once 原语不再取锁）
func (r *reporter) postLocked(status, msg string) {
	logs := append([]string(nil), r.lines[r.sent:]...)
	r.sent = len(r.lines)
	rep := Report{SessionID: r.t.SessionID, NodeID: r.t.NodeID, Status: status, Message: msg, Logs: logs}
	attempts := 1
	if status == "ok" || status == "failed" {
		attempts = 5
	}
	var ack struct {
		Cancel bool `json:"cancel"`
	}
	for i := 0; i < attempts; i++ {
		if err := r.a.postJSON("/api/agent/report", rep, &ack); err == nil {
			if ack.Cancel {
				r.requestCancel()
			}
			r.lastPush = time.Now()
			if r.file != nil {
				_, _ = r.file.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), status))
			}
			return
		} else if i < attempts-1 {
			time.Sleep(2 * time.Second)
		}
	}
	fmt.Fprintf(os.Stderr, "上报终态失败（%s %s/%s）——console 侧将由看门狗超时兜底\n", status, r.t.SessionID, r.t.NodeID)
}

// ---- 工具 ----

// safeRelPath 任务内文件相对路径防逃逸（拒绝绝对路径/../.. 与 Windows 盘符）
func safeRelPath(p string) (string, error) {
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == "" {
		return "", fmt.Errorf("空路径")
	}
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") ||
		filepath.IsAbs(filepath.FromSlash(p)) {
		return "", fmt.Errorf("非法路径 %q", p)
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("非法路径 %q", p)
	}
	return p, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// cwdSentinelSuffix 终端会话 PWD 哨兵行（追加在脚本末尾执行；console 收到后剥离子落库并记忆
// 会话工作目录——每条命令独立进程，cd 的跨命令记忆全靠它回传）
func cwdSentinelSuffix() string {
	if runtime.GOOS == "windows" {
		return "\nWrite-Output \"$([char]1)CWD:$($PWD.ProviderPath)$([char]2)\""
	}
	return "\nprintf '\001CWD:%s\002\n' \"$PWD\""
}
