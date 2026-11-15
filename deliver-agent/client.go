package main

// console ↔ agent 协议（与 deliver-console 侧 agentapi.go 镜像同构，双端独立维护——工具族惯例，
// 两工具各自可整体外迁，协议字段即契约）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Task console 下发的节点任务
type Task struct {
	SessionID string          `json:"sessionId"`
	NodeID    string          `json:"nodeId"`
	Kind      string          `json:"kind"` // load|compose|systemd|cmd|wait|plugin|docker|toolkit
	Payload   json.RawMessage `json:"payload"`
}

// FileSpec 待拉取文件（path 相对部署包根：images/*.tar、artifacts/*.jar、delivery/…）
type FileSpec struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

type LoadPayload struct {
	App   string     `json:"app"`
	Files []FileSpec `json:"files"`
}

type ComposePayload struct {
	App      string            `json:"app"`
	Project  string            `json:"project"`            // compose 项目名（console 按主机分组生成）
	Compose  string            `json:"compose"`            // docker-compose.yml 全文（per-host 子集）
	Env      string            `json:"env,omitempty"`      // 根 .env 全文（凭据/变量，0600）
	Files    map[string]string `json:"files,omitempty"`    // rel path → content（conf/application.yaml 等）
	Services []string          `json:"services,omitempty"` // up 子集（同机就绪分层；空=全部）
	Ports    []string          `json:"ports,omitempty"`    // 本子集宿主发布端口（执行前自检；旧 agent 忽略）
}

type SystemdPayload struct {
	App         string            `json:"app"`
	Unit        string            `json:"unit"`                  // 如 mse-app.service
	UnitFile    string            `json:"unitFile"`              // unit 全文，可含 {{APP_DIR}} 占位
	ConfigFiles map[string]string `json:"configFiles,omitempty"` // rel path → content
	EnvFile     string            `json:"envFile,omitempty"`     // EnvironmentFile 内容（app 目录 .env）
	WorkingDir  string            `json:"workingDir,omitempty"`  // 工作目录（相对 app 目录，缺省 app 目录）
}

