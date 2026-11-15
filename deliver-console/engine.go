// 编排引擎：分配校验 → 变量/凭据解析 → 端点三态回填（同机=服务名 / 跨机=主机地址:发布端口 /
// 外部=手填）→ 逐主机物化（compose 切分/.env/配置渲染/systemd unit/装载清单）→ 默认 DAG 生成
// → 增量 diff（对 inventory，apply 语义：仅变更项入计划）→ 自定义 DAG 覆盖。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ResourceSpec 容器资源限额（K8s resources.limits 对应物）：物化进 compose
// deploy.resources.limits（docker compose v2 up 生效，v1 回退忽略无害）；inventory detail 记
// resHash 参与增量 diff——改限额只 recreate 该服务。
type ResourceSpec struct {
	Cpus  string `json:"cpus,omitempty"`  // CPU 核数（如 "2"、"0.5"）
	MemMB int    `json:"memMB,omitempty"` // 内存上限 MB
}

// InstanceSpec 组件实例化组态（console 组态向导产物，ADR-11）：把包内组件按内置拓扑模板展开成
// 多实例（如 mysql → mysql-primary + mysql-replica）——包只出素材，拓扑安装时组态，免重打包。
// 实例落机沿用 placement.host[实例服务名]；Port 覆盖宿主端口（缺省=模板端口）。
type InstanceSpec struct {
	Component string `json:"component"`      // 源组件编码
	Form      string `json:"form"`           // 拓扑形态 ID（topology.go 内置，如 mysql-ha-1）
	Service   string `json:"service"`        // 实例服务名（如 mysql-primary）
	Role      string `json:"role"`           // 角色名（primary|replica）
	Port      string `json:"port,omitempty"` // 宿主端口覆盖
}

// Placement 安装方案（UI 编辑、scheme 落库）
type Placement struct {
	Mode map[string]string `json:"mode"` // service → docker|systemd（单元可选 systemd；组件恒 docker）
	Host map[string]int64  `json:"host"` // service → hostId（缺省=组件未落机=外部复用；实例化组件此处为实例服务名）
	Vars map[string]string `json:"vars"` // 变量与凭据取值（input/ask 由 UI 表单收集）
	Ext  map[string]string `json:"ext"`  // 外部端点手填值（未落机组件端点占位符 → 值）
	// Skip 跳过的组件编码（第三态：不装也不复用——配置段剔除、端点/凭据免填；组件三态=落机/外部复用/跳过）
	Skip []string `json:"skip,omitempty"`
	// DockerInstall 无 docker 的 Linux 主机不拒收：编排头部自动插入「安装 Docker」节点（仅 Linux）
	DockerInstall bool `json:"dockerInstall,omitempty"`
	// console 组态器产物（K8s「镜像进仓、manifest 安装时编辑」对应物，ADR-11）
	Instances []InstanceSpec         `json:"instances,omitempty"`  // 组件实例化（组态后组件原服务不再放置）
	Resources map[string]ResourceSpec `json:"resources,omitempty"` // svc → 资源限额（实例与普通服务均可）
}

// DagNode DAG 节点（UI 自由编排；自动类节点 key 与引擎生成约定一致，payload 以引擎为权威）
type DagNode struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // load|compose|systemd|cmd|wait|plugin|gate|docker|toolkit
	Title  string   `json:"title"`
	Host   int64    `json:"host,omitempty"`
	Script string   `json:"script,omitempty"` // cmd：脚本内容
	Note   string   `json:"note,omitempty"`   // gate：提示语
	Mode   string   `json:"mode,omitempty"`   // plugin(apply)：idempotent|force（自定义 DAG 可改）
	Deps   []string `json:"deps"`
}

// planNode 会话执行计划节点（物化后的任务快照，随会话落库）
type planNode struct {
	Key          string            `json:"key"`
	Kind         string            `json:"kind"`
	Title        string            `json:"title"`
	Host         int64             `json:"host"`
	Deps         []string          `json:"deps"`
	Payload      json.RawMessage   `json:"payload"`
	TimeoutSec   int               `json:"timeoutSec,omitempty"`
	InvCompose   []string          `json:"invCompose,omitempty"`   // 节点成功后登记的 compose 服务（host 级）
	InvSystemd   string            `json:"invSystemd,omitempty"`   // 节点成功后登记的 systemd 服务
	InvDetail    map[string]string `json:"invDetail,omitempty"`    // service → detail JSON（增量 diff 基线）
	InvEvictHost int64             `json:"invEvictHost,omitempty"` // cleanup 节点：被迁走服务原先所在主机（K8s drain 驱逐）
	InvEvictSvcs []string          `json:"invEvictSvcs,omitempty"` // cleanup 节点：迁走的服务清单（成功后删对应 inventory 行）
}

type buildResult struct {
	Nodes    []planNode        `json:"nodes"`
	Vars     map[string]string `json:"-"`
	EntryURL string            `json:"entryUrl"`
	Warnings []string          `json:"warnings"`
	Mode     string            `json:"mode"`
	Changes  []svcChange       `json:"changes"` // 统一 diff（预览与执行同源；fresh=全 create）
	InvRows  []invRow          `json:"-"`
}

func normPlacement(pl *Placement) *Placement {
	if pl == nil {
		pl = &Placement{}
	}
	n := &Placement{Mode: map[string]string{}, Host: map[string]int64{}, Vars: map[string]string{}, Ext: map[string]string{},
		Skip: append([]string(nil), pl.Skip...), DockerInstall: pl.DockerInstall,
		Instances: append([]InstanceSpec(nil), pl.Instances...)}
	for k, v := range pl.Mode {
		n.Mode[k] = v
	}
	for k, v := range pl.Host {
		n.Host[k] = v
	}
	for k, v := range pl.Vars {
		n.Vars[k] = v
	}
	for k, v := range pl.Ext {
		n.Ext[k] = v
	}
	if pl.Resources != nil {
		n.Resources = make(map[string]ResourceSpec, len(pl.Resources))
		for k, v := range pl.Resources {
			n.Resources[k] = v
		}
	}
	return n
}

// DockerOpts 基础工具安装源（console settings 物化传入，引擎与 Server 解耦）：
// docker 引擎——ScriptURL 空时兜底 defaultDockerScript（在线通道恒可用），Media 版本化介质优先；
// compose 插件——Compose（nil=未配置，引擎跳过 compose 自动补装）。
type DockerOpts struct {
	ScriptURL string
	Mirrors   []string
	Media     mediaLib
	Compose   *ComposeOpts
}

// ComposeOpts docker compose 插件安装源：Template 在线直链模板（{version}/{platform} 占位符，
// 空=defaultComposeTemplate 兜底）、Version 在线缺省版本；Media 版本化介质（优先于在线）。
type ComposeOpts struct {
	Template string
	Version  string
	Media    mediaLib
}

const defaultDockerScript = "https://get.docker.com"

// defaultComposeTemplate compose 在线直链兜底模板（官方 GitHub releases；国内网络可在安装源页改镜像站）
const defaultComposeTemplate = "https://github.com/docker/compose/releases/download/{version}/docker-compose-{platform}"

// composePlatform compose 官方资产平台段（GOARCH → GitHub releases 资产名；未知架构空）
func composePlatform(goarch string) string {
	switch goarch {
	case "amd64":
		return "linux-x86_64"
	case "arm64":
		return "linux-aarch64"
	default:
		return ""
	}
}

// renderComposeURL 在线直链渲染（{version}/{platform} 占位替换；模板不含占位符则原样返回——
// 即管理员钉死单一在线版本的用法）
func renderComposeURL(template, version, goarch string) string {
	u := strings.ReplaceAll(template, "{version}", version)
	return strings.ReplaceAll(u, "{platform}", composePlatform(goarch))
}

// composeOptsOf nil 安全取 compose 安装源
func composeOptsOf(opts *DockerOpts) *ComposeOpts {
	if opts == nil {
		return nil
	}
	return opts.Compose
}

// dockerInstallKey 安装 Docker 节点 key（主机级，DAG 头部）
func dockerInstallKey(hostID int64) string { return fmt.Sprintf("docker-install-h%d", hostID) }

// composeInstallKey 安装 docker compose 节点 key（主机级，DAG 头部）
func composeInstallKey(hostID int64) string { return fmt.Sprintf("compose-install-h%d", hostID) }

// buildDockerNode 按安装源物化「安装/维护 Docker」节点（引擎自动补装与主机池一键装共用）。
// version 指定目标版本（空=最新介质/在线最新）；指定版本仅介质可承载——一键装入口已先行校验
// 介质存在，引擎路径恒传 ""。legacy 介质不带版本（不参与精确匹配）。
func buildDockerNode(h *hostRow, opts *DockerOpts, version string, force bool) planNode {
	p := DockerPayload{TimeoutSec: 1800, Force: force}
	if opts != nil {
		if m, ver := pickMedia(opts.Media, h.Info.OS+"-"+h.Info.Arch, version); m.SHA256 != "" {
			p.MediaName, p.MediaSHA256, p.MediaSize, p.Version = m.Name, m.SHA256, m.Size, ver
		}
		p.ScriptURL, p.Mirrors = opts.ScriptURL, opts.Mirrors
	}
	if p.MediaName == "" && p.ScriptURL == "" {
		p.ScriptURL = defaultDockerScript // 在线兜底恒可用（清空设置=回到官方脚本）
	}
	title := h.Name + "：安装 Docker"
	if p.Version != "" {
		title += " " + p.Version
	}
	return planNode{Key: dockerInstallKey(h.ID), Kind: "docker",
		Title: title, Host: h.ID,
		Payload: mustJSON(p), TimeoutSec: 1800}
}

// buildToolkitPayload 物化 compose 安装载荷（介质优先——指定版本精确取/缺省取最新；无介质走
// 在线直链：指定版本或在线缺省版本渲染，模板空=官方 GitHub 兜底）。两通道皆空=无安装源，
// 调用方按需校验。
func buildToolkitPayload(h *hostRow, opts *DockerOpts, version string, force bool) ToolkitPayload {
	p := ToolkitPayload{App: "compose", Tool: "compose", TimeoutSec: 600, Force: force}
	if co := composeOptsOf(opts); co != nil {
		if m, ver := pickMedia(co.Media, h.Info.OS+"-"+h.Info.Arch, version); m.SHA256 != "" {
			p.MediaName, p.MediaSHA256, p.MediaSize, p.MediaVer = m.Name, m.SHA256, m.Size, ver
		} else {
			v := orStr(version, co.Version)
			if v != "" && composePlatform(h.Info.Arch) != "" {
				p.MediaVer, p.OnlineURL = v, renderComposeURL(orStr(co.Template, defaultComposeTemplate), v, h.Info.Arch)
			}
		}
	}
	return p
}

// buildToolkitNode 按安装源物化「安装 docker-compose」节点（引擎自动补装与主机池一键装共用）
func buildToolkitNode(h *hostRow, opts *DockerOpts, version string, force bool) planNode {
	p := buildToolkitPayload(h, opts, version, force)
	title := h.Name + "：安装 docker-compose"
	if p.MediaVer != "" {
		title += " " + p.MediaVer
	}
	return planNode{Key: composeInstallKey(h.ID), Kind: "toolkit",
		Title: title, Host: h.ID,
		Payload: mustJSON(p), TimeoutSec: 600}
}

