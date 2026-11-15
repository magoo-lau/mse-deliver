package main

// docker 任务执行器：安装/维护 Docker（双通道——console 版本化离线介质优先 / 在线脚本兜底；
// 仅 Linux、需 root）。Version=目标版本：空=装上即可（已装即跳过）；非空=精确安装/升级到该版本
//（仅介质可定版，在线脚本只能装最新）。幂等可重试：已装且（无目标或==目标）直接 ok（不重装、
// 不动 daemon.json）；换版重装走介质覆盖 + systemctl restart。装完统一 daemon.json 镜像加速
//（仅新装且文件不存在时写）→ systemd 启用 → 就绪轮询 → 重注册刷新能力档案。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// verEqual 版本等价判定（归一：去空白与前导 v/V——docker server 报 27.5.1、compose --short 报
// 2.30.3、介质版本标签常带 v 前缀；空串不等于任何值）
func verEqual(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
			s = s[1:]
		}
		return s
	}
	return norm(a) != "" && norm(a) == norm(b)
}

func (a *agent) execDocker(p DockerPayload, rep *reporter) error {
	up, cur := probeDocker()
	if up {
		if (p.Version == "" || verEqual(cur, p.Version)) && !p.Force { // 幂等快路径
			rep.logf("Docker 已就绪（%s），跳过安装", cur)
			return nil
		}
		if p.Version != "" && !verEqual(cur, p.Version) {
			rep.logf("Docker %s → %s：离线介质重装换版", cur, p.Version)
		}
	}
	reinstall := up // 守护进程原本在跑=重装（enable --now 不会重启换新二进制）
	if runtime.GOOS != "linux" {
		return fmt.Errorf("自动安装 Docker 仅支持 Linux 主机（Windows 请预装 Docker Desktop 并确保守护进程运行）")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 Docker 需要 root（请以 root 运行 deliver-agent 服务）")
	}
	switch {
	case p.MediaSHA256 != "":
		if err := a.installDockerOffline(p, rep); err != nil {
			return err
		}
	case p.ScriptURL != "":
		if p.Version != "" {
			return fmt.Errorf("指定版本 %s 安装需要 console 上传对应离线介质（在线脚本只能装最新版）", p.Version)
		}
		if err := a.installDockerOnline(p, rep); err != nil {
			return err
		}
	default:
		return fmt.Errorf("docker 任务缺少安装源（离线介质与在线脚本均为空）")
	}
	if err := writeDaemonMirrors(p.Mirrors, rep); err != nil {
		return err
	}
	if err := runLogged(rep, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if reinstall {
		if err := runLogged(rep, "systemctl", "restart", "docker"); err != nil {
			return err
		}
	} else if err := runLogged(rep, "systemctl", "enable", "--now", "docker"); err != nil {
		return err
	}
	ver, err := waitDockerReady(rep, 120)
	if err != nil {
		return err
	}
	rep.logf("Docker 就绪（server %s）", ver)
	// 刷新能力档案（Docker=true/DockerVer 上行——主机池即时显示已装）
	if err := a.ensureRegistered(); err != nil {
		rep.logf("能力档案刷新失败（不影响安装结果）: %v", err)
	}
	return nil
}

// downloadConsoleMedia 从 console 版本化介质库下载到 dst（X-Agent-Token；sha256 校验；
// 已缓存且校验一致复用；.part 原子改名；进度上报）。docker tar.gz / compose 二进制共用。
func (a *agent) downloadConsoleMedia(tool, version, sha string, size int64, dst string, rep *reporter) error {
	if got, err := fileSHA256(dst); err == nil && got == sha {
		rep.logf("%s 介质已缓存且校验一致，复用（%s）", tool, filepath.Base(dst))
		return nil
	}
	if size > 0 {
		rep.logf("开始下载 %s 介质 %s（%s）", tool, orStr(version, "最新"), humanSize(size))
	} else {
		rep.logf("开始下载 %s 介质 %s", tool, orStr(version, "最新"))
	}
	u := a.server + "/api/agent/media/" + tool + "/" + runtime.GOOS + "/" + runtime.GOARCH
	if version != "" {
		u += "?v=" + url.QueryEscape(version)
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("X-Agent-Token", a.state.AgentToken)
	resp, err := a.dl.Do(req)
	if err != nil {
		return fmt.Errorf("下载 %s 介质失败: %w", tool, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		return fmt.Errorf("下载 %s 介质失败（%s）: %s", tool, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		resp.Body.Close()
		return err
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		resp.Body.Close()
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hash, &downloadProgress{rep: rep, path: tool + "-media", total: size}), resp.Body)
	resp.Body.Close()
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sha {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s 介质校验不符（sha256 %s）", tool, got[:12])
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	rep.logf("%s 介质已下载（%s）", tool, humanSize(size))
	return nil
}

// installDockerOffline 拉离线介质并解包落盘（缓存按 sha 命名复用——不同版本各自成文件）
func (a *agent) installDockerOffline(p DockerPayload, rep *reporter) error {
	dir := filepath.Join(a.dataDir, "docker-media")
	dst := filepath.Join(dir, runtime.GOOS+"-"+runtime.GOARCH+"-"+p.MediaSHA256[:12]+".tar.gz")
	if err := a.downloadConsoleMedia("docker", p.Version, p.MediaSHA256, p.MediaSize, dst, rep); err != nil {
		return err
	}
	rep.logf("解包介质到 /usr/local/bin ……")
	if err := extractDockerMedia(dst, "/usr/local/bin", "/etc/systemd/system/docker.service", rep); err != nil {
		return err
	}
	return ensureComposePlugin("/usr/local/bin", rep)
}

// installDockerOnline 目标机在线直装（curl 脚本，缺 curl 退化 wget；apt/yum 源安装由脚本承担）
func (a *agent) installDockerOnline(p DockerPayload, rep *reporter) error {
	script := fmt.Sprintf("command -v curl >/dev/null 2>&1 && curl -fsSL %s | sh || wget -qO- %s | sh", p.ScriptURL, p.ScriptURL)
	rep.logf("在线安装（%s）——目标机需可访问外网，包管理器安装需数分钟……", p.ScriptURL)
	timeout := time.Duration(orInt(p.TimeoutSec, 1800)-120) * time.Second // 留就绪轮询余量
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runCmdIO(rep, exec.CommandContext(ctx, "sh", "-c", script))
}

// writeDaemonMirrors 新装主机的镜像加速（/etc/docker/daemon.json 不存在才写——绝不动已有配置）
func writeDaemonMirrors(mirrors []string, rep *reporter) error {
	if len(mirrors) == 0 {
		return nil
	}
	path := "/etc/docker/daemon.json"
	if _, err := os.Stat(path); err == nil {
		rep.logf("已存在 daemon.json，跳过镜像加速配置")
		return nil
	}
	if err := os.MkdirAll("/etc/docker", 0755); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"registry-mirrors": mirrors})
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	rep.logf("已写 %s（registry-mirrors %d 项）", path, len(mirrors))
	return nil
}

// waitDockerReady 守护进程就绪轮询（首次启动初始化数秒）
func waitDockerReady(rep *reporter, timeoutSec int) (string, error) {
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	waited := 0
	for {
		if ok, ver := probeDocker(); ok {
			return ver, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("docker 守护进程 %ds 内未就绪（journalctl -u docker 查看原因）", timeoutSec)
		}
		if waited%16 == 0 {
			rep.logf("等待 docker 守护进程就绪（已 %ds）……", waited)
		}
		time.Sleep(2 * time.Second)
		waited += 2
	}
}

// ensureComposePlugin compose 插件就位：介质内含 docker-compose 时同步到 docker CLI 插件目录
// （/usr/local/lib/docker/cli-plugins——`docker compose` 子命令依赖此路径，缺则只剩回退 docker-compose 旧客户端）
func ensureComposePlugin(binDir string, rep *reporter) error {
	src := filepath.Join(binDir, "docker-compose")
	if _, err := os.Stat(src); err != nil {
		return nil // 介质未随带 compose：compose 任务有 docker-compose 回退链路
	}
	dstDir := "/usr/local/lib/docker/cli-plugins"
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, "docker-compose")
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0755); err != nil {
		return err
	}
	rep.logf("compose 插件已就位（docker compose 子命令可用）")
	return nil
}

