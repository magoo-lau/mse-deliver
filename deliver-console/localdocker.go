// 本机节点 docker / toolkit 任务执行（与 deliver-agent dockertask.go、toolkittask.go 同构的进程内
// 实现）：离线介质从 console-data/media/ 直读（不走 HTTP）；装完刷新本机能力档案（hosts local 行）。
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func (s *Server) localDocker(p DockerPayload, report func(string, string, []string)) error {
	logf := func(format string, args ...any) { report("running", "", []string{fmt.Sprintf(format, args...)}) }
	cur := ""
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		cur = strings.TrimSpace(string(out))
	}
	if cur != "" {
		if p.Version == "" || verEqual(cur, p.Version) {
			if !p.Force {
				logf("Docker 已就绪（%s），跳过安装", cur)
				return nil
			}
		} else {
			logf("Docker %s → %s：离线介质重装换版", cur, p.Version)
		}
	}
	reinstall := cur != ""
	if runtime.GOOS != "linux" {
		return fmt.Errorf("自动安装 Docker 仅支持 Linux 主机（Windows 请预装 Docker Desktop 并确保守护进程运行）")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 Docker 需要 root（console 进程须以 root 运行）")
	}
	switch {
	case p.MediaSHA256 != "":
		media := s.mediaFilePath(mediaToolDocker, runtime.GOOS+"-"+runtime.GOARCH, orStr(p.Version, mediaLegacyVer))
		if got, err := fileSHA256(media); err != nil || got != p.MediaSHA256 {
			return fmt.Errorf("本机离线介质缺失或校验不符（主机池页重新上传 %s-%s）", runtime.GOOS, runtime.GOARCH)
		}
		logf("解包离线介质 %s 到 /usr/local/bin ……", filepath.Base(media))
		if err := localExtractDockerMedia(media, "/usr/local/bin", "/etc/systemd/system/docker.service", logf); err != nil {
			return err
		}
		if err := localEnsureComposePlugin("/usr/local/bin", logf); err != nil {
			return err
		}
	case p.ScriptURL != "":
		if p.Version != "" {
			return fmt.Errorf("指定版本 %s 安装需要 console 上传对应离线介质（在线脚本只能装最新版）", p.Version)
		}
		script := fmt.Sprintf("command -v curl >/dev/null 2>&1 && curl -fsSL %s | sh || wget -qO- %s | sh", p.ScriptURL, p.ScriptURL)
		logf("在线安装（%s）——本机需可访问外网，包管理器安装需数分钟……", p.ScriptURL)
		ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
		defer cancel()
		if err := localRunIO(report, exec.CommandContext(ctx, "sh", "-c", script)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("docker 任务缺少安装源（离线介质与在线脚本均为空）")
	}
	if len(p.Mirrors) > 0 {
		if _, err := os.Stat("/etc/docker/daemon.json"); err == nil {
			logf("已存在 daemon.json，跳过镜像加速配置")
		} else {
			_ = os.MkdirAll("/etc/docker", 0755)
			data, _ := json.Marshal(map[string]any{"registry-mirrors": p.Mirrors})
			if err := os.WriteFile("/etc/docker/daemon.json", data, 0644); err != nil {
				return err
			}
			logf("已写 /etc/docker/daemon.json（registry-mirrors %d 项）", len(p.Mirrors))
		}
	}
	if err := localRun(report, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// 新装 enable --now；重装（守护进程原本在跑）restart 换代二进制
	if reinstall {
		if err := localRun(report, "systemctl", "restart", "docker"); err != nil {
			return err
		}
	} else {
		if err := localRun(report, "systemctl", "enable", "--now", "docker"); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
		if err == nil {
			logf("Docker 就绪（server %s）", strings.TrimSpace(string(out)))
			s.refreshLocalHostInfo()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("docker 守护进程 120s 内未就绪（journalctl -u docker 查看原因）")
		}
		time.Sleep(2 * time.Second)
	}
}

// localToolkit 本机 toolkit 任务（v1 仅 compose：插件安装/升级到指定版本；介质从 console-data
// 直读，在线兜底 curl/wget 直下——与 agent toolkittask.go 同构）
func (s *Server) localToolkit(p ToolkitPayload, report func(string, string, []string)) error {
	logf := func(format string, args ...any) { report("running", "", []string{fmt.Sprintf(format, args...)}) }
	if p.Tool != "compose" {
		return fmt.Errorf("本机节点不支持工具 %q（v1 仅 compose）", p.Tool)
	}
	if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
		cur := strings.TrimSpace(string(out))
		if !p.Force && (p.MediaVer == "" || verEqual(cur, p.MediaVer)) {
			logf("docker compose 已就绪（%s），跳过安装", cur)
			return nil
		}
		if p.MediaVer != "" && !verEqual(cur, p.MediaVer) {
			logf("docker compose %s → %s：换版安装", cur, p.MediaVer)
		}
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("docker/docker-compose 自动安装仅支持 Linux（Windows 请预装 Docker Desktop 并确保守护进程运行）")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 docker compose 插件需要 root（console 进程须以 root 运行）")
	}
	if p.MediaSHA256 == "" && p.OnlineURL == "" {
		return fmt.Errorf("compose 任务缺少安装源（离线介质与在线直链均为空）")
	}
	dstDir := "/usr/local/lib/docker/cli-plugins"
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, "docker-compose")
	tmp := dst + ".part"
	switch {
	case p.MediaSHA256 != "":
		if p.MediaVer == "" {
			return fmt.Errorf("compose 介质未标版本（主机池页重新上传带版本号的介质）")
		}
		src := s.mediaFilePath(mediaToolCompose, runtime.GOOS+"-"+runtime.GOARCH, p.MediaVer)
		if got, err := fileSHA256(src); err != nil || got != p.MediaSHA256 {
			return fmt.Errorf("本机 compose 介质缺失或校验不符（主机池页重新上传 %s-%s）", runtime.GOOS, runtime.GOARCH)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(tmp, data, 0755); err != nil {
			return err
		}
		logf("介质已就位（%s，%s）", p.MediaVer, humanSizeLocal(int64(len(data))))
	default: // 在线直链（目标机 curl/wget 直下，不经 console 转发）
		script := fmt.Sprintf("command -v curl >/dev/null 2>&1 && curl -fL -o '%s' '%s' || wget -qO '%s' '%s'", tmp, p.OnlineURL, tmp, p.OnlineURL)
		logf("在线下载 docker compose %s（%s）……", orStr(p.MediaVer, ""), p.OnlineURL)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := localRunIO(report, exec.CommandContext(ctx, "sh", "-c", script)); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if p.OnlineSHA256 != "" {
			if got, err := fileSHA256(tmp); err != nil || got != p.OnlineSHA256 {
				_ = os.Remove(tmp)
				return fmt.Errorf("在线直链校验不符（期望 sha256 %s）", p.OnlineSHA256[:12])
			}
		}
		_ = os.Chmod(tmp, 0755)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	logf("compose 插件已就位 %s", dst)
	out, err := exec.Command("docker", "compose", "version", "--short").Output()
	if err != nil {
		return fmt.Errorf("docker compose 安装后校验失败（手动执行 docker compose version 排查）")
	}
	logf("docker compose 就绪（%s）", strings.TrimSpace(string(out)))
	s.refreshLocalHostInfo()
	return nil
}

// refreshLocalHostInfo 重探本机能力并回写 hosts local 行（装完 docker 即时上屏）
func (s *Server) refreshLocalHostInfo() {
	list, err := s.store.ListHosts()
	if err != nil {
		return
	}
	info := probeLocalHostInfo()
	for _, h := range list {
		if h.Local {
			_ = s.store.TouchHost(h.ID, &info)
		}
	}
}

func localEnsureComposePlugin(binDir string, logf func(string, ...any)) error {
	src := filepath.Join(binDir, "docker-compose")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dstDir := "/usr/local/lib/docker/cli-plugins"
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dstDir, "docker-compose"), data, 0755); err != nil {
		return err
	}
	logf("compose 插件已就位（docker compose 子命令可用）")
	return nil
}