// BuildSessionPlan 会话构建主入口：mode=fresh|incremental|overwrite（incremental 且无库存时自动
// 回落 fresh；overwrite=覆盖重装——跳过增量裁剪全量物化执行，apply --force 对应物，./data 数据保留）；
// dopts=Docker 安装源（placement.DockerInstall 开启时用于物化安装节点，nil=仅默认在线脚本）
func BuildSessionPlan(pp *ParsedPackage, pl *Placement, hosts map[int64]*hostRow, inventory []invRow,
	customDag []DagNode, mode string, dopts *DockerOpts) (*buildResult, error) {
	switch mode {
	case "fresh", "incremental", "overwrite":
	default:
		return nil, fmt.Errorf("安装模式非法 %q（fresh|incremental|overwrite）", mode)
	}
	b := &builder{pp: pp, pl: normPlacement(pl), hosts: hosts, inv: inventory,
		vars: map[string]string{}, svcHost: map[string]int64{}, svcMode: map[string]string{},
		confBySvc: map[string]map[string]string{}, dockerByHost: map[int64][]string{}, filesByHost: map[int64][]FileSpec{},
		sysByHost: map[int64][]string{}, sysPayload: map[string]SystemdPayload{}, dockerSvcs: map[string]bool{},
		confCrossDeps: map[int64][]string{},
		ccApplyBySv:   map[string]string{}, needDocker: map[int64]bool{}, needCompose: map[int64]bool{}, dockerOpts: dopts,
		readinessProbe: map[string]*ProbeDecl{}, initSvcsByNode: map[string][]string{}, roleInitLast: map[string]string{},
		roleBySvc:      map[string]*RoleDecl{},
		skipComp: map[string]bool{}, skipSvc: map[string]bool{}}
	b.initSkip()
	// console 组态器：placement.instances 命中的组件按内置拓扑模板展开（ADR-11，topology.go）——
	// 在变量解析/校验之前改写包视图（补充模板凭据须先进 resolveVars），后续链路原样运行
	if err := b.applyTopology(); err != nil {
		return nil, err
	}
	if err := b.resolveVars(); err != nil {
		return nil, err
	}
	// 纯 cmd/gate 自定义 DAG（工具型任务：巡检/数据修复等）不要求服务分配，跳过自动编排
	utilityOnly := len(customDag) > 0
	for _, n := range customDag {
		if n.Kind != "cmd" && n.Kind != "gate" {
			utilityOnly = false
			break
		}
	}
	var nodes []planNode
	if utilityOnly {
		mode = "fresh"
	} else {
		if err := b.validate(); err != nil {
			return nil, err
		}
		if err := b.materialize(); err != nil {
			return nil, err
		}
		var err error
		if nodes, err = b.buildDag(); err != nil {
			return nil, err
		}
	}
	if mode == "incremental" && len(inventory) == 0 {
		mode = "fresh"
	}
	// 统一 diff（期望态 vs inventory）：fresh/overwrite 也计算（预览同源——fresh 全 create、
	// overwrite 如实呈现差异但执行不裁剪）；incremental 据此裁剪计划
	changes := b.computeChanges(nodes)
	if mode == "incremental" {
		nodes = b.diffFilter(nodes, changes)
		if len(nodes) == 0 {
			b.warnings = append(b.warnings, "增量计划为空：全部服务与配置均未变化，无需执行")
		}
	} else if mode == "overwrite" && len(inventory) > 0 {
		// 覆盖重装：迁移服务仍需旧主机清理（否则残留容器与新实例并存）；全量节点保留不裁剪
		nodes = b.appendEvictions(nodes, changes)
		b.warnings = append(b.warnings, "覆盖重装：全量重新物化执行（数据目录 ./data 保留；配置与镜像按包内最新为准）")
	}
	if len(customDag) > 0 {
		overlay, err := b.overlayDag(customDag, nodes)
		if err != nil {
			return nil, err
		}
		nodes = overlay
	}
	nodes = b.enforceDockerNodes(nodes)
	b.warnDockerAgentVersion()
	if err := b.precheckPorts(nodes); err != nil {
		return nil, err
	}
	if len(nodes) > 0 {
		if err := validateDag(nodes); err != nil {
			return nil, err
		}
	}
	return &buildResult{Nodes: nodes, Vars: b.vars, EntryURL: b.entryURL(),
		Warnings: b.warnings, Mode: mode, Changes: changes, InvRows: b.invRows(nodes)}, nil
}

// ---- builder ----

type builder struct {
	pp    *ParsedPackage
	pl    *Placement
	hosts map[int64]*hostRow
	inv   []invRow

	vars    map[string]string
	svcHost map[string]int64
	svcMode map[string]string

	envText      string
	confBySvc    map[string]map[string]string // 服务级配置文件集（desired.go；替代旧主机级 confByHost）
	dockerByHost map[int64][]string
	filesByHost  map[int64][]FileSpec
	sysByHost    map[int64][]string
	sysPayload   map[string]SystemdPayload
	dockerSvcs   map[string]bool

	// format:4 角色链（StatefulSet 对应物）：探针/初始化编译产物索引
	readinessProbe map[string]*ProbeDecl // svc → 角色就绪探针（waitNode 消费；exec 型编译为 cmd 重试环）
	initSvcsByNode map[string][]string   // init 节点 key → 角色落机服务集（增量裁剪判据）
	roleInitLast   map[string]string     // "comp:role" → 末位 init 节点 key（After/wait-entry 接线）
	roleBySvc      map[string]*RoleDecl  // svc → 角色声明（脚本渲染取角色变量）

	// 配置端点跨机引用（消费主机 → 模板实际引用且落机在别的主机的服务）：应用级依赖对引擎
	// 不可见（包内 depends_on 常缺——app 经 DB_HOST 连 mysql），端点声明是权威信号，buildDag
	// 并入 crossDeps 生成跨机 wait 门控，防「消费方先于依赖就绪启动→崩溃循环→wait 超时」
	confCrossDeps map[int64][]string

	// schema 物化（外部组件 SQL 预置；buildDag 消费）
	schemaNode  *planNode
	schemaHosts map[int64]bool // 需带 extras/SQL 的主机
	schemaGate  []string       // 受 schema 门控的节点 key（应用侧服务节点）

	// 配置中心插件链（configCenter 声明组件；materialize 生成节点，buildDag 接线前置依赖）
	ccChains    []ccChain
	ccApplyBySv map[string]string // 配置中心组件服务 → cc-apply 节点 key（消费方编排节点后置）

	// 自动安装 Docker（placement.DockerInstall）：无 docker 的 Linux 主机集合与安装源
	needDocker  map[int64]bool
	needCompose map[int64]bool
	dockerOpts  *DockerOpts

	// 跳过组件（placement.Skip，第三态=不装不复用）：配置段剔除、端点/凭据免填
	skipComp map[string]bool
	skipSvc  map[string]bool

	warnings []string
}

// initSkip 跳过组件集初始化：placement.Skip 的编码 → 组件/服务两级索引（validate/resolveVars/
// materialize 共用；未知编码忽略——包里没有该组件时无从跳起）
func (b *builder) initSkip() {
	for _, code := range b.pl.Skip {
		b.skipComp[code] = true
	}
	for _, c := range b.pp.Decl.Components {
		if b.skipComp[c.Code] {
			for _, s := range c.Services {
				b.skipSvc[s] = true
			}
		}
	}
}

// compPlaced 组件是否落机（任一服务分配了主机即视为落机；validate 前也可用）
func (b *builder) compPlaced(c ComponentDecl) bool {
	for _, s := range c.Services {
		if _, ok := b.pl.Host[s]; ok {
			return true
		}
	}
	return false
}

// ccChain 单个配置中心组件的插件链四节点（plan → gate → apply → health）
type ccChain struct {
	code   string
	svc    string // 组件首个服务（落机时；外部组件为空）
	hostID int64  // 组件主机（落机时）
	nodes  [4]planNode
}

// validate 分配校验：单元必须落机；systemd 需 Linux+systemd+java+单服务单元+随包制品；
// docker 需主机有 docker；组件要么整体落机要么整体外部
func (b *builder) validate() error {
	pp := b.pp
	unitSvcs := map[string]string{} // service → unit code
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			unitSvcs[s] = u.Code
		}
	}
	compSvcs := map[string]string{}
	for _, c := range pp.Decl.Components {
		for _, s := range c.Services {
			compSvcs[s] = c.Code
		}
	}
	hostSvcs := map[int64][]string{}
	for svc, hostID := range b.pl.Host {
		if b.skipSvc[svc] {
			return fmt.Errorf("服务 %s 所属组件已标记跳过，但仍分配到主机——请先取消分配或移除跳过标记", svc)
		}
		h, ok := b.hosts[hostID]
		if !ok {
			return fmt.Errorf("服务 %s 分配到不存在的主机 #%d", svc, hostID)
		}
		if h.Revoked {
			return fmt.Errorf("服务 %s 分配到已吊销主机 %s", svc, h.Name)
		}
		hostSvcs[hostID] = append(hostSvcs[hostID], svc)
	}
	// 单元落机与形态校验
	for svc, code := range unitSvcs {
		hostID, ok := b.pl.Host[svc]
		if !ok {
			return fmt.Errorf("单元服务 %s（%s）未分配主机", svc, code)
		}
		mode := b.pl.Mode[svc]
		if mode == "" {
			mode = "docker"
		}
		h := b.hosts[hostID]
		var unit *UnitDecl
		for i := range pp.Decl.Units {
			if pp.Decl.Units[i].Code == code {
				unit = &pp.Decl.Units[i]
			}
		}
		if unit == nil {
			return fmt.Errorf("服务 %s 的单元 %s 不在声明中", svc, code)
		}
		if mode == "systemd" {
			if len(unitServices(*unit)) > 1 {
				return fmt.Errorf("单元 %s 含多个服务，不支持 systemd（裸机）形态", code)
			}
			if unit.Artifact == "" {
				return fmt.Errorf("单元 %s 的部署包未随包附带制品（打包时勾选「制品随包附带」后才支持 systemd）", code)
			}
			if h.Info.OS != "linux" {
				return fmt.Errorf("单元 %s 以 systemd 形态只能分配到 Linux 主机（%s 是 %s）", code, h.Name, h.Info.OS)
			}
			if !h.Info.Systemd {
				return fmt.Errorf("主机 %s 无 systemd，不能承载单元 %s 的裸机形态", h.Name, code)
			}
			if h.Info.JavaVer == "" {
				return fmt.Errorf("主机 %s 未探测到 java（裸机 fat-jar 运行必需），不能承载单元 %s", h.Name, code)
			}
		} else {
			if !h.Info.Docker && !b.allowDockerless(h, svc) {
				if b.pl.DockerInstall && h.Info.OS != "linux" {
					return fmt.Errorf("主机 %s 无 docker 且为 %s——自动安装仅支持 Linux（Windows 请预装 Docker Desktop）", h.Name, h.Info.OS)
				}
				return fmt.Errorf("主机 %s 无 docker（或守护进程未运行），不能承载服务 %s（分配步可开启「自动安装 Docker」）", h.Name, svc)
			}
		}
		b.svcHost[svc], b.svcMode[svc] = hostID, mode
	}
	// 组件整体性：同组件服务要么全落机要么全外部；跳过组件（第三态）整体不参与编排
	for _, c := range pp.Decl.Components {
		if b.skipComp[c.Code] {
			if c.ConfigCenter != nil {
				return fmt.Errorf("组件 %s 是配置基线载体（声明了 configCenter），不能跳过——本包服务配置依赖其发布；请改为落机或外部复用", c.Code)
			}
			continue
		}
		placed := 0
		for _, s := range c.Services {
			if _, ok := b.pl.Host[s]; ok {
				placed++
			}
		}
		if placed > 0 && placed < len(c.Services) {
			return fmt.Errorf("组件 %s 的服务必须整体落机或整体外部（当前 %d/%d 落机）", c.Code, placed, len(c.Services))
		}
		// 反亲和硬校验（pod anti-affinity 对应物）：SpreadHosts 组件的已落机服务必须两两不同主机
		//（主从同机=单点故障+资源争抢，preset 声明即硬约束）
		if c.SpreadHosts && placed > 1 {
			seen := map[int64]string{}
			for _, s := range c.Services {
				hostID, ok := b.pl.Host[s]
				if !ok {
					continue
				}
				if prev, dup := seen[hostID]; dup {
					h := b.hosts[hostID]
					name := fmt.Sprintf("h%d", hostID)
					if h != nil {
						name = h.Name
					}
					return fmt.Errorf("组件 %s 声明了反亲和（spreadHosts），服务 %s 与 %s 不能同落主机 %s——请分配到不同主机（主从同机即单点，违背主从初衷）",
						c.Code, prev, s, name)
				}
				seen[hostID] = s
			}
		}
		for _, s := range c.Services {
			if hostID, ok := b.pl.Host[s]; ok {
				h := b.hosts[hostID]
				if !h.Info.Docker && !b.allowDockerless(h, s) {
					return fmt.Errorf("主机 %s 无 docker，不能承载组件服务 %s（分配步可开启「自动安装 Docker」）", h.Name, s)
				}
				b.svcHost[s], b.svcMode[s] = hostID, "docker"
			}
		}
	}
	// 未识别服务（compose 有块但声明没登记）：允许落机与否均可，落机则按 docker 校验
	for svc := range pp.Meta {
		if _, isUnit := unitSvcs[svc]; isUnit {
			continue
		}
		if _, isComp := compSvcs[svc]; isComp {
			continue
		}
		if hostID, ok := b.pl.Host[svc]; ok {
			h := b.hosts[hostID]
			if !h.Info.Docker && !b.allowDockerless(h, svc) {
				return fmt.Errorf("主机 %s 无 docker，不能承载服务 %s（分配步可开启「自动安装 Docker」）", h.Name, svc)
			}
			b.svcHost[svc], b.svcMode[svc] = hostID, "docker"
			b.warnings = append(b.warnings, fmt.Sprintf("服务 %s 未在声明中登记归属，按 docker 编排处理", svc))
		}
	}
	return nil
}

func unitServices(u UnitDecl) []string {
	if len(u.Services) > 0 {
		return u.Services
	}
	if u.Service != "" {
		return []string{u.Service}
	}
	return nil
}

