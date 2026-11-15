package main

// 宿主端口占用探测与归因——安装前置预检的数据源。归因口径（三层预检共享）：
//   本项目 compose 容器占用 = 复用（重装/升级幂等语义，不算冲突）；
//   异源容器/非容器进程占用 = 冲突（报归因，由用户调整分配或清理占用）。
// 数据链路：心跳随能力档案上报 console（分配阶段展示）→ 建会话校验 → execCompose 执行前自检兜底。

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// ContainerPort 运行中容器的宿主端口映射（console 侧 protocol.go 同构镜像）
type ContainerPort struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	HostPorts []string `json:"hostPorts,omitempty"`
	Project   string   `json:"project,omitempty"` // com.docker.compose.project 标签（本项目复用判定）
}

// probeContainerPorts docker ps 归因清单（含未发布端口的容器——展示用；HostPorts 空）；
// docker 不可用返回 nil（调用方按能力档案 Docker 布尔区分「无 docker」与「无占用」）
func probeContainerPorts() []ContainerPort {
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
		c := ContainerPort{Name: f[0], Image: f[1], HostPorts: parseHostPorts(f[2])}
		if len(f) >= 4 {
			c.Project = f[3]
		}
		cs = append(cs, c)
	}
	return cs
}

// parseHostPorts 解析 docker ps Ports 字段的宿主端口段（0.0.0.0:3306->3306/tcp、:::6379->…），
// 取绑定地址末段端口；同端口的 IPv4/IPv6 双行去重
func parseHostPorts(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seg := range strings.Split(s, ",") {
		seg = strings.TrimSpace(seg)
		head, _, ok := strings.Cut(seg, "->")
		if !ok {
			continue // 无发布映射段不算宿主端口
		}
		i := strings.LastIndex(head, ":")
		if i < 0 || i+1 >= len(head) {
			continue
		}
		port := head[i+1:]
		if port == "" || !allDigits(port) || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	sort.Strings(out)
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// portConflictDetail 端口冲突明细（空=无冲突）。归因优先 docker 容器（覆盖绝大多数占用）；
// 容器归因不到时试绑判忙，忙则 ss/netstat 尽力归因进程（拿不到报未知，如实暴露占用事实）
func portConflictDetail(ports []string, project string, cs []ContainerPort) []string {
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
				continue // 本项目容器：重装/升级复用（compose up 幂等）
			}
			conflicts = append(conflicts, fmt.Sprintf("端口 %s 已被容器 %s（镜像 %s）占用", p, c.Name, c.Image))
			continue
		}
		if !hostPortBusy(p) {
			continue // 空闲
		}
		conflicts = append(conflicts, fmt.Sprintf("端口 %s 已被非容器进程占用（%s）", p, barePortHolder(p)))
	}
	return conflicts
}

// hostPortBusy 试绑判忙（与 docker 发布同口径绑全地址；瞬时占用立即释放）
func hostPortBusy(port string) bool {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// barePortHolder 非容器占用者归因（尽力而为）：Linux ss -tlnpH 进程名，Windows netstat PID
func barePortHolder(port string) string {
	if runtime.GOOS == "linux" {
		out, err := exec.Command("ss", "-tlnpH").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				// LISTEN 0 151 0.0.0.0:3306 0.0.0.0:* users:(("mysqld",pid=1234,fd=20))
				if !strings.Contains(line, ":"+port+" ") {
					continue
				}
				if i := strings.Index(line, "users:"); i >= 0 {
					return "进程 " + strings.TrimSpace(line[i:])
				}
			}
		}
	} else if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				// TCP    0.0.0.0:3306    0.0.0.0:0    LISTENING    1234
				f := strings.Fields(line)
				if len(f) >= 5 && strings.HasSuffix(f[1], ":"+port) && strings.EqualFold(f[3], "LISTENING") {
					return "PID " + f[4] + "（tasklist 可查进程名）"
				}
			}
		}
	}
	return "未知进程，可经 ss/netstat 查看占用者"
}