// dockerUnitFallback 静态安装缺省 unit（介质未随带 systemd/docker.service 时使用）
const dockerUnitFallback = `[Unit]
Description=Docker Application Container Engine
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd
ExecReload=/bin/kill -s HUP $MAINPID
Restart=always
RestartSec=5
LimitNOFILE=infinity
LimitNPROC=infinity

[Install]
WantedBy=multi-user.target
`

// extractDockerMedia 解包介质：bin/* 与根级可执行文件（官方 static 包顶层扁平布局）→ binDir
// （0755）；systemd/docker.service（或根级 docker.service）→ unitPath；其余跳过。
// 布局与 normalizeMediaEntry 见函数注释；目标目录可参数化（单测用临时目录）。
func extractDockerMedia(tarPath, binDir, unitPath string, rep *reporter) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("介质不是有效的 tar.gz: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	unitWritten := false
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("介质解包失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // 目录/链接跳过（二进制按最终文件落盘）
		}
		name := normalizeMediaEntry(hdr.Name)
		if name == "" {
			continue
		}
		switch {
		case strings.HasPrefix(name, "bin/") && !strings.Contains(strings.TrimPrefix(name, "bin/"), "/"):
			if err := writeMediaFile(tr, filepath.Join(binDir, filepath.Base(name)), 0755); err != nil {
				return err
			}
			rep.logf("已落盘 %s", filepath.Join(binDir, filepath.Base(name)))
		case strings.HasSuffix(name, "docker.service"):
			if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
				return err
			}
			if err := writeMediaFile(tr, unitPath, 0644); err != nil {
				return err
			}
			unitWritten = true
			rep.logf("已落盘 %s", unitPath)
		case !strings.Contains(name, "/") && hdr.Mode&0111 != 0: // 官方 static 包：顶层扁平可执行文件
			if err := writeMediaFile(tr, filepath.Join(binDir, name), 0755); err != nil {
				return err
			}
			rep.logf("已落盘 %s", filepath.Join(binDir, name))
		default:
			rep.logf("跳过介质条目 %s", hdr.Name)
		}
	}
	if !unitWritten {
		if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(unitPath, []byte(dockerUnitFallback), 0644); err != nil {
			return err
		}
		rep.logf("介质未随带 unit，已写内置模板 %s", unitPath)
	}
	return nil
}

// normalizeMediaEntry 剥掉 "./" 前缀与官方 static 包顶层 "docker/" 目录（docker/docker → docker、
// docker/bin/docker → bin/docker），得到 bin/、systemd/ 或根级形态；逃逸路径拒绝
func normalizeMediaEntry(name string) string {
	p := strings.TrimPrefix(filepath.ToSlash(name), "./")
	if strings.HasPrefix(p, "docker/") {
		rest := strings.TrimPrefix(p, "docker/")
		if !strings.Contains(rest, "/") || strings.HasPrefix(rest, "bin/") || strings.HasPrefix(rest, "systemd/") {
			p = rest
		}
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") || filepath.IsAbs(filepath.FromSlash(p)) {
		return ""
	}
	return p
}

func writeMediaFile(r io.Reader, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
