// console ↔ agent 协议（与 deliver-agent/client.go 镜像同构，双端独立维护——协议字段即契约）
// 与 format:3 部署包声明（deliver-studio 产出、本工具唯一消费格式）。
package main

import "encoding/json"

// ---- agent 协议 ----

type Task struct {
	SessionID string          `json:"sessionId"`
	NodeID    string          `json:"nodeId"`
	Kind      string          `json:"kind"` // load|compose|systemd|cmd|wait|plugin|docker|toolkit
	Payload   json.RawMessage `json:"payload"`
}

type FileSpec struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Dest   string `json:"dest,omitempty"` // 覆盖落盘位置（相对 app 目录；缺省=path）
}

type LoadPayload struct {
	App   string     `json:"app"`
	Files []FileSpec `json:"files"`
}

type ComposePayload struct {
	App      string            `json:"app"`
	Project  string            `json:"project"`
	Compose  string            `json:"compose"`
	Env      string            `json:"env,omitempty"`
	Files    map[string]string `json:"files,omitempty"`
	Services []string          `json:"services,omitempty"` // up 子集（同机就绪分层 base/rest 段；空=全部）
	Ports    []string          `json:"ports,omitempty"`    // 本子集宿主发布端口（agent 执行前端口自检；旧 agent 忽略）
}

type SystemdPayload struct {
	App         string            `json:"app"`
	Unit        string            `json:"unit"`
	UnitFile    string            `json:"unitFile"` // 可含 {{APP_DIR}} 占位
	ConfigFiles map[string]string `json:"configFiles,omitempty"`
	EnvFile     string            `json:"envFile,omitempty"`
	WorkingDir  string            `json:"workingDir,omitempty"`
}

