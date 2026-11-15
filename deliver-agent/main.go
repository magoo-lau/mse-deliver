// deliver-agent：安装节点代理（mse-deliver 交付工具族，纯标准库零第三方依赖，双平台单文件）。
//
// 与 deliver-console（安装控制面）配套：控制面从不主动登录节点，
// 本代理出站长轮询领任务——目标机只需一次性拷入本二进制并启动，之后全部通信由本代理发起。
//
// 用法（<data-dir> 缺省 Linux=/opt/mse/deliver，Windows=C:\ProgramData\mse\deliver）：
//
//	deliver-agent run     -server http://console:7870 -token <引导令牌> [-data-dir ...]   前台运行（缺省子命令）
//	deliver-agent install -server http://console:7870 -token <引导令牌> [-data-dir ...]   注册并自装系统服务
//	deliver-agent uninstall [-data-dir ...]    卸载系统服务
//	deliver-agent status  [-data-dir ...]      查看本机注册状态
//
// 令牌模型（与 console 双令牌校验）：引导令牌仅用于首次注册；console 签发 agent 令牌落盘
// <data-dir>/agent.json，重启免重注；此后 poll/report/文件下载全部凭 agent 令牌鉴权（console 可按主机吊销）。
//
// 任务七类（console 编排 DAG 下发，全部幂等可重试）：
//
//	load    拉取本主机所需文件（images/*.tar 自动 docker load；artifacts/、delivery/ 等落应用目录）
//	compose 写 per-host 编排文件（docker-compose.yml/.env/配置）并 docker compose up -d
//	systemd 裸机服务：写 unit（{{APP_DIR}} 占位替换）+ 配置 + systemd restart（仅 Linux）
//	cmd     一次性 shell（如 SQL 预置一次性容器、自定义命令）
//	wait    本地健康等待（http | docker 容器态 | systemd active | tcp 端口）
//	plugin  配置中心插件（stdio JSON 子进程协议）
//	docker  安装 Docker（双通道：console 离线介质优先 / 在线脚本兜底；仅 Linux、需 root；装完刷新能力档案）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// agentVersion 构建时注入（-ldflags -X，值为构建时间戳 yyyyMMddHHmm）——主机池页按此识别 agent 新旧
var agentVersion = "1.0.0"

// 服务名（systemd unit / Windows 计划任务名共用）
const serviceName = "deliver-agent"

func main() {
	sub := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("deliver-agent", flag.ExitOnError)
	server := fs.String("server", "", "console 地址（如 http://10.0.0.5:7870）")
	token := fs.String("token", "", "引导令牌（首次注册必填；agent.json 已有令牌时可省略）")
	dataDir := fs.String("data-dir", defaultDataDir(), "本机数据目录（应用部署与令牌落盘）")
	_ = fs.Parse(args)

	switch sub {
	case "run":
		if err := runAgent(*server, *token, *dataDir); err != nil {
			die(err)
		}
	case "install":
		if err := installService(*server, *token, *dataDir); err != nil {
			die(err)
		}
	case "uninstall":
		if err := uninstallService(*dataDir); err != nil {
			die(err)
		}
	case "status":
		printStatus(*dataDir)
	case "selfrestart":
		// 自升级换代 helper（内部子命令，由旧进程 spawn 的孤儿进程执行）：等旧进程退出后
		// 重启计划任务（Windows 服务模式专用；schtasks /End 对未运行任务是无害空操作）
		time.Sleep(1500 * time.Millisecond)
		if out, err := exec.Command("schtasks", "/End", "/TN", serviceName).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] schtasks /End: %v: %s（旧实例或已退出，忽略）\n", err, out)
		}
		if out, err := exec.Command("schtasks", "/Run", "/TN", serviceName).CombinedOutput(); err != nil {
			die(fmt.Errorf("重启计划任务失败: %v: %s", err, out))
		}
		fmt.Println("[upgrade] 计划任务已重启，新 agent 上线")
	default:
		die(fmt.Errorf("未知子命令 %q（可用：run/install/uninstall/status）", sub))
	}
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\mse\deliver`
	}
	return "/opt/mse/deliver"
}

// ---- 本机注册状态（<data-dir>/agent.json，0600） ----

type agentState struct {
	Server     string `json:"server"`               // console 地址
	AgentToken string `json:"agentToken,omitempty"` // console 签发的节点令牌
	HostID     int64  `json:"hostId,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
}

func statePath(dataDir string) string { return filepath.Join(dataDir, "agent.json") }

func loadState(dataDir string) *agentState {
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return &agentState{}
	}
	var st agentState
	_ = json.Unmarshal(data, &st)
	return &st
}

func saveState(dataDir string, st *agentState) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(statePath(dataDir), data, 0600)
}

func printStatus(dataDir string) {
	st := loadState(dataDir)
	fmt.Printf("数据目录: %s\n", dataDir)
	if st.AgentToken == "" {
		fmt.Println("注册状态: 未注册（run/install 需 -server 与 -token 引导令牌）")
		return
	}
	fmt.Printf("注册状态: 已注册（console=%s hostId=%d hostname=%s）\n", st.Server, st.HostID, st.Hostname)
	fmt.Println("服务安装: " + serviceInstalledHint())
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "deliver-agent:", err)
	os.Exit(1)
}