// allowDockerless 主机无 docker 的放行判定：自动安装开启且 Linux → 登记 needDocker（首次 warn）
// 并放行（安装节点由 buildDag/enforceDockerNodes 注入）；否则 false（调用方维持原拒收）
func (b *builder) allowDockerless(h *hostRow, svc string) bool {
	if !b.pl.DockerInstall || h.Info.OS != "linux" {
		return false
	}
	if !b.needDocker[h.ID] {
		b.needDocker[h.ID] = true
		b.warnings = append(b.warnings, fmt.Sprintf("主机 %s 无 docker，将在安装前自动安装 Docker（服务 %s 所需）", h.Name, svc))
	}
	return true
}

// enforceDockerNodes 保底注入：needDocker 主机若缺 docker-install 节点（自定义/导入 DAG 没带），
// 补插到头部并把该主机 load/compose 节点与 schema cmd 挂上依赖——自定义 DAG 不能丢掉安装前置。
// 随后 compose 保底注入（enforceComposeNodes）。
func (b *builder) enforceDockerNodes(nodes []planNode) []planNode {
	if len(b.needDocker) == 0 {
		return b.enforceComposeNodes(nodes, nodeIndex(nodes))
	}
	byKey := nodeIndex(nodes)
	var ids []int64
	for id := range b.needDocker {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var inject []planNode
	for _, id := range ids {
		key := dockerInstallKey(id)
		if _, ok := byKey[key]; ok {
			continue
		}
		n := buildDockerNode(b.hosts[id], b.dockerOpts, "", false)
		inject = append(inject, n)
		byKey[n.Key] = &inject[len(inject)-1]
		for i := range nodes {
			if nodes[i].Host == id && (nodes[i].Kind == "load" || nodes[i].Kind == "compose") {
				nodes[i].Deps = append(nodes[i].Deps, key)
			}
		}
	}
	if len(inject) > 0 {
		nodes = append(inject, nodes...)
	}
	return b.enforceComposeNodes(nodes, byKey)
}

// nodeIndex key → 节点指针索引（保底注入接线用）
func nodeIndex(nodes []planNode) map[string]*planNode {
	byKey := map[string]*planNode{}
	for i := range nodes {
		byKey[nodes[i].Key] = &nodes[i]
	}
	return byKey
}

// enforceComposeNodes compose 保底注入：编排含 compose 节点的 Linux 主机，DockerInstall 开关下
// 缺 compose 插件（或无 docker 连带未知——装完 docker 由 agent 快路径自判跳过）→ 注入
// compose-install 节点（该主机已有 docker-install 时后置于它，compose 节点全部后置）；
// 无安装源不注入（compose 任务自带 docker-compose 旧客户端回退链路兜底）。
func (b *builder) enforceComposeNodes(nodes []planNode, byKey map[string]*planNode) []planNode {
	if !b.pl.DockerInstall {
		return nodes
	}
	hasComposeNode := map[int64]bool{}
	for _, n := range nodes {
		if n.Kind == "compose" {
			hasComposeNode[n.Host] = true
		}
	}
	if len(hasComposeNode) == 0 {
		return nodes
	}
	var ids []int64
	for id := range hasComposeNode {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var inject []planNode
	for _, id := range ids {
		h := b.hosts[id]
		if h == nil || h.Info.OS != "linux" {
			continue // Windows 主机不支持自动安装（预装 Docker Desktop 自带 compose）
		}
		if _, exists := byKey[composeInstallKey(id)]; exists {
			continue
		}
		if h.Info.Docker && h.Info.Compose {
			continue // 已就绪
		}
		p := buildToolkitPayload(h, b.dockerOpts, "", false)
		if p.MediaSHA256 == "" && p.OnlineURL == "" {
			continue // 无安装源：不注入
		}
		n := buildToolkitNode(h, b.dockerOpts, "", false)
		if _, hasDockerNode := byKey[dockerInstallKey(id)]; hasDockerNode {
			n.Deps = append(n.Deps, dockerInstallKey(id)) // 先装 docker 再补 compose
		}
		if !b.needCompose[id] {
			b.needCompose[id] = true
			if !h.Info.Docker {
				b.warnings = append(b.warnings, fmt.Sprintf("主机 %s 无 docker，将在安装 Docker 后自动补装 docker compose 插件", h.Name))
			} else {
				b.warnings = append(b.warnings, fmt.Sprintf("主机 %s 缺 docker compose 插件，将在安装前自动补装", h.Name))
			}
		}
		inject = append(inject, n)
		byKey[n.Key] = &inject[len(inject)-1]
		for i := range nodes {
			if nodes[i].Host == id && nodes[i].Kind == "compose" {
				nodes[i].Deps = append(nodes[i].Deps, n.Key)
			}
		}
	}
	if len(inject) == 0 {
		return nodes
	}
	return append(inject, nodes...)
}

// warnDockerAgentVersion 旧 agent 护栏：docker/toolkit 任务对旧 agent 是新任务类型，版本低于
// console 内嵌戳的远端主机会报「未知任务类型」——提前 warning 提示重装（agentStamp 构建期注入，空=跳过）
func (b *builder) warnDockerAgentVersion() {
	if (len(b.needDocker) == 0 && len(b.needCompose) == 0) || agentStamp == "" {
		return
	}
	warned := map[int64]bool{}
	for _, m := range []map[int64]bool{b.needDocker, b.needCompose} {
		for id := range m {
			if warned[id] {
				continue
			}
			warned[id] = true
			h := b.hosts[id]
			if h == nil || h.Local || h.Info.AgentVersion == "" || h.Info.AgentVersion == agentStamp {
				continue
			}
			b.warnf("主机 %s 的 agent 版本 %s 低于 console 内嵌 %s——安装 Docker/Compose 需重装 agent（主机池重新下载）",
				h.Name, h.Info.AgentVersion, agentStamp)
		}
	}
}

// resolveVars 变量与凭据解析：const/random/input（预填与 UI 取值兜底顺序：UI 表单 > delivery 预填）
func (b *builder) resolveVars() error {
	pl, prefill := b.pl, b.pp.Prefill
	if pl.Vars == nil {
		pl.Vars = map[string]string{}
	}
	for _, v := range b.pp.Decl.Variables {
		if b.vars[v.Name] != "" {
			continue
		}
		given := pl.Vars[v.Name]
		switch v.Source {
		case "const":
			b.vars[v.Name] = v.Value
		case "random":
			if given == "" {
				if pv, ok := prefill[v.Name]; ok {
					given = pv
				} else {
					given = randomSecret16()
				}
			}
			b.vars[v.Name] = given
		default: // input
			if given == "" {
				given = prefill[v.Name]
			}
			if given == "" {
				return fmt.Errorf("变量 %s（%s）未赋值——请在安装表单填写", v.Name, orStr(v.Label, v.Name))
			}
			b.vars[v.Name] = given
		}
	}
	for _, c := range b.pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue // 跳过组件：凭据免填不进 .env（对应配置段已整体剔除）
		}
		for _, cred := range c.Credentials {
			if b.vars[cred.Placeholder] != "" {
				continue
			}
			given := pl.Vars[cred.Placeholder]
			switch cred.Generate {
			case "random":
				if given == "" {
					given = randomSecret16()
				}
			case "const":
				given = orStr(given, cred.Value)
			default: // ask
				if given == "" {
					given = prefill[cred.Placeholder]
				}
				if given == "" {
					if b.compPlaced(c) {
						// 落机组件=本包新起实例：留空随机生成（与安装表单「留空=随机」提示一致）
						given = randomSecret16()
					} else {
						// 外部复用组件：外部实例口令无从随机生成，必须填写
						return fmt.Errorf("凭据 %s（%s）未赋值——组件 %s 为外部复用，实例口令无法随机生成，请在安装表单填写",
							cred.Placeholder, orStr(cred.Label, cred.Placeholder), c.Code)
					}
				}
			}
			b.vars[cred.Placeholder] = given
		}
	}
	// 根 .env：变量（Env 非空）+ 组件凭据（env 缺省=占位符名）——compose ${VAR} 引用同源
	var env strings.Builder
	for _, v := range b.pp.Decl.Variables {
		if v.Env != "" {
			env.WriteString(v.Env + "=" + b.vars[v.Name] + "\n")
		}
	}
	for _, c := range b.pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue
		}
		for _, cred := range c.Credentials {
			env.WriteString(orStr(cred.Env, cred.Placeholder) + "=" + b.vars[cred.Placeholder] + "\n")
		}
	}
	b.envText = env.String()
	return nil
}

// hostAddr 主机访问地址（UI 可改，缺省=agent 上报首个 IP）
func (b *builder) hostAddr(hostID int64) string {
	if h := b.hosts[hostID]; h != nil {
		if h.Addr != "" {
			return h.Addr
		}
		if len(h.Info.IPs) > 0 {
			return h.Info.IPs[0]
		}
		return h.Name
	}
	return ""
}

// endpointValueFor 端点占位符在「消费主机」视角的取值（三态：同机=服务名 / 跨机=地址:发布端口 / 外部=手填）
func (b *builder) endpointValueFor(ep Endpoint, consumerHost int64) (string, error) {
	// 未落机（外部复用）：手填值权威（任意端点形态，含 from=port——外部端口可能与声明不同）
	if _, placed := b.svcHost[ep.Service]; !placed {
		v, ok := b.pl.Ext[ep.Placeholder]
		if !ok || v == "" {
			return "", fmt.Errorf("组件端点 %s（服务 %s）未落机且未手填外部地址——在「外部端点」表单补齐", ep.Placeholder, ep.Service)
		}
		return v, nil
	}
	if ep.From == "port" {
		return ep.Port, nil
	}
	var hostPart string
	if hostID := b.svcHost[ep.Service]; hostID == consumerHost {
		hostPart = ep.Service + ":" + ep.Port // 同机：compose 容器网络互访（容器端口）
	} else {
		meta := b.pp.Meta[ep.Service]
		hostPort := ep.Port
		if meta != nil {
			if hp, ok := meta.Ports[ep.Port]; ok {
				hostPort = hp
			} else if len(meta.Ports) > 0 {
				b.warnf("服务 %s 未发布容器端口 %s，跨机访问可能不通（compose 需加 ports 映射）", ep.Service, ep.Port)
			}
		}
		hostPart = b.hostAddr(hostID) + ":" + hostPort
	}
	if ep.From == "host" {
		return strings.SplitN(hostPart, ":", 2)[0], nil
	}
	return joinURL(ep, hostPart), nil
}

func joinURL(ep Endpoint, hostPart string) string {
	if ep.Scheme != "" {
		return ep.Scheme + "://" + hostPart
	}
	return hostPart
}

// endpointValueGlobal 端点占位符的全局取值（配置中心 Data ID 渲染视角）：Data ID 被全部消费服务共享，
// 不能用「消费主机视角」的服务名形态——一律「主机地址:发布端口」（同机消费者经宿主发布端口同样可达）；
// 未落机=手填外部值（与 endpointValueFor 同口径）
func (b *builder) endpointValueGlobal(ep Endpoint) (string, error) {
	hostID, placed := b.svcHost[ep.Service]
	if !placed {
		v, ok := b.pl.Ext[ep.Placeholder]
		if !ok || v == "" {
			return "", fmt.Errorf("组件端点 %s（服务 %s）未落机且未手填外部地址——在「外部端点」表单补齐", ep.Placeholder, ep.Service)
		}
		return v, nil
	}
	if ep.From == "port" {
		if meta := b.pp.Meta[ep.Service]; meta != nil {
			if hp, ok := meta.Ports[ep.Port]; ok {
				return hp, nil
			}
		}
		return ep.Port, nil
	}
	hostPart := b.hostAddr(hostID) + ":" + ep.Port
	if meta := b.pp.Meta[ep.Service]; meta != nil {
		if hp, ok := meta.Ports[ep.Port]; ok {
			hostPart = b.hostAddr(hostID) + ":" + hp
		} else if len(meta.Ports) > 0 {
			b.warnf("服务 %s 未发布容器端口 %s，全局基线端点可能不通（compose 需加 ports 映射）", ep.Service, ep.Port)
		}
	}
	if ep.From == "host" {
		return b.hostAddr(hostID), nil
	}
	return joinURL(ep, hostPart), nil
}

func (b *builder) warnf(format string, args ...any) {
	b.warnings = append(b.warnings, fmt.Sprintf(format, args...))
}

// 组件配置段标记（与 deliver-studio 引擎写出口径一致："---  # 组件 <code> 配置段"）
const (
	compSectionPrefix = "---  # 组件 "
	compSectionSuffix = " 配置段"
)