type CmdPayload struct {
	App        string `json:"app"`
	Script     string `json:"script"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	Cwd        string `json:"cwd,omitempty"`      // 工作目录（终端会话记忆 cd；空=app 目录/数据目录）
	TrackCwd   bool   `json:"trackCwd,omitempty"` // 终端会话：执行时追加 PWD 哨兵行回传（旧 agent 忽略）
}

type WaitPayload struct {
	Type       string `json:"type"` // http|tcp|docker|systemd
	URL        string `json:"url,omitempty"`
	Host       string `json:"host,omitempty"` // tcp：探测地址
	Port       string `json:"port,omitempty"` // tcp：探测端口
	Project    string `json:"project,omitempty"`
	Service    string `json:"service,omitempty"`
	Unit       string `json:"unit,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// DockerPayload 安装/维护 Docker 任务（双通道：离线介质优先——console /api/agent/media/docker/{os}/{arch}
// 分发（?v= 定版），sha256 校验缓存复用；无介质时目标机在线脚本直装——在线脚本只能装最新版）。
// 仅 Linux；Version 指定目标版本（空=装上即可、已装即跳过；非空=精确安装/升级到该版本，仅离线介质
// 可定版），已装版本==目标即幂等跳过；装完统一写 daemon.json（registry-mirrors，仅新装且文件不存在时）
// + systemd 启用 + 就绪轮询 + 重注册刷新能力档案。
type DockerPayload struct {
	Version     string   `json:"version,omitempty"`     // 目标版本（空=旧语义；非空须有对应离线介质）
	Force       bool     `json:"force,omitempty"`       // 已装==目标仍强制重装
	ScriptURL   string   `json:"scriptUrl,omitempty"`   // 在线安装脚本（curl|sh；空=仅离线）
	MediaName   string   `json:"mediaName,omitempty"`   // 离线介质原始文件名（展示用；下载按 os/arch 走 media 端点）
	MediaSHA256 string   `json:"mediaSha256,omitempty"` // 介质校验（agent 缓存复用判据）
	MediaSize   int64    `json:"mediaSize,omitempty"`
	Mirrors     []string `json:"mirrors,omitempty"` // daemon.json registry-mirrors（不动已有配置）
	TimeoutSec  int      `json:"timeoutSec,omitempty"`
}

// ToolkitPayload 基础工具维护任务（v1 仅 compose：docker compose 插件安装/升级到指定版本，仅 Linux；
// 旧 agent 不识别 toolkit kind 会报「未知任务类型」——warnDockerAgentVersion 护栏前置提示）。
// 介质优先（console /api/agent/media/compose/{os}/{arch}?v= 下载，sha256 校验缓存复用）；
// 在线兜底为 console 按所选版本渲染好的完整直链（目标机 curl/wget 直下，不经 console 转发）。
type ToolkitPayload struct {
	App          string `json:"app"`
	Tool         string `json:"tool"` // compose
	MediaName    string `json:"mediaName,omitempty"`
	MediaSHA256  string `json:"mediaSha256,omitempty"`
	MediaSize    int64  `json:"mediaSize,omitempty"`
	MediaVer     string `json:"mediaVer,omitempty"` // 目标版本（幂等判据）
	OnlineURL    string `json:"onlineUrl,omitempty"`
	OnlineSHA256 string `json:"onlineSha256,omitempty"` // 可选校验
	Force        bool   `json:"force,omitempty"`
	TimeoutSec   int    `json:"timeoutSec,omitempty"`
}

// PluginPayload 配置中心插件任务（ADR-4）：插件二进制内嵌 console、按执行主机 OS 下载
// （/api/plugin/{name}/{os}，SHA256 校验缓存复用）；spawn 子进程 stdin 一行 JSON 请求、
// stdout 一行 JSON 响应（协议见 docs/tech/deliver-design.md）。
type PluginPayload struct {
	App        string            `json:"app"`
	Plugin     string            `json:"plugin"`           // 插件名（如 nacos-config）
	OS         string            `json:"os"`               // 执行主机 OS：linux|windows
	SHA256     string            `json:"sha256,omitempty"` // 插件二进制校验（agent 缓存复用判据）
	Op         string            `json:"op"`               // plan|apply|health
	Mode       string            `json:"mode,omitempty"`   // apply：idempotent（缺省）|force
	Config     map[string]string `json:"config,omitempty"` // serverAddr/namespace/group/identityKey/identityValue
	Items      []PluginItem      `json:"items,omitempty"`  // plan/apply 基线清单（内容已物化嵌入）
	TimeoutSec int               `json:"timeoutSec,omitempty"`
}

type PluginItem struct {
	DataID  string `json:"dataId"`
	Group   string `json:"group,omitempty"`
	Content string `json:"content"`
}

type Report struct {
	SessionID string   `json:"sessionId"`
	NodeID    string   `json:"nodeId"`
	Status    string   `json:"status"` // running|ok|failed
	Message   string   `json:"message,omitempty"`
	Logs      []string `json:"logs,omitempty"`
}

type RegisterReq struct {
	EnrollToken string   `json:"enrollToken,omitempty"`
	AgentToken  string   `json:"agentToken,omitempty"`
	Host        HostInfo `json:"host"`
}

type RegisterResp struct {
	AgentToken string `json:"agentToken"`
	HostID     int64  `json:"hostId"`
}

type PollResp struct {
	Task *Task `json:"task"`
	// Upgrade agent 自升级指令（与任务平行的控制字段，不经会话队列/上报）：仅当「在线、
	// 版本旧于内嵌 agentStamp、自动开关开或手动按钮置位」时随 poll 下发；旧 agent 忽略
	// 未知字段天然兼容。agent 在任务间隙处理（pollLoop 串行），处理完自替换重启。
	Upgrade *UpgradeDirective `json:"upgrade,omitempty"`
}

// UpgradeDirective agent 自升级指令：version=目标版本（=agentStamp 构建戳）、os 目标平台
// （agent 端与 runtime.GOOS 比对防错包）、sha256 内嵌二进制校验（agent 下载后校验再替换自身）
type UpgradeDirective struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	SHA256  string `json:"sha256"`
}

type Heartbeat struct {
	MemMB      int64           `json:"memMB"`
	DiskFreeGB int64           `json:"diskFreeGB"`
	Containers []ContainerPort `json:"containers,omitempty"` // 运行容器端口映射（安装预检档案；旧 agent 不带=不覆盖）
}

// HostInfo agent 注册上报的能力档案（本机节点由 console 进程内探测填充）
type HostInfo struct {
	Hostname     string          `json:"hostname"`
	IPs          []string        `json:"ips"`
	OS           string          `json:"os"`
	Arch         string          `json:"arch"`
	Docker       bool            `json:"docker"`
	DockerVer    string          `json:"dockerVersion,omitempty"`
	Compose      bool            `json:"compose"` // docker compose 插件可用（CLI 侧探测，无需 daemon）
	ComposeVer   string          `json:"composeVersion,omitempty"`
	Systemd      bool            `json:"systemd"`
	JavaVer      string          `json:"javaVersion,omitempty"`
	CPUCore      int             `json:"cpuCores"`
	MemMB        int64           `json:"memMB"`
	DiskFreeGB   int64           `json:"diskFreeGB"`
	AgentVersion string          `json:"agentVersion"`
	Containers   []ContainerPort `json:"containers,omitempty"` // 运行容器宿主端口映射（安装预检档案；旧 agent 不上报=无数据降级）
}