// localExtractDockerMedia 与 agent extractDockerMedia 同构（布局/normalize 规则见 agent 侧注释）
func localExtractDockerMedia(tarPath, binDir, unitPath string, logf func(string, ...any)) error {
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
			continue
		}
		p := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if strings.HasPrefix(p, "docker/") {
			rest := strings.TrimPrefix(p, "docker/")
			if !strings.Contains(rest, "/") || strings.HasPrefix(rest, "bin/") || strings.HasPrefix(rest, "systemd/") {
				p = rest
			}
		}
		p = strings.Trim(p, "/")
		if p == "" || strings.Contains(p, "..") || filepath.IsAbs(filepath.FromSlash(p)) {
			continue
		}
		switch {
		case strings.HasPrefix(p, "bin/") && !strings.Contains(strings.TrimPrefix(p, "bin/"), "/"):
			dst := filepath.Join(binDir, filepath.Base(p))
			if err := writeLocalMediaFile(tr, dst, 0755); err != nil {
				return err
			}
			logf("已落盘 %s", dst)
		case strings.HasSuffix(p, "docker.service"):
			_ = os.MkdirAll(filepath.Dir(unitPath), 0755)
			if err := writeLocalMediaFile(tr, unitPath, 0644); err != nil {
				return err
			}
			unitWritten = true
			logf("已落盘 %s", unitPath)
		case !strings.Contains(p, "/") && hdr.Mode&0111 != 0:
			dst := filepath.Join(binDir, p)
			if err := writeLocalMediaFile(tr, dst, 0755); err != nil {
				return err
			}
			logf("已落盘 %s", dst)
		}
	}
	if !unitWritten {
		if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(unitPath, []byte(localDockerUnit), 0644); err != nil {
			return err
		}
		logf("介质未随带 unit，已写内置模板 %s", unitPath)
	}
	return nil
}

const localDockerUnit = `[Unit]
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

func writeLocalMediaFile(r io.Reader, dst string, mode os.FileMode) error {
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
