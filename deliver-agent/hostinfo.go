package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// HostInfo 注册上报的能力档案——console 侧用于分配校验（docker 服务只能落 docker 主机、
// systemd 单元需 Linux+systemd、fat-jar 单元需 java）与主机池展示
type HostInfo struct {
	Hostname     string   `json:"hostname"`
	IPs          []string `json:"ips"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Docker       bool     `json:"docker"`
	DockerVer    string   `json:"dockerVersion,omitempty"`
	Compose      bool     `json:"compose"` // docker compose 插件可用（CLI 侧探测，无需 daemon）
	ComposeVer   string   `json:"composeVersion,omitempty"`
	Systemd      bool     `json:"systemd"`
	JavaVer      string   `json:"javaVersion,omitempty"`
	CPUCore      int            `json:"cpuCores"`
	MemMB        int64          `json:"memMB"`
	DiskFreeGB   int64          `json:"diskFreeGB"`
	AgentVersion string         `json:"agentVersion"`
	Containers   []ContainerPort `json:"containers,omitempty"` // 运行容器宿主端口映射（安装预检档案；旧 console 忽略未知字段）
}

func gatherHostInfo(dataDir string) HostInfo {
	h := HostInfo{Hostname: hostname(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUCore: runtime.NumCPU(), AgentVersion: agentVersion, IPs: localIPs()}
	h.Docker, h.DockerVer = probeDocker()
	h.Compose, h.ComposeVer = probeCompose()
	h.Systemd = probeSystemd()
	h.JavaVer = probeJava()
	h.MemMB = probeMemMB()
	h.DiskFreeGB = probeDiskFreeGB(dataDir)
	h.Containers = probeContainerPorts()
	return h
}

// probeDocker 探测 docker 可用性（要求守护进程在跑：docker version 需连上 Server）
func probeDocker() (bool, string) {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return false, ""
	}
	return true, strings.TrimSpace(string(out))
}

// probeCompose 探测 docker compose 插件（`docker compose version` 为 CLI 侧子命令，无需守护
// 进程；docker CLI 缺失或无插件均 false）
func probeCompose() (bool, string) {
	out, err := exec.Command("docker", "compose", "version", "--short").Output()
	if err != nil {
		return false, ""
	}
	return true, strings.TrimSpace(string(out))
}

// probeSystemd 仅 Linux 判定（/run/systemd/system 目录存在即 systemd 为 init）
func probeSystemd() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}

var javaVerRe = regexp.MustCompile(`version "([^"]+)"`)

func probeJava() string {
	out, err := exec.Command("java", "-version").CombinedOutput()
	if err != nil {
		return ""
	}
	if m := javaVerRe.FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return ""
}

func probeMemMB() int64 {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
					if fields := strings.Fields(rest); len(fields) >= 1 {
						kb, _ := strconv.ParseInt(fields[0], 10, 64)
						return kb / 1024
					}
				}
			}
		}
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"[math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory/1MB)").Output()
		if err == nil {
			mb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			return mb
		}
	}
	return 0
}

// probeDiskFreeGB 数据目录所在卷剩余空间（尽力而为：Linux/macOS=df，Windows=Get-PSDrive）
func probeDiskFreeGB(dir string) int64 {
	if runtime.GOOS == "windows" {
		if len(dir) < 2 || dir[1] != ':' {
			return 0
		}
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("[math]::Round((Get-PSDrive %s).Free/1GB)", strings.ToUpper(dir[:1]))).Output()
		if err == nil {
			gb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			return gb
		}
		return 0
	}
	out, err := exec.Command("df", "-Pk", dir).Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) >= 4 {
		kb, _ := strconv.ParseInt(fields[3], 10, 64)
		return kb / 1024 / 1024
	}
	return 0
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	// 取短主机名（去域名后缀），console 主机池展示与分配引用更友好
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