// stripSkippedSections 配置模板剔除被跳过组件的配置段：打包引擎以标记行为界逐组件追加 YAML 文档，
// 跳过组件的段整段移除——对应能力回落单元模板基线的保守默认（OFF），端点/凭据亦不再参与渲染
func stripSkippedSections(tpl string, skip map[string]bool) string {
	if len(skip) == 0 || !strings.Contains(tpl, compSectionPrefix) {
		return tpl
	}
	lines := strings.Split(tpl, "\n")
	out := make([]string, 0, len(lines))
	dropping := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, compSectionPrefix) {
			code := strings.TrimSuffix(strings.TrimPrefix(ln, compSectionPrefix), compSectionSuffix)
			dropping = skip[code]
			if dropping {
				continue
			}
		}
		if !dropping {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// materialize 逐主机物化：compose 切分 / 配置渲染 / systemd unit / load 清单 / schema 节点
func (b *builder) materialize() error {
	pp := b.pp
	// 服务按主机分组（保持 compose 原顺序）
	for _, svc := range pp.Order {
		hostID, ok := b.svcHost[svc]
		if !ok {
			continue
		}
		if b.svcMode[svc] == "systemd" {
			b.sysByHost[hostID] = append(b.sysByHost[hostID], svc)
		} else {
			b.dockerByHost[hostID] = append(b.dockerByHost[hostID], svc)
			b.dockerSvcs[svc] = true
		}
	}
	// 哪些主机跑应用单元（消费配置）
	unitHosts := map[int64]bool{}
	unitBySvc := map[string]string{}
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			unitBySvc[s] = u.Code
			if hostID, ok := b.svcHost[s]; ok {
				unitHosts[hostID] = true
			}
		}
	}
	// 期望态配置：服务级配置文件集渲染（format:3 单模板等价兼容 / format:4 单元多文件与角色文件）
	// + 配置端点跨机引用收集（desired.go；替代旧「主机级单文件」渲染）
	if err := b.assignConfBySvc(); err != nil {
		return err
	}
	// load 文件清单：镜像 tar（docker 主机）+ 制品（systemd 主机）+ delivery 载荷（入口主机）+ extras SQL（schema 主机）
	fileIndex := map[string]pkgFile{}
	for _, f := range pp.Row.Files {
		fileIndex[f.Path] = f
	}
	spec := func(path, dest string) (FileSpec, bool) {
		f, ok := fileIndex[path]
		if !ok {
			return FileSpec{}, false
		}
		return FileSpec{Path: path, SHA256: f.SHA256, Size: f.Size, Dest: dest}, true
	}
	appendFiles := func(hostID int64, files ...FileSpec) {
		b.filesByHost[hostID] = append(b.filesByHost[hostID], files...)
	}
	for hostID, svcs := range b.dockerByHost {
		var specs []FileSpec
		for _, svc := range svcs {
			ref := b.resolvedImageRef(svc)
			if ref == "" {
				continue
			}
			if s, ok := spec("images/"+safeImageTarName(ref), ""); ok {
				specs = append(specs, s)
			} else {
				return fmt.Errorf("服务 %s 的镜像 %s 不在包内 images/（引用可能含未解析变量）", svc, ref)
			}
		}
		appendFiles(hostID, dedupFiles(specs)...)
	}
	for hostID, svcs := range b.sysByHost {
		var specs []FileSpec
		for _, svc := range svcs {
			code := unitBySvc[svc]
			for _, u := range pp.Decl.Units {
				if u.Code == code && u.Artifact != "" {
					s, ok := spec(artifactsDir+"/"+u.Artifact, "")
					if !ok {
						return fmt.Errorf("单元 %s 的制品 %s 不在包内 %s/", code, u.Artifact, artifactsDir)
					}
					specs = append(specs, s)
				}
			}
		}
		appendFiles(hostID, dedupFiles(specs)...)
	}
	// delivery 载荷 → 入口单元所在主机（拷入 data/conf/，应用启动自动导入消费）
	if entryHost, ok := b.svcHost[pp.Decl.Entry.Service]; ok {
		var specs []FileSpec
		for _, f := range pp.Row.Files {
			if strings.HasPrefix(f.Path, deliveryDir+"/") && !strings.HasSuffix(f.Path, "/"+varsEnvFile) && f.Path != deliveryDir+"/"+varsEnvFile {
				specs = append(specs, FileSpec{Path: f.Path, SHA256: f.SHA256, Size: f.Size,
					Dest: "data/conf/" + strings.TrimPrefix(f.Path, deliveryDir+"/")})
			}
		}
		appendFiles(entryHost, dedupFiles(specs)...)
	}
	// schema（外部组件 SQL 预置）：执行主机=首个跑应用单元的 docker 主机，extras/SQL 随其 load
	b.schemaHosts = map[int64]bool{}
	for _, c := range pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue // 跳过组件：不装也不连，SQL 预置无意义
		}
		s := c.Schema
		if s == nil || s.Dir == "" || s.Image == "" {
			continue
		}
		if _, placed := b.svcHost[firstServiceOf(c)]; placed {
			continue // 组件落机：initdb 挂载覆盖，无需一次性容器
		}
		execHost := int64(0)
		for _, svc := range pp.Order {
			if unitBySvc[svc] != "" && b.svcMode[svc] == "docker" {
				if h, ok := b.svcHost[svc]; ok {
					execHost = h
					break
				}
			}
		}
		if execHost == 0 {
			b.warnf("组件 %s 声明了 SQL 预置但无 docker 应用主机可执行，已跳过（应用结构自足化兜底）", c.Code)
			continue
		}
		// schema 连接参数是端点/凭据占位符：端点值按执行主机视角解析（外部=手填、跨机=地址:端口）
		epVals := map[string]string{}
		for _, ep := range c.Endpoints {
			v, err := b.endpointValueFor(ep, execHost)
			if err != nil {
				return err
			}
			epVals[ep.Placeholder] = v
		}
		if epVals[s.Host] == "" {
			b.warnf("组件 %s SQL 预置连接参数 %s 未赋值，已跳过", c.Code, s.Host)
			continue
		}
		var sqlSpecs []FileSpec
		for _, f := range pp.Row.Files {
			if strings.HasPrefix(f.Path, strings.TrimSuffix(s.Dir, "/")+"/") && strings.HasSuffix(f.Path, ".sql") {
				sqlSpecs = append(sqlSpecs, FileSpec{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
			}
		}
		if len(sqlSpecs) == 0 {
			b.warnf("组件 %s SQL 目录 %s 无 *.sql，跳过预置", c.Code, s.Dir)
			continue
		}
		appendFiles(execHost, dedupFiles(sqlSpecs)...)
		b.schemaHosts[execHost] = true
		// 与旧安装器 applySchemas 同口径：一次性容器按字典序执行 *.sql
		connArgs := fmt.Sprintf("mysql -h%s -P%s -u%s", epVals[s.Host], orStr(epVals[s.Port], "3306"), orStr(b.vars[s.User], "root"))
		if s.Password != "" && b.vars[s.Password] != "" {
			connArgs += " -p" + b.vars[s.Password]
		}
		if s.Database != "" && b.vars[s.Database] != "" {
			connArgs += " " + b.vars[s.Database]
		}
		script := fmt.Sprintf(`docker run --rm -v "$(pwd)/%s:/sql:ro" %s sh -c 'for f in /sql/*.sql; do echo "应用 $f"; %s < "$f" || exit 1; done'`,
			s.Dir, s.Image, connArgs)
		payload, _ := json.Marshal(CmdPayload{App: pp.Decl.Name, Script: script, TimeoutSec: 900})
		b.schemaNode = &planNode{Key: "schema-" + c.Code, Kind: "cmd", Title: "SQL 预置（" + c.Code + "，一次性容器）",
			Host: execHost, Payload: payload, TimeoutSec: 900}
	}
	// 配置中心插件链（configCenter 声明组件）：plan → gate（人工审差异）→ apply → health
	for _, c := range pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue // 跳过组件：配置基线不发布（微服务形态 nacos 由 validate 硬校验不可跳过）
		}
		cc := c.ConfigCenter
		if cc == nil || len(cc.Configs) == 0 {
			continue
		}
		if err := b.materializeCC(c, cc, unitBySvc); err != nil {
			return err
		}
	}
	// systemd unit 物化
	for hostID, svcs := range b.sysByHost {
		_ = hostID
		for _, svc := range svcs {
			code := unitBySvc[svc]
			var unit *UnitDecl
			for i := range pp.Decl.Units {
				if pp.Decl.Units[i].Code == code {
					unit = &pp.Decl.Units[i]
				}
			}
			if unit == nil {
				return fmt.Errorf("systemd 服务 %s 找不到单元声明", svc)
			}
			confEnv := map[string]string{"APP_PORT": orStr(unit.Port, "8080")}
			for k, v := range unit.ConfEnv {
				confEnv[k] = v
			}
			var envLines []string
			for k, v := range confEnv {
				envLines = append(envLines, fmt.Sprintf("Environment=%s=%s", k, v))
			}
			sort.Strings(envLines)
			unitFile := fmt.Sprintf(`[Unit]
Description=%s %s（deliver-console）
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory={{APP_DIR}}
EnvironmentFile=-{{APP_DIR}}/.env
%s
ExecStart=java -jar {{APP_DIR}}/%s/%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, pp.Decl.Name, unit.Code, strings.Join(envLines, "\n"), artifactsDir, unit.Artifact)
			sp := SystemdPayload{App: pp.Decl.Name, Unit: fmt.Sprintf("%s-%s.service", sanitizeSeg(pp.Decl.Name), sanitizeSeg(unit.Code)),
				UnitFile: unitFile, EnvFile: b.envText,
				ConfigFiles: b.confBySvc[svc]}
			b.sysPayload[svc] = sp
		}
	}
	return nil
}

func (b *builder) resolvedImageRef(svc string) string {
	meta := b.pp.Meta[svc]
	if meta == nil {
		return ""
	}
	return resolveEnvRefs(meta.Image, b.vars)
}

// materializeCC 配置中心链物化：基线内容从包内读入嵌入 Items、连接参数按执行主机解析。
// 执行主机=组件落机主机（服务就近）；外部组件=首个单元主机（目标网络内可达，插件执行免 docker）。
// serverAddr 取宿主视角：同机=127.0.0.1:发布端口（插件进程在宿主 OS，compose 服务名仅容器网络内可解析）/
// 跨机=主机地址:发布端口 / 外部=手填。
func (b *builder) materializeCC(c ComponentDecl, cc *ConfigCenterDecl, unitBySvc map[string]string) error {
	pp := b.pp
	plugin := orStr(cc.Plugin, "nacos-config")
	var svc string
	var execHost int64
	if s := firstServiceOf(c); s != "" {
		if h, ok := b.svcHost[s]; ok {
			svc, execHost = s, h // 组件落机：链落组件主机（svc 非空=有前置编排依赖）
		}
	}
	if execHost == 0 {
		for _, s := range pp.Order {
			if unitBySvc[s] != "" {
				if h, ok := b.svcHost[s]; ok {
					execHost = h
					break
				}
			}
		}
	}
	if execHost == 0 {
		b.warnf("组件 %s 声明了配置中心但找不到可执行插件的主机（无单元分配），已跳过", c.Code)
		return nil
	}
	host := b.hosts[execHost]
	// serverAddr：组件首个端点按宿主视角解析
	if len(c.Endpoints) == 0 {
		b.warnf("组件 %s 声明了配置中心但无端点（serverAddr 无法解析），已跳过", c.Code)
		return nil
	}
	ep := c.Endpoints[0]
	var serverAddr string
	if _, placed := b.svcHost[ep.Service]; placed {
		hostPort := ep.Port
		if meta := pp.Meta[ep.Service]; meta != nil {
			if hp, ok := meta.Ports[ep.Port]; ok {
				hostPort = hp
			}
		}
		addr := "127.0.0.1"
		if b.svcHost[ep.Service] != execHost {
			addr = b.hostAddr(b.svcHost[ep.Service])
		}
		hostPart := addr + ":" + hostPort
		if ep.Scheme != "" {
			hostPart = ep.Scheme + "://" + hostPart
		}
		serverAddr = hostPart
	} else {
		v, ok := b.pl.Ext[ep.Placeholder]
		if !ok || v == "" {
			return fmt.Errorf("配置中心端点 %s（服务 %s）未落机且未手填外部地址——在「外部端点」表单补齐", ep.Placeholder, ep.Service)
		}
		serverAddr = v
	}
	args := map[string]string{"serverAddr": serverAddr, "namespace": orStr(cc.Namespace, "public")}
	for name, key := range map[string]string{"identityKey": cc.IdentityKey, "identityValue": cc.IdentityValue} {
		if key == "" {
			continue
		}
		v := b.vars[key]
		if v == "" {
			return fmt.Errorf("组件 %s 配置中心身份参数 %s 未解析到取值（对应变量未赋值）", c.Code, key)
		}
		args[name] = v
	}
	// 基线内容读入（文本 yaml，嵌入 payload——免二次文件分发）并按执行主机渲染安装期占位符：
	// Data ID 全局共享（所有消费服务读同一份），端点一律取「主机地址:发布端口」全局形态——
	// 服务名形态仅本机 compose 网络内有效，不能进全局基线（同机消费者经宿主发布端口访问同样可达）
	ccVals := map[string]string{}
	for k, v := range b.vars {
		ccVals[k] = v
	}
	for _, c := range pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue
		}
		for _, ep := range c.Endpoints {
			v, err := b.endpointValueGlobal(ep)
			if err != nil {
				return err
			}
			ccVals[ep.Placeholder] = v
		}
	}
	items := make([]PluginItem, 0, len(cc.Configs))
	for _, it := range cc.Configs {
		clean, err := safeRelPath(it.File)
		if err != nil {
			return fmt.Errorf("组件 %s 配置基线路径非法 %q: %w", c.Code, it.File, err)
		}
		data, err := os.ReadFile(filepath.Join(pp.PkgDir, filepath.FromSlash(clean)))
		if err != nil {
			return fmt.Errorf("组件 %s 配置基线 %s 读取失败: %w", c.Code, it.File, err)
		}
		rendered, unknown := renderPlaceholders(string(data), ccVals)
		if len(unknown) > 0 {
			return fmt.Errorf("组件 %s 配置基线 %s 存在未知占位符 %v（变量/端点/凭据均未覆盖）", c.Code, it.DataID, unknown)
		}
		items = append(items, PluginItem{DataID: it.DataID, Group: it.Group, Content: rendered})
	}
	sha := pluginSHA(plugin, host.Info.OS)
	base := PluginPayload{App: pp.Decl.Name, Plugin: plugin, OS: host.Info.OS, SHA256: sha,
		Config: args, TimeoutSec: 300}
	planP, applyP, healthP := base, base, base
	planP.Op, applyP.Op, healthP.Op = "plan", "apply", "health"
	applyP.Mode = "idempotent"
	planP.Items, applyP.Items = items, items
	ch := ccChain{code: c.Code, svc: svc, hostID: execHost}
	ch.nodes[0] = planNode{Key: "cc-plan-" + c.Code, Kind: "plugin", Title: "配置基线比对（" + c.Code + "，plan）",
		Host: execHost, Payload: mustJSON(planP), TimeoutSec: 300}
	ch.nodes[1] = planNode{Key: "gate-cc-" + c.Code, Kind: "gate",
		Title: "确认发布配置基线（" + c.Code + "）——先审阅 cc-plan 节点日志差异清单；apply 缺省幂等（仅写缺失项），覆盖线上差异请在 DAG 中把 apply 节点改为 force 模式"}
	ch.nodes[2] = planNode{Key: "cc-apply-" + c.Code, Kind: "plugin", Title: "发布配置基线（" + c.Code + "，apply）",
		Host: execHost, Payload: mustJSON(applyP), TimeoutSec: 300}
	ch.nodes[3] = planNode{Key: "cc-health-" + c.Code, Kind: "plugin", Title: "配置中心探活（" + c.Code + "）",
		Host: execHost, Payload: mustJSON(healthP), TimeoutSec: 120}
	b.ccChains = append(b.ccChains, ch)
	for _, s := range c.Services {
		b.ccApplyBySv[s] = ch.nodes[2].Key
	}
	return nil
}

func (b *builder) schemaConn(s *SchemaExec) string {
	return b.vars[s.Host]
}

func firstServiceOf(c ComponentDecl) string {
	if len(c.Services) > 0 {
		return c.Services[0]
	}
	return ""
}

// buildDag 默认 DAG：load → compose/systemd → 跨机依赖 wait 门控 → schema 门控 → 入口 wait
func (b *builder) buildDag() ([]planNode, error) {
	pp := b.pp
	var nodes []planNode
	unitBySvc := map[string]string{}
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			unitBySvc[s] = u.Code
		}
	}
	hostIDs := make([]int64, 0, len(b.hosts))
	for id := range b.hosts {
		hostIDs = append(hostIDs, id)
	}
	sort.Slice(hostIDs, func(i, j int) bool { return hostIDs[i] < hostIDs[j] })

	// compose 项目名（主机唯一）
	projOf := map[int64]string{}
	for _, id := range hostIDs {
		projOf[id] = fmt.Sprintf("%s-h%d", sanitizeSeg(pp.Decl.Name), id)
	}

	// 跨机依赖收集：消费服务 → 依赖目标（目标落机且不同主机 / 目标 systemd）
	crossDeps := map[string][]string{} // consumer svc → dep svc（需要显式 wait）
	for _, svc := range pp.Order {
		meta := pp.Meta[svc]
		if meta == nil {
			continue
		}
		for _, dep := range meta.Deps {
			depHost, placed := b.svcHost[dep]
			if !placed {
				continue // 外部依赖：已在位
			}
			if b.svcMode[svc] == "docker" && b.svcHost[svc] == depHost && b.svcMode[dep] == "docker" {
				continue // 同机 docker：compose depends_on 生效
			}
			crossDeps[svc] = append(crossDeps[svc], dep)
		}
	}
	// 配置端点跨机引用并入：消费方=该主机全部落机服务（配置为应用级共享渲染，无法归因到单一
	// 服务；偏保守但时序安全——app 先于 mysql 就绪启动会崩溃循环直至 wait 超时，实测会话 #32）
	for hostID, deps := range b.confCrossDeps {
		consumers := append(append([]string{}, b.dockerByHost[hostID]...), b.sysByHost[hostID]...)
		for _, svc := range consumers {
			for _, dep := range deps {
				if !slices.Contains(crossDeps[svc], dep) {
					crossDeps[svc] = append(crossDeps[svc], dep)
				}
			}
		}
	}

	// wait 节点：跨机依赖目标 + 入口服务
	needWait := map[string]bool{}
	for _, deps := range crossDeps {
		for _, d := range deps {
			needWait[d] = true
		}
	}
	// format:4 角色服务一律进入就绪等待集（即使无消费方 depends_on——角色就绪是初始化链与
	// 入口确认的前置）；探针类型由角色声明覆盖（缺省走 tcp/容器态启发式）
	for _, c := range pp.Decl.Components {
		for ri := range c.Roles {
			role := &c.Roles[ri]
			for _, svc := range role.Services {
				if _, placed := b.svcHost[svc]; !placed {
					continue
				}
				needWait[svc] = true
				b.roleBySvc[svc] = role
				if role.Readiness != nil {
					b.readinessProbe[svc] = role.Readiness
				}
			}
		}
	}
	// 角色脚本渲染（可失败步骤，提前于 waitNode/初始化链消费）：探针与初始化脚本共用同一值集
	roleScript := map[string]string{} // svc → 渲染后的 exec 探针脚本
	for svc, probe := range b.readinessProbe {
		if probe.Type != "exec" {
			continue
		}
		s, err := b.renderRoleScript(svc, probe.Script)
		if err != nil {
			return nil, err
		}
		roleScript[svc] = s
	}
	// 入口服务不在依赖 wait 集内：其最终确认由独立的 wait-entry 节点承担（防止与跨机依赖 wait 成环）
	waitNode := func(svc string) planNode {
		hostID := b.svcHost[svc]
		meta := pp.Meta[svc]
		title := "等待 " + svc + " 健康"
		var payload WaitPayload
		timeout := 600
		// format:4 角色探针优先（readinessProbe 对应物）：exec=容器内命令重试环（cmd 节点承载，
		// agent 协议零改动）；http/tcp=指定端口/路径的显式探测
		if probe := b.readinessProbe[svc]; probe != nil {
			switch probe.Type {
			case "exec":
				ctr := fmt.Sprintf("%s-%s-1", projOf[hostID], svc)
				script := fmt.Sprintf("for i in $(seq 1 120); do docker exec %s sh -c %s >/dev/null 2>&1 && exit 0; sleep 5; done\necho \"服务 %s 探针在 600s 内未就绪（exec 失败循环 120 次）\"\nexit 1",
					ctr, shellQuote(roleScript[svc]), svc)
				return planNode{Key: "wait-" + svc, Kind: "cmd", Title: title + "（exec 探针）", Host: hostID,
					Payload: mustJSON(CmdPayload{App: pp.Decl.Name, Script: script, TimeoutSec: timeout}),
					TimeoutSec: timeout}
			case "http":
				if port := orStr(probe.Port, publishedHostPort(meta)); port != "" {
					payload = WaitPayload{Type: "http", URL: "http://127.0.0.1:" + port + orStr(probe.Path, "/"), TimeoutSec: timeout}
				}
			case "tcp":
				if port := orStr(probe.Port, publishedHostPort(meta)); port != "" {
					payload = WaitPayload{Type: "tcp", Host: "127.0.0.1", Port: port, TimeoutSec: timeout}
				}
			}
			if payload.Type != "" {
				return planNode{Key: "wait-" + svc, Kind: "wait", Title: title, Host: hostID, TimeoutSec: timeout,
					Payload: mustJSON(payload)}
			}
			b.warnf("服务 %s 的探针无可用端口，退化为容器状态探测", svc)
		}
		// 单元健康路径优先（docker 需发布端口）
		var unit *UnitDecl
		if code := unitBySvc[svc]; code != "" {
			for i := range pp.Decl.Units {
				if pp.Decl.Units[i].Code == code {
					unit = &pp.Decl.Units[i]
				}
			}
		}
		if unit != nil && unit.HealthPath != "" && unit.Port != "" {
			port := unit.Port
			if meta != nil && b.svcMode[svc] == "docker" {
				if hp, ok := meta.Ports[unit.Port]; ok {
					port = hp
				} else {
					b.warnf("服务 %s 有健康路径但未发布端口 %s，健康等待退化为容器状态探测", svc, unit.Port)
					payload = WaitPayload{Type: "docker", Project: projOf[hostID], Service: svc, TimeoutSec: timeout}
					return planNode{Key: "wait-" + svc, Kind: "wait", Title: title, Host: hostID, TimeoutSec: timeout,
						Payload: mustJSON(payload)}
				}
			}
			payload = WaitPayload{Type: "http", URL: "http://127.0.0.1:" + port + unit.HealthPath, TimeoutSec: timeout}
		} else if b.svcMode[svc] == "systemd" {
			payload = WaitPayload{Type: "systemd", Unit: b.sysPayload[svc].Unit, TimeoutSec: timeout}
		} else if port := publishedHostPort(meta); port != "" {
			// TCP 就绪探测：无 HTTP 健康路径的组件（mysql/redis 等），端口开始监听 ≈ 初始化完成
			// 开始接受连接（官方镜像 entrypoint 先初始化数据目录后才开始监听），比容器 running 判定强一级
			payload = WaitPayload{Type: "tcp", Host: "127.0.0.1", Port: port, TimeoutSec: timeout}
		} else {
			if meta != nil && len(meta.Ports) == 0 {
				b.warnf("服务 %s 无健康路径且未发布端口，健康等待只能判容器运行态", svc)
			}
			payload = WaitPayload{Type: "docker", Project: projOf[hostID], Service: svc, TimeoutSec: timeout}
		}
		return planNode{Key: "wait-" + svc, Kind: "wait", Title: title, Host: hostID, TimeoutSec: timeout,
			Payload: mustJSON(payload)}
	}
	waitKeys := map[string]string{} // svc → node key
	for svc := range needWait {
		if _, placed := b.svcHost[svc]; !placed {
			continue
		}
		n := waitNode(svc)
		nodes = append(nodes, n)
		waitKeys[svc] = n.Key
	}

	// 同机 docker 依赖就绪分层：被本机其他服务 depends_on 的服务（base）先行 up 并等健康，其余
	//（rest）后 up。depends_on short syntax 只保证启动顺序（mysql 容器 running ≠ 可连接），
	// 就绪顺序需显式 wait 门控；两层覆盖「组件→单元」主场景，更深的链 base 段内部仍由 compose
	// depends_on 启动序 + 应用重启自愈兜底（ADR-4 边界）。分层 wait 登记 waitKeys：cc 链与
	// 跨机门控逻辑天然复用；「既是跨机依赖目标又是本机 base」时改写既有 wait 的 Deps 挂到 base 段。
	type hostSplit struct{ base, rest []string }
	splits := map[int64]*hostSplit{}
	baseWaits := map[int64][]string{} // hostID → base 服务 wait key 列表（rest compose 依赖）
	for _, hostID := range hostIDs {
		svcs := b.dockerByHost[hostID]
		if len(svcs) < 2 {
			continue
		}
		inHost := map[string]bool{}
		for _, s := range svcs {
			inHost[s] = true
		}
		baseSet := map[string]bool{}
		for _, svc := range svcs {
			if meta := pp.Meta[svc]; meta != nil {
				for _, dep := range meta.Deps {
					if dep != svc && inHost[dep] {
						baseSet[dep] = true
					}
				}
			}
		}
		var sp hostSplit
		for _, s := range svcs {
			if baseSet[s] {
				sp.base = append(sp.base, s)
			} else {
				sp.rest = append(sp.rest, s)
			}
		}
		if len(sp.base) == 0 || len(sp.rest) == 0 {
			continue
		}
		splits[hostID] = &sp
		baseKey := fmt.Sprintf("compose-h%d-base", hostID)
		for _, svc := range sp.base {
			key := "wait-" + svc
			found := false
			for i := range nodes {
				if nodes[i].Key == key {
					nodes[i].Deps = []string{baseKey}
					found = true
					break
				}
			}
			if !found {
				n := waitNode(svc)
				n.Deps = []string{baseKey}
				nodes = append(nodes, n)
			}
			waitKeys[svc] = key
			baseWaits[hostID] = append(baseWaits[hostID], key)
		}
	}

	// schema 节点
	schemaKey := ""
	if b.schemaNode != nil {
		b.schemaNode.Deps = nil
		// schema 依赖：连接目标健康（外部组件无 wait 节点——操作员自行保证在位；保持无依赖）
		if b.needDocker[b.schemaNode.Host] { // 一次性容器依赖本机 docker
			b.schemaNode.Deps = append(b.schemaNode.Deps, dockerInstallKey(b.schemaNode.Host))
		}
		nodes = append(nodes, *b.schemaNode)
		schemaKey = b.schemaNode.Key
	}

	// 配置中心插件链（materialize 已生成节点；前置=组件服务健康：有 wait 节点用 wait，否则其主机 compose）
	for _, ch := range b.ccChains {
		plan, gate, apply, health := ch.nodes[0], ch.nodes[1], ch.nodes[2], ch.nodes[3]
		if ch.svc != "" {
			if k, ok := waitKeys[ch.svc]; ok {
				plan.Deps = []string{k}
			} else {
				plan.Deps = []string{fmt.Sprintf("compose-h%d", ch.hostID)}
			}
		}
		gate.Deps = []string{plan.Key}
		apply.Deps = []string{gate.Key}
		health.Deps = []string{apply.Key}
		nodes = append(nodes, plan, gate, apply, health)
	}
	// 消费方后置：依赖配置中心组件服务的单元编排节点排在 cc-apply 之后（配置就绪才准应用启动；
	// 同机同 compose 项目的单元无法拆序——由 compose depends_on healthcheck + 应用重启自愈兜底，ADR-4 边界）
	ccDepsFor := func(svcs []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, svc := range svcs {
			for _, dep := range crossDeps[svc] {
				if key, ok := b.ccApplyBySv[dep]; ok && !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
			}
		}
		return out
	}

	// 主机级 load / compose 节点
	for _, hostID := range hostIDs {
		files := b.filesByHost[hostID]
		dockerSvcs := b.dockerByHost[hostID]
		sysSvcs := b.sysByHost[hostID]
		var loadDeps []string
		if b.needDocker[hostID] { // 无 docker 主机：装载前先装 docker（docker load 依赖 dockerd）
			nodes = append(nodes, buildDockerNode(b.hosts[hostID], b.dockerOpts, "", false))
			loadDeps = []string{dockerInstallKey(hostID)}
		}
		if len(files) > 0 {
			payload := mustJSON(LoadPayload{App: pp.Decl.Name, Files: files})
			nodes = append(nodes, planNode{Key: fmt.Sprintf("load-h%d", hostID), Kind: "load",
				Title:   fmt.Sprintf("%s：装载镜像/制品（%d 个文件）", b.hosts[hostID].Name, len(files)),
				Host:    hostID,
				Payload: payload, TimeoutSec: 3600, // 多 GB 镜像慢链路下载+docker load 放宽
				Deps: loadDeps})
		}
		if len(dockerSvcs) > 0 {
			// 悬空依赖剥离：depends_on 指向本机之外的服务（跨机/外部/systemd）时删掉该条目，
			// 否则 docker compose up 报 undefined service；顺序保证已由跨机 wait / 就绪分层承担
			inHost := map[string]bool{}
			for _, svc := range dockerSvcs {
				inHost[svc] = true
			}
			var blocks strings.Builder
			for _, svc := range dockerSvcs {
				blk := stripDanglingDeps(pp.Meta[svc].Block, func(dep string) bool { return inHost[dep] })
				if h := confHashOf(b.confBySvc[svc]); h != "" {
					// 配置指纹注入（K8s checksum annotation 模式，服务级）：compose 感知不到 bind 内容变化，
					// label 值随本服务配置文件集变 → up 判定配置变更 recreate 该容器，新配置真正生效。
					// 注意：invDetails 的 blockHash 基于包内原文块，不含本注入——增量基线不因注入漂移
					blk = injectConfLabel(blk, h)
				}
				// 资源限额注入（K8s resources.limits 对应物）：placement.resources 覆盖 > 包内组件缺省；
				// resHash 进 inventory detail（不含进 blockHash——改限额经 resHash 精确 diff）
				blk = injectResourceLimits(blk, b.resSpecOf(svc))
				blocks.WriteString(strings.TrimRight(blk, "\n") + "\n")
			}
			composeText := "name: " + projOf[hostID] + "\nservices:\n" + blocks.String()
			// 服务级配置文件并集下发（format:3=同机各单元服务共享同一份 data/conf/application.yaml；
			// format:4=各服务/角色各自的文件——同路径不同内容即报错，防静默错配）
			files2 := map[string]string{}
			for _, svc := range dockerSvcs {
				for path, content := range b.confBySvc[svc] {
					if prev, dup := files2[path]; dup && prev != content {
						return nil, fmt.Errorf("主机 %s 上服务 %s 的配置文件 %s 与其他服务同路径不同内容（format:4 需在声明中按服务区分路径）",
							b.hosts[hostID].Name, svc, path)
					}
					files2[path] = content
				}
			}
			mkPayload := func(svcs []string) json.RawMessage {
				if len(svcs) == 0 {
					svcs = dockerSvcs // 空=全部（up 子集语义对齐）
				}
				return mustJSON(ComposePayload{App: pp.Decl.Name, Project: projOf[hostID],
					Compose: composeText, Env: b.envText, Files: files2, Services: svcs,
					Ports: b.svcHostPorts(svcs)})
			}
			mkDeps := func(svcs []string) (out []string) {
				out = append(out, fmt.Sprintf("load-h%d", hostID))
				out = append(out, b.gateDeps(svcs, crossDeps, waitKeys)...)
				out = append(out, ccDepsFor(svcs)...)
				if schemaKey != "" && b.hostHasUnit(svcs, unitBySvc) {
					out = append(out, schemaKey)
				}
				return out
			}
			if sp := splits[hostID]; sp != nil {
				// base 段：基础服务先行 up（就绪等待由分层 wait 节点承担）
				baseNode := planNode{Key: fmt.Sprintf("compose-h%d-base", hostID), Kind: "compose",
					Title: fmt.Sprintf("%s：docker 编排 up（基础层 %d：%s）", b.hosts[hostID].Name, len(sp.base), strings.Join(sp.base, ", ")),
					Host:  hostID, Payload: mkPayload(sp.base), TimeoutSec: 1800,
					InvCompose: sp.base, InvDetail: b.invDetails(sp.base, nil)}
				baseNode.Deps = mkDeps(sp.base)
				nodes = append(nodes, baseNode)
				node := planNode{Key: fmt.Sprintf("compose-h%d", hostID), Kind: "compose",
					Title: fmt.Sprintf("%s：docker 编排 up（%d 个服务）", b.hosts[hostID].Name, len(sp.rest)),
					Host:  hostID, Payload: mkPayload(sp.rest), TimeoutSec: 1800,
					InvCompose: sp.rest, InvDetail: b.invDetails(sp.rest, nil)}
				node.Deps = append(node.Deps, fmt.Sprintf("load-h%d", hostID))
				node.Deps = append(node.Deps, baseWaits[hostID]...) // base 全部健康后才 up 消费层
				node.Deps = append(node.Deps, b.gateDeps(sp.rest, crossDeps, waitKeys)...)
				node.Deps = append(node.Deps, ccDepsFor(sp.rest)...)
				if schemaKey != "" && b.hostHasUnit(sp.rest, unitBySvc) {
					node.Deps = append(node.Deps, schemaKey)
				}
				nodes = append(nodes, node)
			} else {
				node := planNode{Key: fmt.Sprintf("compose-h%d", hostID), Kind: "compose",
					Title: fmt.Sprintf("%s：docker 编排 up（%d 个服务）", b.hosts[hostID].Name, len(dockerSvcs)),
					Host:  hostID, Payload: mkPayload(nil), TimeoutSec: 1800,
					InvCompose: dockerSvcs, InvDetail: b.invDetails(dockerSvcs, nil)}
				node.Deps = mkDeps(dockerSvcs)
				nodes = append(nodes, node)
			}
		}
		for _, svc := range sysSvcs {
			sp := b.sysPayload[svc]
			payload := mustJSON(sp)
			node := planNode{Key: "sys-" + svc, Kind: "systemd",
				Title: fmt.Sprintf("%s：systemd 部署 %s", b.hosts[hostID].Name, svc),
				Host:  hostID, Payload: payload, InvSystemd: svc,
				InvDetail: b.invDetails(nil, []string{svc})}
			node.Deps = append(node.Deps, fmt.Sprintf("load-h%d", hostID))
			node.Deps = append(node.Deps, b.gateDeps([]string{svc}, crossDeps, waitKeys)...)
			node.Deps = append(node.Deps, ccDepsFor([]string{svc})...)
			if schemaKey != "" {
				node.Deps = append(node.Deps, schemaKey)
			}
			nodes = append(nodes, node)
		}
	}

	// format:4 角色初始化链（K8s Job 对应物）：角色服务就绪后在其所在主机 docker exec 进容器执行
	// 幂等脚本（如 mysql 建复制账号 / CHANGE REPLICATION SOURCE）。链内按声明顺序串行；
	// After=<角色名> 跨角色接线（replica 的 join 等 primary 的建号步骤完成）。
	for _, c := range pp.Decl.Components {
		for ri := range c.Roles {
			role := &c.Roles[ri]
			placedSvcs := []string{}
			roleWaitKeys := []string{}
			for _, svc := range role.Services {
				if hostID, ok := b.svcHost[svc]; ok {
					placedSvcs = append(placedSvcs, svc)
					_ = hostID
					if k, ok := waitKeys[svc]; ok {
						roleWaitKeys = append(roleWaitKeys, k)
					}
				}
			}
			if len(placedSvcs) == 0 || len(role.Init) == 0 {
				continue
			}
			primarySvc := placedSvcs[0] // 角色首服务（执行宿主与容器名口径）
			primaryHost := b.svcHost[primarySvc]
			ctr := fmt.Sprintf("%s-%s-1", projOf[primaryHost], primarySvc)
			prevKey := ""
			for _, st := range role.Init {
				key := fmt.Sprintf("init-%s-%s-%s", c.Code, role.Name, st.ID)
				script, err := b.renderRoleScript(primarySvc, st.Script)
				if err != nil {
					return nil, fmt.Errorf("组件 %s 角色 %s 初始化步骤 %s：%w", c.Code, role.Name, st.ID, err)
				}
				deps := append([]string{}, roleWaitKeys...)
				if prevKey != "" {
					deps = append(deps, prevKey)
				}
				if st.After != "" && st.After != role.Name {
					if k, ok := b.roleInitLast[c.Code+":"+st.After]; ok {
						deps = append(deps, k)
					} else {
						return nil, fmt.Errorf("组件 %s 角色 %s 初始化步骤 %s 引用的角色 %s 未落机（After 依赖无法满足）",
							c.Code, role.Name, st.ID, st.After)
					}
				}
				timeout := st.TimeoutSec
				if timeout <= 0 {
					timeout = 300
				}
				nodes = append(nodes, planNode{Key: key, Kind: "cmd",
					Title: fmt.Sprintf("%s 初始化：%s/%s（%s，容器内执行）", b.hosts[primaryHost].Name, role.Name, st.ID, primarySvc),
					Host: primaryHost,
					Payload: mustJSON(CmdPayload{App: pp.Decl.Name,
						Script:     fmt.Sprintf("docker exec %s sh -c %s", ctr, shellQuote(script)),
						TimeoutSec: timeout}),
					TimeoutSec: timeout, Deps: deps})
				b.initSvcsByNode[key] = placedSvcs
				prevKey = key
			}
			b.roleInitLast[c.Code+":"+role.Name] = prevKey
		}
	}

	// 单元健康等待（多单元包·微服务形态）：每个有健康路径的非入口落机单元各生成 wait-unit-<svc>，
	// 挂在其所在主机的 rest compose 之后；入口 wait 最终依赖之——入口可达不再只证明自身，而是全单元就绪
	//（单包单后端时不产生新节点：入口单元即唯一健康单元，wait-entry 已覆盖，行为零回归）
	unitWaitKeys := []string{}
	for _, u := range pp.Decl.Units {
		for _, svc := range unitServices(u) {
			if u.HealthPath == "" || svc == pp.Decl.Entry.Service {
				continue
			}
			hostID, placed := b.svcHost[svc]
			if !placed {
				continue
			}
			n := waitNode(svc)
			n.Key = "wait-unit-" + svc
			n.Title = "单元就绪（" + svc + "）"
			depKey := fmt.Sprintf("compose-h%d", hostID)
			if sp := splits[hostID]; sp != nil {
				for _, s := range sp.base {
					if s == svc {
						depKey = fmt.Sprintf("compose-h%d-base", hostID)
						break
					}
				}
			}
			n.Deps = []string{depKey}
			nodes = append(nodes, n)
			unitWaitKeys = append(unitWaitKeys, n.Key)
		}
	}

	// 入口最终确认 wait-entry：独立于依赖 wait 节点，依赖全部服务节点与单元健康；format:4 再叠加
	// 角色初始化链末位节点与 exec 探针 wait（入口确认=复制链路已建立，非仅端口监听）
	if entrySvc := pp.Decl.Entry.Service; entrySvc != "" {
		if _, placed := b.svcHost[entrySvc]; placed {
			n := waitNode(entrySvc)
			n.Key = "wait-entry"
			n.Title = "确认入口可达（" + entrySvc + "）"
			n.Deps = unitWaitKeys
			for _, x := range nodes {
				if (x.Kind == "compose" || x.Kind == "systemd") && x.Key != n.Key {
					n.Deps = append(n.Deps, x.Key)
				}
			}
			for _, last := range b.roleInitLast {
				if last != "" {
					n.Deps = append(n.Deps, last)
				}
			}
			for svc, probe := range b.readinessProbe {
				if probe.Type == "exec" {
					if _, placed := b.svcHost[svc]; placed {
						n.Deps = append(n.Deps, "wait-"+svc)
					}
				}
			}
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// publishedHostPort 服务已发布的宿主端口（取数值最小者——主流组件单端口，主端口通常最小：
// 3306/6379/8848/9200；多端口组件为启发式）。未发布端口返回空
func publishedHostPort(meta *ServiceMeta) string {
	if meta == nil || len(meta.Ports) == 0 {
		return ""
	}
	best, bestN := "", -1
	for _, hp := range meta.Ports {
		n, err := strconv.Atoi(strings.Split(hp, ":")[0])
		if err != nil {
			continue
		}
		if bestN == -1 || n < bestN {
			best, bestN = strings.Split(hp, ":")[0], n
		}
	}
	return best
}

// gateDeps 消费服务集合的跨机依赖 wait 键（去重）
func (b *builder) gateDeps(svcs []string, crossDeps map[string][]string, waitKeys map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, svc := range svcs {
		for _, dep := range crossDeps[svc] {
			if key, ok := waitKeys[dep]; ok && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}

func (b *builder) hostHasUnit(svcs []string, unitBySvc map[string]string) bool {
	for _, svc := range svcs {
		if unitBySvc[svc] != "" {
			return true
		}
	}
	return false
}

// invDetails 生成 inventory detail（增量 diff 基线）：compose 服务=块哈希+镜像；systemd=unit 哈希。
// envHash/confHash 均为服务级（旧口径 env=.env 整串、conf=主机级渲染——改一变量全量重建的历史口径
// 已废弃；存量 detail 首次比对不等会触发一次幂等重装收敛）
func (b *builder) invDetails(dockerSvcs, sysSvcs []string) map[string]string {
	out := map[string]string{}
	for _, svc := range dockerSvcs {
		meta := b.pp.Meta[svc]
		if meta == nil {
			continue
		}
		detail := map[string]any{"kind": "compose", "blockHash": hashStr(meta.Block),
			"image": b.resolvedImageRef(svc), "envHash": b.svcEnvHash(svc)}
		if h := confHashOf(b.confBySvc[svc]); h != "" {
			detail["confHash"] = h
		}
		if res, ok := b.pl.Resources[svc]; ok && (res.Cpus != "" || res.MemMB > 0) {
			detail["resHash"] = hashStr(res.Cpus + "|" + fmt.Sprint(res.MemMB))
		} else if r := b.resSpecOf(svc); r.Cpus != "" || r.MemMB > 0 {
			detail["resHash"] = hashStr(r.Cpus + "|" + fmt.Sprint(r.MemMB))
		}
		out[svc] = string(mustJSON(detail))
	}
	for _, svc := range sysSvcs {
		sp := b.sysPayload[svc]
		var artifactSha string
		for _, f := range b.pp.Row.Files {
			if strings.HasPrefix(f.Path, artifactsDir+"/") && strings.HasSuffix(f.Path, artifactOf(b.pp, svc)) {
				artifactSha = f.SHA256
			}
		}
		detail := map[string]any{"kind": "systemd", "unitHash": hashStr(sp.UnitFile),
			"artifactSha": artifactSha, "envHash": b.svcEnvHash(svc)}
		if h := confHashOf(b.confBySvc[svc]); h != "" {
			detail["confHash"] = h
		}
		out[svc] = string(mustJSON(detail))
	}
	return out
}

func artifactOf(pp *ParsedPackage, svc string) string {
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			if s == svc {
				return u.Artifact
			}
		}
	}
	return ""
}

// resSpecOf 服务资源限额（K8s resources 对应物）：placement.resources 逐服务覆盖优先，
// 包内组件缺省声明兜底（studio 素材侧缺省，ADR-11）
func (b *builder) resSpecOf(svc string) ResourceSpec {
	if r, ok := b.pl.Resources[svc]; ok && (r.Cpus != "" || r.MemMB > 0) {
		return r
	}
	for _, c := range b.pp.Decl.Components {
		if c.Resources == nil {
			continue
		}
		for _, s := range c.Services {
			if s == svc {
				return *c.Resources
			}
		}
	}
	return ResourceSpec{}
}

// svcHostPorts 服务子集的宿主发布端口（升序去重）——随 ComposePayload 下发，agent 执行前自检用
func (b *builder) svcHostPorts(svcs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, svc := range svcs {
		for _, hp := range b.pp.Meta[svc].Ports { // 容器端口 → 宿主端口，值即宿主端口
			if hp != "" && !seen[hp] {
				seen[hp] = true
				out = append(out, hp)
			}
		}
	}
	sort.Strings(out)
	return out
}

// precheckPorts 建会话端口硬校验（运行前拦截，而非等 compose up 报 port is already allocated）：
// 每台主机 compose 子集的宿主发布端口 × 主机容器端口档案（agent 心跳上报；本机节点由 console
// 进程内即时探测）。归因与 agent ports.go 同口径：本项目容器占用=重装/升级复用（仅提示）；
// 异源容器占用=拒绝建会话（明细带占用者）；档案缺失（旧 agent/心跳未达/无 docker）=降级放行
// （不能因数据缺失阻断安装——执行期 agent 侧自检兜底，非容器进程占用也在那一层才拦）。
func (b *builder) precheckPorts(nodes []planNode) error {
	type hostCheck struct {
		name    string
		project string
		ports   map[string]bool
	}
	byHost := map[int64]*hostCheck{}
	for _, n := range nodes {
		if n.Kind != "compose" {
			continue
		}
		var p ComposePayload
		if json.Unmarshal(n.Payload, &p) != nil || len(p.Ports) == 0 {
			continue
		}
		hc := byHost[n.Host]
		if hc == nil {
			h := b.hosts[n.Host]
			if h == nil {
				continue
			}
			hc = &hostCheck{name: h.Name, project: p.Project, ports: map[string]bool{}}
			byHost[n.Host] = hc
		}
		for _, port := range p.Ports {
			hc.ports[port] = true
		}
	}
	if len(byHost) == 0 {
		return nil
	}
	var ids []int64
	for id := range byHost {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	appSeg := sanitizeSeg(b.pp.Decl.Name)
	var conflicts, reused, nodata []string
	for _, id := range ids {
		hc := byHost[id]
		h := b.hosts[id]
		var cs []ContainerPort
		switch {
		case h.Local:
			if !h.Info.Docker {
				continue // 本机档案无 docker（未装/档案未刷新）：无从预检，执行期 localCompose 自检兜底
			}
			cs = localContainerPorts() // 本机节点：console 进程内即时探测，档案恒新鲜
		case h.Info.Docker:
			cs = h.Info.Containers
			if cs == nil {
				nodata = append(nodata, fmt.Sprintf("主机 %s：暂无容器端口档案（agent 版本较旧或心跳未达），跳过端口预检——执行期 agent 侧自检兜底", hc.name))
				continue
			}
		default:
			continue // 无 docker 主机：DockerInstall 开启时装完才有容器，无从预检
		}
		var ports []string
		for p := range hc.ports {
			ports = append(ports, p)
		}
		sort.Strings(ports)
		for _, port := range ports {
			for _, c := range cs {
				match := false
				for _, hp := range c.HostPorts {
					if hp == port {
						match = true
						break
					}
				}
				if !match {
					continue
				}
				switch {
				case hc.project != "" && c.Project == hc.project:
					// 本项目容器：升级/重装的正常起点（apply 接管语义）——原地重建变更服务，数据保留
					reused = append(reused, fmt.Sprintf("主机 %s 端口 %s：已有本项目部署容器 %s——本次安装将原地升级重建变更服务，数据保留", hc.name, port, c.Name))
				case c.Project != "" && strings.HasPrefix(c.Project, appSeg+"-h"):
					// 同包历史项目（旧主机分配/包名变更前遗留）：up 必撞端口，拒绝并给清理指引
					conflicts = append(conflicts, fmt.Sprintf("主机 %s 端口 %s 被本应用历史部署容器 %s（项目 %s，镜像 %s）占用——多为旧主机分配/包名变更前的遗留，可清理后安装（bind 挂数据保留在主机 data/ 目录）或调整分配", hc.name, port, c.Name, c.Project, c.Image))
				default:
					conflicts = append(conflicts, fmt.Sprintf("主机 %s 端口 %s 已被容器 %s（镜像 %s）占用——请调整服务分配或清理占用容器后重试", hc.name, port, c.Name, c.Image))
				}
				break // 一个端口一个占用者归因即止
			}
		}
	}
	for _, m := range reused {
		b.warnings = append(b.warnings, m)
	}
	for _, m := range nodata {
		b.warnings = append(b.warnings, m)
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("宿主端口冲突，安装未发起：%s", strings.Join(conflicts, "；"))
	}
	return nil
}

// evictNodes 迁移驱逐节点生成（K8s drain 对应物）：服务换宿主（inventory 记 A、本次分到 B）→
// 旧主机 A 注入 cleanup 节点按 compose v2/v1 双命名精确清容器（--remove-orphans 只清同项目，
// 跨项目 demo-hA 永久孤儿）；成功后回写器删旧行。incremental 与 overwrite 共用。
func (b *builder) evictNodes(changes []svcChange) []planNode {
	invMap := map[string]invRow{}
	for _, r := range b.inv {
		if _, ok := invMap[r.Service]; !ok {
			invMap[r.Service] = r
		}
	}
	evict := map[int64][]string{} // 旧主机 → 迁走的服务
	for _, c := range changes {
		if c.Action != "update" {
			continue
		}
		if row, ok := invMap[c.Service]; ok && row.HostID != c.HostID {
			evict[row.HostID] = append(evict[row.HostID], c.Service)
		}
	}
	if len(evict) == 0 {
		return nil
	}
	var ids []int64
	for id := range evict {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	appSeg := sanitizeSeg(b.pp.Decl.Name)
	var out []planNode
	for _, id := range ids {
		h := b.hosts[id]
		if h == nil {
			continue
		}
		proj := fmt.Sprintf("%s-h%d", appSeg, id)
		var script strings.Builder
		for _, svc := range evict[id] {
			fmt.Fprintf(&script, "docker rm -f %s-%s-1\n", proj, svc) // compose v2 命名
			fmt.Fprintf(&script, "docker rm -f %s-%s_1\n", proj, svc) // compose v1 命名
		}
		script.WriteString("exit 0\n") // 容器名不存在时 rm 非零退出=正常，统一判成功（sh/PowerShell 均兼容）
		out = append(out, planNode{Key: fmt.Sprintf("cleanup-h%d", id), Kind: "cmd",
			Title: fmt.Sprintf("%s：清理已迁走的旧部署（%s，项目 %s）", h.Name, strings.Join(evict[id], ", "), proj),
			Host:  id, Payload: mustJSON(CmdPayload{App: b.pp.Decl.Name, Script: script.String(), TimeoutSec: 300}),
			TimeoutSec: 300, InvEvictHost: id, InvEvictSvcs: evict[id]})
	}
	return out
}

// appendEvictions overwrite 模式的驱逐追加（全量节点保留，仅补迁移清理）
func (b *builder) appendEvictions(nodes []planNode, changes []svcChange) []planNode {
	return append(nodes, b.evictNodes(changes)...)
}

// diffFilter 增量裁剪（apply 语义：仅变更项入计划）。驱动=统一 diff（svcChanges，与预览同源）：
// 变更服务所在主机的 load/compose/systemd 节点保留，未变更主机整组剔除（load 幂等跳过已存在文件）；
// 角色初始化节点随角色服务变更保留（角色零变更时重放无意义——容器未动复制已建立）；
// exec 探针 wait 同理。反亲和组件（SpreadHosts）升级序「先从后主」：主角色（首个声明角色）编排
// 后置于其余角色就绪——从库先升级就绪再动主库，复制中断窗口最小（StatefulSet 滚动对应物）。
func (b *builder) diffFilter(nodes []planNode, changes []svcChange) []planNode {
	changedHost := map[int64]bool{}
	changedSvc := map[string]bool{}
	for _, c := range changes {
		if !c.changed() {
			continue
		}
		changedSvc[c.Service] = true
		changedHost[c.HostID] = true // create/update=新宿主安装；remove=旧宿主 up --remove-orphans 清理
	}
	nodes = append(nodes, b.evictNodes(changes)...)
	var kept []planNode
	dropped := map[string]bool{}
	for _, n := range nodes {
		switch n.Kind {
		case "load", "compose":
			if len(b.filesByHost[n.Host]) == 0 && n.Kind == "load" {
				continue // 无文件节点本就不存在
			}
			if !changedHost[n.Host] {
				dropped[n.Key] = true
				continue
			}
		case "systemd":
			if !changedHost[n.Host] {
				dropped[n.Key] = true
				continue
			}
		case "cmd":
			// 角色初始化节点：角色服务零变更 → 裁剪（容器未重建，幂等脚本重放无意义）
			if svcs, ok := b.initSvcsByNode[n.Key]; ok {
				roleChanged := false
				for _, s := range svcs {
					if changedSvc[s] {
						roleChanged = true
						break
					}
				}
				if !roleChanged {
					dropped[n.Key] = true
					continue
				}
			}
			// exec 探针 wait（key=wait-<svc>、kind=cmd）：角色服务零变更 → 裁剪
			if svc := strings.TrimPrefix(n.Key, "wait-"); svc != n.Key && b.readinessProbe[svc] != nil {
				if !changedSvc[svc] {
					dropped[n.Key] = true
					continue
				}
			}
		}
		kept = append(kept, n)
	}
	// 反亲和组件升级序（先从后主）：主角色与其余角色的宿主编排均保留时接线
	keptIdx := nodeIndex(kept)
	for _, c := range b.pp.Decl.Components {
		if !c.SpreadHosts || len(c.Roles) < 2 {
			continue
		}
		primarySvcs := placedServices(b, c.Roles[0].Services)
		if len(primarySvcs) == 0 {
			continue
		}
		primaryChanged := false
		for _, s := range primarySvcs {
			if changedSvc[s] {
				primaryChanged = true
			}
		}
		if !primaryChanged {
			continue
		}
		primaryHost := b.svcHost[primarySvcs[0]]
		for _, r := range c.Roles[1:] {
			for _, svc := range placedServices(b, r.Services) {
				if b.svcHost[svc] == primaryHost || !changedSvc[svc] {
					continue
				}
				if w, ok := keptIdx["wait-"+svc]; ok {
					for _, pk := range []string{fmt.Sprintf("compose-h%d", primaryHost), fmt.Sprintf("compose-h%d-base", primaryHost), fmt.Sprintf("compose-h%d-rest", primaryHost)} {
						if pn, ok2 := keptIdx[pk]; ok2 && !containsStr(pn.Deps, w.Key) {
							pn.Deps = append(pn.Deps, w.Key)
						}
					}
				}
			}
		}
	}
	// 剔除「仅被剔除节点引用」的 wait 节点（wait-entry 除外——最终确认恒保留）；
	// 配置中心插件链（plugin/gate）增量恒保留——基线差异不在 inventory 哈希内，靠 plan 重新比对发现
	hasWork := false
	for _, n := range kept {
		if n.Kind == "compose" || n.Kind == "systemd" || n.Kind == "load" || n.Kind == "cmd" || n.Kind == "plugin" || n.Kind == "docker" {
			hasWork = true
			break
		}
	}
	if !hasWork {
		return nil
	}
	var out []planNode
	for _, n := range kept {
		if n.Kind == "wait" && n.Key != "wait-entry" {
			referenced := false
			for _, k := range kept {
				for _, d := range k.Deps {
					if d == n.Key {
						referenced = true
					}
				}
			}
			if !referenced {
				continue
			}
		}
		// 清理指向被剔除节点的依赖
		var deps []string
		for _, d := range n.Deps {
			if !dropped[d] {
				deps = append(deps, d)
			}
		}
		n.Deps = deps
		out = append(out, n)
	}
	return out
}

// placedServices 过滤出已落机的服务（保序）
func placedServices(b *builder, svcs []string) []string {
	var out []string
	for _, s := range svcs {
		if _, ok := b.svcHost[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// overlayDag 自定义 DAG 覆盖：自动类节点沿用引擎物化载荷（key 对齐），cmd/gate 按声明构造
func (b *builder) overlayDag(custom []DagNode, auto []planNode) ([]planNode, error) {
	autoMap := map[string]planNode{}
	for _, n := range auto {
		autoMap[n.Key] = n
	}
	var out []planNode
	for _, c := range custom {
		n := planNode{Key: c.ID, Kind: c.Kind, Title: c.Title, Host: c.Host, Deps: c.Deps}
		switch c.Kind {
		case "load", "compose", "systemd", "wait", "plugin":
			a, ok := autoMap[c.ID]
			if !ok {
				return nil, fmt.Errorf("自定义 DAG 节点 %s（%s）不在当前自动计划中（增量模式未变更项已剔除，或 key 不符）", c.ID, c.Kind)
			}
			n.Payload, n.TimeoutSec = a.Payload, a.TimeoutSec
			n.InvCompose, n.InvSystemd, n.InvDetail = a.InvCompose, a.InvSystemd, a.InvDetail
			if n.Title == "" {
				n.Title = a.Title
			}
			if n.Host == 0 {
				n.Host = a.Host
			}
			// plugin(apply)：自定义 DAG 可改发布模式（idempotent/force——覆盖线上差异）
			if c.Kind == "plugin" && c.Mode != "" {
				var p PluginPayload
				if json.Unmarshal(n.Payload, &p) == nil && p.Op == "apply" {
					if c.Mode != "idempotent" && c.Mode != "force" {
						return nil, fmt.Errorf("自定义 plugin 节点 %s 模式非法 %q（idempotent|force）", c.ID, c.Mode)
					}
					p.Mode = c.Mode
					n.Payload = mustJSON(p)
				}
			}
		case "cmd":
			if a, ok := autoMap[c.ID]; ok && c.Script == "" {
				// 自动类 cmd（schema SQL 预置）经 UI/导入往返丢脚本：key 对齐回用引擎物化载荷
				n.Payload, n.TimeoutSec = a.Payload, a.TimeoutSec
				if n.Title == "" {
					n.Title = a.Title
				}
				if n.Host == 0 {
					n.Host = a.Host
				}
			} else {
				if c.Host == 0 || c.Script == "" {
					return nil, fmt.Errorf("自定义 cmd 节点 %s 需要主机与脚本", c.ID)
				}
				n.Payload = mustJSON(CmdPayload{App: b.pp.Decl.Name, Script: c.Script, TimeoutSec: 900})
				n.TimeoutSec = 900
			}
		case "docker":
			// 安装 Docker 节点（自动注入或手动/导入添加）：载荷按当前安装源由引擎物化（与自动节点同源）
			if c.Host == 0 {
				return nil, fmt.Errorf("自定义 docker 节点 %s 需要主机", c.ID)
			}
			if h, ok := b.hosts[c.Host]; ok {
				dn := buildDockerNode(h, b.dockerOpts, "", false)
				n.Payload, n.TimeoutSec = dn.Payload, dn.TimeoutSec
				if n.Title == "" {
					n.Title = dn.Title
				}
			} else {
				return nil, fmt.Errorf("自定义 docker 节点 %s 主机 #%d 不存在", c.ID, c.Host)
			}
		case "toolkit":
			// 安装 docker-compose 节点（自动补装或手动/导入添加）：载荷按当前安装源物化（同源）
			if c.Host == 0 {
				return nil, fmt.Errorf("自定义 toolkit 节点 %s 需要主机", c.ID)
			}
			if h, ok := b.hosts[c.Host]; ok {
				tn := buildToolkitNode(h, b.dockerOpts, "", false)
				n.Payload, n.TimeoutSec = tn.Payload, tn.TimeoutSec
				if n.Title == "" {
					n.Title = tn.Title
				}
			} else {
				return nil, fmt.Errorf("自定义 toolkit 节点 %s 主机 #%d 不存在", c.ID, c.Host)
			}
		case "gate":
			n.Host = 0
			n.Title = orStr(c.Title, "人工确认") + orStr(c.Note, "")
		default:
			return nil, fmt.Errorf("自定义 DAG 节点 %s 类型未知 %q", c.ID, c.Kind)
		}
		out = append(out, n)
	}
	return out, nil
}

// validateDag 环检测 + 依赖存在性 + 主机有效性
func validateDag(nodes []planNode) error {
	if len(nodes) == 0 {
		return fmt.Errorf("执行计划为空")
	}
	ByKey := map[string]planNode{}
	for _, n := range nodes {
		if _, dup := ByKey[n.Key]; dup {
			return fmt.Errorf("DAG 节点 key 重复: %s", n.Key)
		}
		ByKey[n.Key] = n
	}
	for _, n := range nodes {
		for _, d := range n.Deps {
			if _, ok := ByKey[d]; !ok {
				return fmt.Errorf("节点 %s 依赖不存在的节点 %s", n.Key, d)
			}
		}
	}
	// 拓扑排序检测环
	state := map[string]int{} // 0=未访 1=在栈 2=完
	var visit func(key string) error
	visit = func(key string) error {
		switch state[key] {
		case 1:
			return fmt.Errorf("DAG 存在环（经 %s）", key)
		case 2:
			return nil
		}
		state[key] = 1
		for _, d := range ByKey[key].Deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[key] = 2
		return nil
	}
	for _, n := range nodes {
		if err := visit(n.Key); err != nil {
			return err
		}
	}
	return nil
}

// entryURL 入口访问地址（http://<入口主机地址>:<发布端口>）
func (b *builder) entryURL() string {
	svc := b.pp.Decl.Entry.Service
	hostID, ok := b.svcHost[svc]
	if !ok {
		return ""
	}
	port := orStr(b.pp.Decl.Entry.Port, "8080")
	if meta := b.pp.Meta[svc]; meta != nil {
		if hp, okp := meta.Ports[port]; okp {
			port = hp
		}
	}
	return "http://" + b.hostAddr(hostID) + ":" + port
}

// invRows 成功后回写 inventory（compose 节点级 + systemd 节点级）
func (b *builder) invRows(nodes []planNode) []invRow {
	var rows []invRow
	for _, n := range nodes {
		for _, svc := range n.InvCompose {
			rows = append(rows, invRow{Service: svc, HostID: n.Host, Kind: "compose", Detail: n.InvDetail[svc]})
		}
		if n.InvSystemd != "" {
			rows = append(rows, invRow{Service: n.InvSystemd, HostID: n.Host, Kind: "systemd", Detail: n.InvDetail[n.InvSystemd]})
		}
	}
	for i := range rows {
		rows[i].App = b.pp.Decl.Name
	}
	return rows
}

// ---- 小工具 ----

var placeholderRe = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

// renderPlaceholders 已知占位符替换；返回渲染结果与未覆盖占位符清单
func renderPlaceholders(tpl string, values map[string]string) (string, []string) {
	out := tpl
	pairs := make([]string, 0, len(values)*2)
	for k, v := range values {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	out = strings.NewReplacer(pairs...).Replace(out)
	seen := map[string]bool{}
	var unknown []string
	for _, m := range placeholderRe.FindAllStringSubmatch(out, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			unknown = append(unknown, m[1])
		}
	}
	return out, unknown
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func hashStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pluginSHA 内嵌插件二进制指纹（agent 下载后校验与缓存复用判据；占位文件同样可算——下载端点按大小拒绝）
func pluginSHA(plugin, goos string) string {
	data, err := pluginBinFS(pluginFileName(plugin, goos))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func randomSecret16() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func readFileOrEmpty(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func dedupFiles(files []FileSpec) []FileSpec {
	seen := map[string]bool{}
	var out []FileSpec
	for _, f := range files {
		if !seen[f.Path] {
			seen[f.Path] = true
			out = append(out, f)
		}
	}
	return out
}