// ContainerPort 运行中容器的宿主端口映射（agent ports.go 同构镜像；随注册/心跳上报）
type ContainerPort struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	HostPorts []string `json:"hostPorts,omitempty"`
	Project   string   `json:"project,omitempty"` // com.docker.compose.project 标签（本项目复用判定）
}

// ---- format:3/4 部署包声明（components.json） ----
//
// format:4 在 format:3 基础上新增可选段（Component.Roles 角色化多服务 / Unit.ConfigFiles 多配置文件
// / ProbeDecl 服务级就绪探针），其余结构不变。console 双读 3|4；format:3 旧包行为零变化（单模板
// 单配置文件路径）。旧 console 只认 format:3，对 format:4 包报「不受支持」——防止旧引擎静默忽略
// 角色段把主从当普通服务半装。

const declFormat = 3
const declFormat4 = 4

// FileDecl 声明式配置文件（format:4）：模板文本内联在声明里，安装期渲染 {{VAR}}/{{端点}}/{{凭据}}
// 占位符后经 ComposePayload.Files 落盘（agent 既有管道，协议零改动）、由 compose volumes 挂载进容器。
// path=应用目录内相对路径（如 data/mysql-primary/my.cnf）。
type FileDecl struct {
	Path     string `json:"path"`
	Template string `json:"template"`
}

// ProbeDecl 服务级就绪探针（format:4，K8s readinessProbe 对应物）：tcp 缺省（发布端口监听）；
// http 走发布端口+路径；exec=容器内命令探活——引擎编译为 cmd 节点（docker exec 重试环），
// agent 协议零改动。探针脚本同模板渲染（凭据占位符可用）。
type ProbeDecl struct {
	Type   string `json:"type"`             // tcp|http|exec
	Port   string `json:"port,omitempty"`   // tcp/http：容器端口（缺省=数值最小发布端口）
	Path   string `json:"path,omitempty"`   // http：健康路径
	Script string `json:"script,omitempty"` // exec：容器内 sh -c 脚本（退出 0=就绪）
}

// InitDecl 角色初始化步骤（format:4，K8s Job 对应物）：角色服务就绪后在其所在主机 docker exec 进
// 容器执行（镜像自带客户端即可操作自身，如 mysql 建复制账号/CHANGE REPLICATION SOURCE）。
// 脚本必须幂等（重试/增量重装重放安全）。After 指定前置角色名——该角色初始化链完成后才执行本步骤
// （主从：replica 的 join 依赖 primary 的建号步骤）。
type InitDecl struct {
	ID         string `json:"id"`
	After      string `json:"after,omitempty"`      // 前置角色名（缺省=本角色就绪即可）
	Script     string `json:"script"`               // 容器内 sh -c 脚本（占位符渲染）
	TimeoutSec int    `json:"timeoutSec,omitempty"` // 缺省 300
}

// RoleDecl 组件角色（format:4，StatefulSet 角色身份对应物）：主从/读写分离等多实例拓扑——每角色
// 独立服务块（compose 内声明）、独立配置文件（角色变量如 MYSQL_SERVER_ID 区分实例）、独立就绪探针
// 与初始化链。端点仍声明在组件级（Service 指向具体服务，如 DB_HOST→primary 服务）。
type RoleDecl struct {
	Name      string            `json:"name"`
	Services  []string          `json:"services"`
	Vars      map[string]string `json:"vars,omitempty"` // 角色变量（渲染进本角色文件/脚本，如 server-id）
	Files     []FileDecl        `json:"files,omitempty"` // 本角色配置文件（多文件，挂载路径由 compose 块声明）
	Readiness *ProbeDecl        `json:"readiness,omitempty"`
	Init      []InitDecl        `json:"init,omitempty"` // 角色就绪后的初始化链（按声明顺序串行）
}

