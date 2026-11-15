package main

// 自装系统服务：Linux=systemd unit（deliver-agent.service），Windows=开机计划任务
// （schtasks ONSTART——不依赖 Windows 服务协议，纯标准库即可）。install 先注册落 agent.json，
// 服务启动时凭其中 agent 令牌免引导令牌。

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func installService(server, enroll, dataDir string) error {
	a := &agent{server: strings.TrimRight(server, "/"), enroll: enroll, dataDir: dataDir,
		state: loadState(dataDir), api: &http.Client{Timeout: 35 * time.Second}, dl: &http.Client{}}
	if a.server == "" {
		a.server = a.state.Server
	}
	if a.server == "" {
		return fmt.Errorf("缺少 -server（console 地址）")
	}
	if err := a.ensureRegistered(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ := filepath.Abs(exe)
	if runtime.GOOS == "linux" {
		return installSystemdUnit(exePath, dataDir)
	}
	return installWindowsTask(exePath, dataDir)
}

func installSystemdUnit(exePath, dataDir string) error {
	unit := fmt.Sprintf(`[Unit]
Description=deliver-agent 安装节点代理（mse-deliver）
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run -data-dir %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, exePath, dataDir)
	unitPath := filepath.Join("/etc/systemd/system", serviceName+".service")
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("写 systemd unit 失败（需要 root）: %w", err)
	}
	// restart 无条件执行：全新装=start；升级装（unit 指向新二进制但服务在跑）必须 restart 换新进程——
	// enable --now 对已 active 服务是空操作，旧进程会一直跑到下次重启（升级不生效的坑）
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", serviceName + ".service"}, {"restart", serviceName + ".service"}} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %v 失败: %v: %s", args, err, out)
		}
	}
	warnStrayAgentProcesses()
	fmt.Println("已安装并启动系统服务:", serviceName+".service")
	return nil
}

// warnStrayAgentProcesses 升级装残留进程告警：手工（nohup 等）启动的旧 agent 不受 systemd
// restart 影响，会与新服务并行抢任务——按 cgroup 区分（本服务 unit 内的 pid 是 restart 拉起的
// 合法新实例，跳过；unit 外的 deliver-agent 进程即手工残留，提示手工 kill）。非致命
func warnStrayAgentProcesses() {
	out, err := exec.Command("pgrep", "-f", serviceName).Output()
	if err != nil {
		return // pgrep 不可用或无匹配——静默
	}
	self := os.Getpid()
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == self {
			continue
		}
		if cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil &&
			strings.Contains(string(cg), serviceName+".service") {
			continue // systemd 托管实例（restart 已换代，合法）
		}
		fmt.Printf("警告：agent 进程 pid %d 不在 %s.service 托管内（多为手工启动的旧实例），"+
			"systemctl restart 不会终止它，请手工 kill 防止双跑抢任务\n", pid, serviceName)
	}
}

func installWindowsTask(exePath, dataDir string) error {
	tr := fmt.Sprintf(`"%s" run -data-dir "%s"`, exePath, dataDir)
	if out, err := exec.Command("schtasks", "/Create", "/TN", serviceName,
		"/SC", "ONSTART", "/RU", "SYSTEM", "/TR", tr, "/F").CombinedOutput(); err != nil {
		return fmt.Errorf("创建计划任务失败: %v: %s", err, out)
	}
	_ = exec.Command("schtasks", "/End", "/TN", serviceName).Run() // 升级装：先停旧进程（未在跑则忽略）
	if out, err := exec.Command("schtasks", "/Run", "/TN", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("启动计划任务失败: %v: %s", err, out)
	}
	fmt.Println("已安装并启动开机计划任务:", serviceName)
	return nil
}

func uninstallService(dataDir string) error {
	if runtime.GOOS == "linux" {
		unitPath := filepath.Join("/etc/systemd/system", serviceName+".service")
		if _, err := os.Stat(unitPath); err == nil {
			_ = exec.Command("systemctl", "disable", "--now", serviceName+".service").Run()
			if err := os.Remove(unitPath); err != nil {
				return fmt.Errorf("删除 unit 失败（需要 root）: %w", err)
			}
			_ = exec.Command("systemctl", "daemon-reload").Run()
		}
		fmt.Println("已卸载系统服务（数据目录保留:", dataDir, "）")
		return nil
	}
	if out, err := exec.Command("schtasks", "/Delete", "/TN", serviceName, "/F").CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "无法找到") && !strings.Contains(string(out), "does not exist") {
			return fmt.Errorf("删除计划任务失败: %v: %s", err, out)
		}
	}
	fmt.Println("已卸载计划任务（数据目录保留:", dataDir, "）")
	return nil
}

func serviceInstalledHint() string {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat(filepath.Join("/etc/systemd/system", serviceName+".service")); err == nil {
			return "已安装（systemd）"
		}
		return "未安装（可执行 install 子命令）"
	}
	if err := exec.Command("schtasks", "/Query", "/TN", serviceName).Run(); err == nil {
		return "已安装（计划任务）"
	}
	return "未安装（可执行 install 子命令）"
}
