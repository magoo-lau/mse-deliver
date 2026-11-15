package main

// agent 自升级：poll 响应携带升级指令（console 判定版本落后时下发），任务间隙自替换二进制并重启。
//
// 替换舞步（applyUpgrade，纯函数可单测）：新二进制先落 <exe>.new → 旧件改名 <exe>.old 留底 →
// 新件改名就位（Windows 运行中 exe 可改名不可覆盖，两步改名跨平台一致；Linux 原子等价）。
// 重启策略（restartSelf）：Linux systemd 托管直接退出（Restart=always ≤5s 拉起新件）；其余
// spawn 分离新进程后自退——手工运行模式直接拉 run；Windows 计划任务模式拉 selfrestart 子命令
// （老进程退出后作为孤儿进程存活，执行 schtasks /End + /Run 换代）。新进程启动时清理 .old 留底。
// 守卫：平台须与本机一致、只升不降（yyyyMMddHHmm 定宽字典序）、sha256 校验不过不替换；
// 失败由 pollLoop 记录并 10 分钟退避（console 手动 pending 在版本翻绿前持续重发自愈）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UpgradeDirective 与 console 侧 protocol.go 镜像同构（协议字段即契约）
type UpgradeDirective struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	SHA256  string `json:"sha256"`
}

const upgradeBackoff = 10 * time.Minute

// selfUpgrade 处理升级指令：成功路径以进程退出收尾（不返回）；失败返回错误由 pollLoop 退避
func (a *agent) selfUpgrade(d UpgradeDirective) error {
	if d.OS != runtime.GOOS {
		return fmt.Errorf("升级指令平台 %s 与本机 %s 不符，跳过", d.OS, runtime.GOOS)
	}
	if d.Version == agentVersion {
		return nil // 幂等：已达标（console 未及观察到新版本上报时）
	}
	if d.Version < agentVersion {
		return fmt.Errorf("升级指令版本 %s 不高于本机 %s，跳过（只升不降）", d.Version, agentVersion)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ := filepath.Abs(exe)
	data, err := a.downloadAgentBin(d)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[upgrade] %s → %s：新二进制已下载（%s），替换自身……\n",
		agentVersion, d.Version, humanSize(int64(len(data))))
	if err := applyUpgrade(exePath, data); err != nil {
		return fmt.Errorf("自替换失败: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[upgrade] 替换完成，重启生效（%s）\n", restartDesc())
	if err := restartSelf(exePath, a.dataDir); err != nil {
		// 二进制已换代但重启编排失败：保持旧进程继续服务（console 会再次下发，下次启动即新件）
		return fmt.Errorf("重启编排失败（二进制已换代，下次进程重启生效）: %w", err)
	}
	os.Exit(0)
	return nil
}

// downloadAgentBin 从 console 拉取本平台新二进制并做 sha256 校验
func (a *agent) downloadAgentBin(d UpgradeDirective) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, a.server+"/api/agent/binary/"+runtime.GOOS, nil)
	req.Header.Set("X-Agent-Token", a.state.AgentToken)
	resp, err := a.dl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载新二进制失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载新二进制失败（%s）", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if d.SHA256 != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != d.SHA256 {
			return nil, fmt.Errorf("新二进制校验不符（sha256），已放弃替换")
		}
	}
	return data, nil
}

// applyUpgrade 二进制替换舞步：<exe>.part → <exe>.new →（旧件挪 .old 留底）→ 新件就位。
// 就位失败回滚旧件；调用方保证 data 已校验
func applyUpgrade(exePath string, data []byte) error {
	newPath := exePath + ".new"
	tmp := newPath + ".part"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return err
	}
	if err := os.Rename(tmp, newPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(exePath, exePath+".old"); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, exePath); err != nil {
		_ = os.Rename(exePath+".old", exePath) // 就位失败回滚旧件
		_ = os.Remove(newPath)
		return err
	}
	return nil
}

// removeStaleBinary 新进程启动时清理上次自升级留底（Windows 上旧件若仍被占用会失败——忽略）
func removeStaleBinary() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// restartDesc 重启方式描述（升级日志可读性）
func restartDesc() string {
	switch {
	case runtime.GOOS == "linux" && underSystemd():
		return "systemd Restart=always 拉起"
	case runtime.GOOS == "windows" && taskInstalled():
		return "计划任务 selfrestart 换代"
	default:
		return "分离新进程接替"
	}
}

// restartSelf 安排自重启（替换已完成）：Linux systemd 托管直接返回（调用方 os.Exit(0)，由
// Restart=always 拉起新件）；其余 spawn 分离新进程——手工运行模式拉 run，Windows 计划任务
// 模式拉 selfrestart（孤儿进程执行 schtasks /End + /Run）
func restartSelf(exePath, dataDir string) error {
	if runtime.GOOS == "linux" && underSystemd() {
		return nil
	}
	if runtime.GOOS == "windows" && taskInstalled() {
		return spawnDetached(exePath, "selfrestart", "-data-dir", dataDir)
	}
	return spawnDetached(exePath, "run", "-data-dir", dataDir)
}

// underSystemd 本进程是否由 systemd 托管（cgroup 内含本服务 unit；手工 nohup 启动则否）
func underSystemd() bool {
	data, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(data), serviceName+".service")
}

// taskInstalled Windows 计划任务是否已安装（schtasks 不可用/未装 → false）
func taskInstalled() bool {
	return exec.Command("schtasks", "/Query", "/TN", serviceName).Run() == nil
}
