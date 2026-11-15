package main

// toolkit 任务执行器：基础工具维护（v1 仅 compose——docker compose 插件安装/升级到指定版本，
// 仅 Linux、需 root）。双通道：console 版本化介质优先（X-Agent-Token 下载 + sha256 校验缓存
// 复用）/ 在线直链兜底（console 按目标版本渲染好的完整 URL，目标机 curl/wget 直下——不经
// console 转发，令牌绝不出境）。落位 /usr/local/lib/docker/cli-plugins/docker-compose（与
// docker 介质附带的 ensureComposePlugin 同址同形态——`docker compose` 子命令）；
// 幂等：已装且（无目标版本或==目标）直接 ok；装后验证 + 重注册刷新能力档案
//（Compose/ComposeVer 上行，主机池即时翻绿）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func (a *agent) execToolkit(p ToolkitPayload, rep *reporter) error {
	if p.Tool != "compose" {
		return fmt.Errorf("未知工具 %q（v1 仅 compose）", p.Tool)
	}
	if ok, cur := probeCompose(); ok {
		if !p.Force && (p.MediaVer == "" || verEqual(cur, p.MediaVer)) { // 幂等快路径
			rep.logf("docker compose 已就绪（%s），跳过安装", cur)
			return nil
		}
		if p.MediaVer != "" && !verEqual(cur, p.MediaVer) {
			rep.logf("docker compose %s → %s：换版安装", cur, p.MediaVer)
		}
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("docker/docker-compose 自动安装仅支持 Linux（Windows 请预装 Docker Desktop——桌面版自带 compose 插件）")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 docker compose 插件需要 root（请以 root 运行 deliver-agent 服务）")
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
			return fmt.Errorf("compose 介质未标版本（console 重新上传带版本号的介质）")
		}
		cache := filepath.Join(a.dataDir, "toolkit-media", "compose-"+p.MediaVer+".bin")
		if err := a.downloadConsoleMedia("compose", p.MediaVer, p.MediaSHA256, p.MediaSize, cache, rep); err != nil {
			return err
		}
		data, err := os.ReadFile(cache)
		if err != nil {
			return err
		}
		if err := os.WriteFile(tmp, data, 0755); err != nil {
			return err
		}
	default: // 在线直链兜底（目标机 curl/wget 直下；可选 sha 校验）
		rep.logf("在线下载 docker compose %s（%s）……", p.MediaVer, p.OnlineURL)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(orInt(p.TimeoutSec, 600)-60)*time.Second)
		defer cancel()
		if err := runCmdIO(rep, commandContextSh(ctx, composeOnlineScript(tmp, p.OnlineURL))); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if p.OnlineSHA256 != "" {
			if got := fileSHA256OrEmpty(tmp); got != p.OnlineSHA256 {
				_ = os.Remove(tmp)
				return fmt.Errorf("在线直链校验不符（期望 sha256 %s）", p.OnlineSHA256[:12])
			}
		}
		_ = os.Chmod(tmp, 0755)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	rep.logf("compose 插件已就位 %s", dst)
	ok, ver := probeCompose()
	if !ok {
		return fmt.Errorf("docker compose 安装后校验失败（手动执行 docker compose version 排查）")
	}
	rep.logf("docker compose 就绪（%s）", ver)
	// 刷新能力档案（Compose=true/ComposeVer 上行——主机池即时显示已装）
	if err := a.ensureRegistered(); err != nil {
		rep.logf("能力档案刷新失败（不影响安装结果）: %v", err)
	}
	return nil
}

// commandContextSh sh -c 脚本命令（在线下载通道；exec 抽象便于单测构造断言）
func commandContextSh(ctx context.Context, script string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", script)
}

// composeOnlineScript 在线直链下载脚本（curl 优先退化 wget；路径/URL 单引号包裹防 shell 切词）
func composeOnlineScript(dst, url string) string {
	return fmt.Sprintf("command -v curl >/dev/null 2>&1 && curl -fL -o '%s' '%s' || wget -qO '%s' '%s'", dst, url, dst, url)
}

// fileSHA256OrEmpty 读文件 sha256（失败返回空串——调用方与期望值不等即判失败）
func fileSHA256OrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