type EntryDecl struct {
	Unit       string `json:"unit"`
	Service    string `json:"service,omitempty"`
	Image      string `json:"image,omitempty"`
	Port       string `json:"port,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
}

type UnitDecl struct {
	Code       string            `json:"code"`
	Service    string            `json:"service,omitempty"`  // 首个服务名（入口/兼容口径）
	Services   []string          `json:"services,omitempty"` // 本单元全部服务名（format:3）
	Image      string            `json:"image,omitempty"`
	Port       string            `json:"port,omitempty"`       // format:3：逐单元端口（跨机端点/健康检查）
	HealthPath string            `json:"healthPath,omitempty"` // format:3：逐单元健康路径
	Artifact   string            `json:"artifact,omitempty"`   // format:3：随包制品（artifacts/ 下文件名，systemd 用）
	ConfDir    string            `json:"confDir,omitempty"`
	ConfEnv    map[string]string `json:"confEnv,omitempty"`
	ConfigFiles []FileDecl       `json:"configFiles,omitempty"` // format:4：单元多配置文件（空=回落 config-template.yaml 单模板路径）
}

type VarDecl struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	Source    string `json:"source"` // input|const|random
	Value     string `json:"value,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Env       string `json:"env,omitempty"`
}

type Endpoint struct {
	Placeholder string `json:"placeholder"`
	Service     string `json:"service"`
	Port        string `json:"port"`
	From        string `json:"from,omitempty"` // host|port|url（缺省 url）
	Scheme      string `json:"scheme,omitempty"`
}

type Credential struct {
	Placeholder string `json:"placeholder"`
	Label       string `json:"label,omitempty"`
	Env         string `json:"env,omitempty"`
	Generate    string `json:"generate,omitempty"` // ask|random|const
	Value       string `json:"value,omitempty"`
}

type SchemaExec struct {
	Dir      string `json:"dir"`
	Image    string `json:"image"`
	Host     string `json:"host"`
	Port     string `json:"port,omitempty"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// ConfigCenterDecl 组件配置中心声明（format:3 可选段，ADR-5：可选扩展不升版）。
// serverAddr 取组件首个端点（安装期按执行主机三态解析）；基线 file=包内相对路径。
type ConfigCenterDecl struct {
	Kind          string           `json:"kind"`   // nacos
	Plugin        string           `json:"plugin"` // nacos-config
	Namespace     string           `json:"namespace,omitempty"`
	IdentityKey   string           `json:"identityKey,omitempty"`   // serverIdentity 头名（凭据占位符名）
	IdentityValue string           `json:"identityValue,omitempty"` // serverIdentity 值（凭据占位符名）
	Configs       []ConfigItemDecl `json:"configs"`
}

type ConfigItemDecl struct {
	DataID string `json:"dataId"`
	Group  string `json:"group,omitempty"`
	File   string `json:"file"`
}

type ComponentDecl struct {
	Code         string            `json:"code"`
	Category     string            `json:"category,omitempty"`
	Services     []string          `json:"services,omitempty"` // format:3：本组件全部服务名
	Images       []string          `json:"images,omitempty"`   // format:3：本组件镜像清单（agent 按主机裁剪）
	Endpoints    []Endpoint        `json:"endpoints,omitempty"`
	Credentials  []Credential      `json:"credentials,omitempty"`
	Schema       *SchemaExec       `json:"schema,omitempty"`
	ConfigCenter *ConfigCenterDecl `json:"configCenter,omitempty"`
	// format:4 可选段（ADR-5 惯例：可选扩展；本组为结构性新能力，升版隔离旧 console）
	Roles       []RoleDecl `json:"roles,omitempty"`       // 角色化多服务（主从等；服务块仍在 compose 声明）
	SpreadHosts bool       `json:"spreadHosts,omitempty"` // 反亲和（K8s pod anti-affinity 对应物）：本组件已落机服务必须两两不同主机
	// 可选资源缺省（ADR-11；可选段不升版——旧 console 忽略无害）：console 安装时 placement.resources
	// 逐服务覆盖优先，缺省兜底；物化 deploy.resources.limits、resHash 参与增量 diff
	Resources *ResourceSpec `json:"resources,omitempty"`
}

type ComponentsDecl struct {
	Format     int             `json:"format"` // 3|4
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Entry      EntryDecl       `json:"entry"`
	Units      []UnitDecl      `json:"units"`
	Variables  []VarDecl       `json:"variables"`
	Components []ComponentDecl `json:"components"`
}