type CmdPayload struct {
	App        string `json:"app"` // 缺省=数据目录（非应用级命令）
	Script     string `json:"script"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	Cwd        string `json:"cwd,omitempty"`      // 工作目录（终端会话记忆 cd；空=数据目录/app 目录）
	TrackCwd   bool   `json:"trackCwd,omitempty"` // 终端会话：执行时追加 PWD 哨兵行回传（旧 agent 忽略）
}

type WaitPayload struct {
	Type       string `json:"type"` // http|tcp|docker|systemd
	URL        string `json:"url,omitempty"`
	Host       string `json:"host,omitempty"`    // tcp：探测地址
	Port       string `json:"port,omitempty"`    // tcp：探测端口
	Project    string `json:"project,omitempty"` // docker：compose 项目名
	Service    string `json:"service,omitempty"` // docker：compose 服务名
	Unit       string `json:"unit,omitempty"`    // systemd：unit 名
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// DockerPayload 安装/维护 Docker 任务（双通道：离线介质优先——console /api/agent/media/docker/{os}/{arch}
// 下载（?v= 定版），sha256 校验缓存复用；无介质时在线脚本直装——在线脚本只能装最新版）。仅 Linux；
// Version=目标版本（空=装上即可、已装即跳过；非空=精确安装/升级到该版本，仅离线介质可定版）。
type DockerPayload struct {
	Version     string   `json:"version,omitempty"`
	Force       bool     `json:"force,omitempty"`
	ScriptURL   string   `json:"scriptUrl,omitempty"`
	MediaName   string   `json:"mediaName,omitempty"`
	MediaSHA256 string   `json:"mediaSha256,omitempty"`
	MediaSize   int64    `json:"mediaSize,omitempty"`
	Mirrors     []string `json:"mirrors,omitempty"`
	TimeoutSec  int      `json:"timeoutSec,omitempty"`
}

// ToolkitPayload 基础工具维护任务（v1 仅 compose：docker compose 插件安装/升级，仅 Linux）。
// 介质优先（console /api/agent/media/compose/{os}/{arch}?v= 下载）；在线兜底=console 按目标版本
// 渲染好的完整直链（目标机 curl/wget 直下，不经 console 转发）。
type ToolkitPayload struct {
	App          string `json:"app"`
	Tool         string `json:"tool"` // compose
	MediaName    string `json:"mediaName,omitempty"`
	MediaSHA256  string `json:"mediaSha256,omitempty"`
	MediaSize    int64  `json:"mediaSize,omitempty"`
	MediaVer     string `json:"mediaVer,omitempty"`
	OnlineURL    string `json:"onlineUrl,omitempty"`
	OnlineSHA256 string `json:"onlineSha256,omitempty"`
	Force        bool   `json:"force,omitempty"`
	TimeoutSec   int    `json:"timeoutSec,omitempty"`
}

// PluginPayload 配置中心插件任务（ADR-4）：插件二进制从 console /api/plugin/{name}/{os} 下载
// （sha256 校验，缓存复用），spawn 子进程 stdin 一行 JSON 请求 → stdout 一行 JSON 响应。
type PluginPayload struct {
	App        string            `json:"app"`
	Plugin     string            `json:"plugin"`           // 如 nacos-config
	OS         string            `json:"os"`               // linux|windows（本机应匹配）
	SHA256     string            `json:"sha256,omitempty"` // 插件二进制校验（缓存复用判据）
	Op         string            `json:"op"`               // plan|apply|health
	Mode       string            `json:"mode,omitempty"`   // apply：idempotent（缺省）|force
	Config     map[string]string `json:"config,omitempty"` // serverAddr/namespace/identityKey/identityValue
	Items      []PluginItem      `json:"items,omitempty"`
	TimeoutSec int               `json:"timeoutSec,omitempty"`
}

type PluginItem struct {
	DataID  string `json:"dataId"`
	Group   string `json:"group,omitempty"`
	Content string `json:"content"`
}

// Report 任务状态上报（running 携带增量日志、ok/failed 终态）
type Report struct {
	SessionID string   `json:"sessionId"`
	NodeID    string   `json:"nodeId"`
	Status    string   `json:"status"`
	Message   string   `json:"message,omitempty"`
	Logs      []string `json:"logs,omitempty"`
}

type RegisterReq struct {
	EnrollToken string   `json:"enrollToken,omitempty"` // 新注册必填（console 引导令牌）
	AgentToken  string   `json:"agentToken,omitempty"`  // 已注册刷新能力档案
	Host        HostInfo `json:"host"`
}

type RegisterResp struct {
	AgentToken string `json:"agentToken"`
	HostID     int64  `json:"hostId"`
}

type PollResp struct {
	Task    *Task             `json:"task"`
	Upgrade *UpgradeDirective `json:"upgrade,omitempty"` // 自升级指令（与任务平行：先干任务，任务间隙自替换重启）
}

type Heartbeat struct {
	MemMB      int64           `json:"memMB"`
	DiskFreeGB int64           `json:"diskFreeGB"`
	Containers []ContainerPort `json:"containers,omitempty"` // 运行容器端口映射（安装预检档案；旧 console 忽略未知字段）
}

// ---- agent 运行体 ----

type agent struct {
	server  string
	enroll  string
	dataDir string
	state   *agentState
	api     *http.Client // 普通调用（poll/register/report，超时 > 25s hold）
	dl      *http.Client // 文件下载（大镜像 tar，不设整体超时）
}

func runAgent(server, enroll, dataDir string) error {
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
	removeStaleBinary() // 清理上次自升级留底的旧二进制（无则忽略）
	fmt.Printf("deliver-agent %s 已就绪：console=%s 主机=%s 数据目录=%s\n",
		agentVersion, a.server, a.state.Hostname, a.dataDir)
	go a.heartbeatLoop()
	a.pollLoop()
	return nil
}

// ensureRegistered 未注册凭引导令牌注册；已注册（同 console）凭 agent 令牌刷新能力档案
func (a *agent) ensureRegistered() error {
	req := RegisterReq{Host: gatherHostInfo(a.dataDir)}
	if a.state.AgentToken != "" && a.state.Server == a.server {
		req.AgentToken = a.state.AgentToken
	} else {
		if a.enroll == "" {
			return fmt.Errorf("本机未注册且未提供引导令牌（-token）")
		}
		req.EnrollToken = a.enroll
	}
	var resp RegisterResp
	if err := a.postJSON("/api/agent/register", req, &resp); err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}
	a.state.Server, a.state.AgentToken = a.server, resp.AgentToken
	a.state.HostID, a.state.Hostname = resp.HostID, req.Host.Hostname
	return saveState(a.dataDir, a.state)
}

type apiErr struct{ code int }

func (e *apiErr) Error() string { return fmt.Sprintf("console 响应 %d", e.code) }

func (a *agent) postJSON(path string, body, out any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, a.server+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if a.state != nil && a.state.AgentToken != "" {
		req.Header.Set("X-Agent-Token", a.state.AgentToken)
	}
	return doJSON(a.api, req, out)
}

func (a *agent) getJSON(path string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, a.server+path, nil)
	req.Header.Set("X-Agent-Token", a.state.AgentToken)
	return doJSON(a.api, req, out)
}

func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiErr{code: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// poll 长轮询领任务（console 侧 hold 最多 25s，无任务返回 task=null；响应可并行携带升级指令）
func (a *agent) poll() (*Task, *UpgradeDirective, error) {
	var resp PollResp
	if err := a.getJSON("/api/agent/poll?wait=25", &resp); err != nil {
		return nil, nil, err
	}
	return resp.Task, resp.Upgrade, nil
}

func (a *agent) pollLoop() {
	backoff := time.Second
	var upgradeRetryAt time.Time // 自升级失败退避截止（磁盘满等故障防热循环）
	for {
		task, upgrade, err := a.poll()
		if err != nil {
			if ae, ok := err.(*apiErr); ok && ae.code == http.StatusUnauthorized {
				fmt.Fprintln(os.Stderr, "agent 令牌失效或被吊销，尝试凭引导令牌重新注册……")
				a.state.AgentToken = ""
				if a.enroll != "" {
					if err := a.ensureRegistered(); err == nil {
						backoff = time.Second
						continue
					} else {
						fmt.Fprintln(os.Stderr, "重新注册失败:", err)
					}
				}
			}
			fmt.Fprintf(os.Stderr, "轮询失败: %v（%s 后重试）\n", err, backoff)
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if task != nil {
			a.execute(task) // 部署优先：同轮先干任务，升级指令在其完成后处理
			a.sendHeartbeat() // 任务后即时刷新能力快照（容器端口档案）——不再等 120s 周期心跳
		}
		if upgrade != nil && time.Now().After(upgradeRetryAt) {
			if err := a.selfUpgrade(*upgrade); err != nil {
				fmt.Fprintf(os.Stderr, "[upgrade] 自升级失败: %v（%s 后重试）\n", err, upgradeBackoff)
				upgradeRetryAt = time.Now().Add(upgradeBackoff)
			}
		}
	}
}

// heartbeatLoop 周期心跳（在线状态与资源余量上报；轮询本身也证明在线，此处补资源数字）
func (a *agent) heartbeatLoop() {
	for {
		time.Sleep(120 * time.Second)
		a.sendHeartbeat()
	}
}

// sendHeartbeat 即时上报一次能力快照——任务（安装/远程执行）完成后由 pollLoop 调用：容器端口
// 档案立即刷新（否则最长等 120s 周期心跳，「清理占用容器→重试安装」链路会被陈旧档案再拦一次）
func (a *agent) sendHeartbeat() {
	info := gatherHostInfo(a.dataDir)
	_ = a.postJSON("/api/agent/heartbeat", Heartbeat{MemMB: info.MemMB, DiskFreeGB: info.DiskFreeGB, Containers: info.Containers}, nil)
}
