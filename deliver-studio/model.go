// deliver-studio 数据模型 v2：应用 = 服务单元组合 + 一等变量；组件声明 / 物料 / 镜像仓 / 打包记录。
// 全部零业务知识——「单元怎么构建起容器、组件怎么编排、配置长什么样」都是数据（模板），
// 工具只做渲染与装配。旧格式（单应用混装前后端）在反序列化时自动归一为单元模型（collect-kit 推旧预设恒兼容）。
package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ---- 单元与变量（v2 核心） ----

// Unit 服务单元：一个可独立部署的容器服务（后端/前端/任意镜像，通用不设限）
type Unit struct {
	Code            string            `json:"code"`            // 单元编码（kebab，应用内唯一）
	Name            string            `json:"name"`            // 显示名
	Role            string            `json:"role"`            // backend|frontend|custom（仅影响图标/向导模板/拓扑图，不影响行为）
	Source          string            `json:"source"`          // 镜像来源：artifact|image_tar|image_ref（打包向导预选默认）
	ImageRepo       string            `json:"imageRepo"`       // 应用包构建的镜像名（tag=版本）
	ImageRef        string            `json:"imageRef"`        // 镜像包/镜像地址的缺省镜像地址
	Dockerfile      string            `json:"dockerfile"`      // 应用包构建的 Dockerfile 模板（构建上下文=应用包所在目录；单元内置+打包时变量）
	Port            string            `json:"port"`            // 服务端口（拓扑图/健康检查/UNIT_PORT 变量）
	HealthPath      string            `json:"healthPath"`      // 健康检查路径（空=安装器跳过）
	Entrypoint      bool              `json:"entrypoint"`      // 主入口（对外端口+健康检查+安装器复用模式目标）
	ComposeTemplate string            `json:"composeTemplate"` // 本单元 compose 服务块（2 空格缩进，可含多服务）
	ConfDir         string            `json:"confDir"`         // 可选：配置挂载目录（data/conf 挂载目标）
	ConfEnv         map[string]string `json:"confEnv"`         // 单元容器消费配置的 env
	ConfigTemplate  string            `json:"configTemplate"`  // 可选：单元配置模板（安装时渲染）
	Remark          string            `json:"remark"`
}

// Variable 应用自定义变量（一等公民）：模板中 {{NAME}} 变量引用的显式定义
type Variable struct {
	Name      string `json:"name"`   // 大写蛇形
	Label     string `json:"label"`  // 说明（变量字典与安装器询问时的提示）
	When      string `json:"when"`   // build|install（打包时填值 / 安装时赋值）
	Source    string `json:"source"` // input（人工填，Value=默认值）|const|random
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive,omitempty"` // 敏感值：界面掩码显示、安装器询问时提示
}

// App 应用定义 = 服务单元有序组合 + 变量清单；工具对应用形态零假设
type App struct {
	ID                int64      `json:"id"`
	Code              string     `json:"code"` // 唯一编码（kebab）
	Name              string     `json:"name"`
	DefaultVersion    string     `json:"defaultVersion"`
	Units             []Unit     `json:"units"`
	Variables         []Variable `json:"variables"`
	DefaultComponents []string   `json:"defaultComponents,omitempty"` // 打包向导缺省勾选的组件编码（仅 UI 预选，不参与引擎）
	Remark            string     `json:"remark"`
}

// 单元内置占位符（打包期替换；安装期变量由安装器处理）
const (
	builtinUnitImage    = "UNIT_IMAGE"
	builtinUnitPort     = "UNIT_PORT"
	builtinArtifactFile = "ARTIFACT_FILE"
)

var placeholderRe = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)

var unitCodeSan = regexp.MustCompile(`[^a-z0-9-]+`)

func normalizeUnitCode(s string) string {
	s = unitCodeSan.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-")
}

// scanPlaceholders 收集文本中出现的 {{X}} 变量引用（去重保序）
func scanPlaceholders(texts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		for _, m := range placeholderRe.FindAllStringSubmatch(t, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// renderPlaceholders 按 vals 批量替换 {{K}}
func renderPlaceholders(tpl string, vals map[string]string) string {
	out := tpl
	for k, v := range vals {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// leftoverPlaceholders 替换后仍残留的 {{X}}（不可解析）
func leftoverPlaceholders(tpl string) []string {
	return scanPlaceholders(tpl)
}

var composeSvcLineRe = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):`)

// composeServiceNames 解析编排块内的顶层服务名（2 空格缩进的 `name:` 行）
func composeServiceNames(block string) []string {
	var out []string
	for _, line := range strings.Split(block, "\n") {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// firstComposeServiceName 编排块首个服务名（components.json 的 service 字段）
func firstComposeServiceName(block string) string {
	names := composeServiceNames(block)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

var (
	composeDepKeyRe  = regexp.MustCompile(`^    depends_on:\s*(\[[^\]]*\])?\s*$`)
	composeDepItemRe = regexp.MustCompile(`^ {6,}-\s*([A-Za-z0-9_.-]+)`)
)

// composeDependsOn 解析编排块内 depends_on 依赖（服务名 → 依赖目标列表），
// 兼容块列表与 flow 列表两种写法——与前端 lib.js parseComposeEdges / composeYaml.modelEdges 同口径
func composeDependsOn(block string) map[string][]string {
	out := map[string][]string{}
	cur := ""
	inDeps := false
	for _, line := range strings.Split(block, "\n") {
		if m := composeSvcLineRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
			inDeps = false
			continue
		}
		m := composeDepKeyRe.FindStringSubmatch(line)
		if m != nil && cur != "" {
			inDeps = true
			if m[1] != "" { // flow 列表：depends_on: [a, b]
				for _, item := range strings.Split(strings.Trim(m[1], "[]"), ",") {
					item = strings.Trim(strings.TrimSpace(item), `'"`)
					if item != "" {
						out[cur] = append(out[cur], item)
					}
				}
				inDeps = false
			}
			continue
		}
		if inDeps {
			if m := composeDepItemRe.FindStringSubmatch(line); m != nil {
				out[cur] = append(out[cur], m[1])
				continue
			}
			if line != "" && !strings.HasPrefix(line, "      ") {
				inDeps = false
			}
		}
	}
	return out
}

// validateComposeBlock 编排服务块缩进形态（2 空格倍数、首行 2 空格）——单元与组件共用
func validateComposeBlock(block, ctx string) error {
	trimmed := strings.TrimRight(block, "\n")
	for _, line := range strings.Split(trimmed, "\n") {
		t := strings.TrimLeft(line, " ")
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		indent := len(line) - len(t)
		if indent < 2 || indent%2 != 0 {
			return fmt.Errorf("%s服务块缩进须为 2 空格倍数（首行 2 空格）: %q", ctx, line)
		}
	}
	return nil
}

// safeRolePath 角色配置文件相对路径防逃逸（与 console pkgload.safeRelPath 同规则）
func safeRolePath(p string) error {
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == "" || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") ||
		filepath.IsAbs(filepath.FromSlash(p)) || (len(p) >= 2 && p[1] == ':') {
		return fmt.Errorf("非法路径")
	}
	return nil
}

// ---- 应用校验（CRUD/导入共用） ----

var (
	appCodeRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	variableRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	unitRoles   = map[string]bool{"backend": true, "frontend": true, "custom": true}
	unitSources = map[string]bool{"artifact": true, "image_tar": true, "image_ref": true}
	varWhens    = map[string]bool{"build": true, "install": true}
	varSources  = map[string]bool{"input": true, "const": true, "random": true}
)

// validateApp 应用定义校验：编码/单元编码唯一/入口唯一/编排块形态/变量名形态
func validateApp(a *App) error {
	if !appCodeRe.MatchString(a.Code) {
		return fmt.Errorf("应用编码须为 kebab 形态（小写字母数字短横线，字母开头）: %s", a.Code)
	}
	if len(a.Units) == 0 {
		return fmt.Errorf("应用 %s 至少需要一个服务单元", a.Code)
	}
	entry := 0
	codes := map[string]bool{}
	services := map[string]string{} // 服务名 → 归属（单元编码/组件编码），查重
	for i := range a.Units {
		u := &a.Units[i]
		ctx := fmt.Sprintf("单元 %s: ", u.Code)
		if !appCodeRe.MatchString(u.Code) {
			return fmt.Errorf("%s编码须为 kebab 形态", ctx)
		}
		if codes[u.Code] {
			return fmt.Errorf("%s编码重复", ctx)
		}
		codes[u.Code] = true
		if u.Role != "" && !unitRoles[u.Role] {
			return fmt.Errorf("%s角色须为 backend/frontend/custom", ctx)
		}
		if u.Source != "" && !unitSources[u.Source] {
			return fmt.Errorf("%s镜像来源须为应用包构建（artifact）/镜像包导入（image_tar）/镜像地址导出（image_ref）", ctx)
		}
		if u.Entrypoint {
			entry++
		}
		if strings.TrimSpace(u.ComposeTemplate) == "" {
			return fmt.Errorf("%s缺少编排服务块（compose 服务定义）", ctx)
		}
		if err := validateComposeBlock(u.ComposeTemplate, ctx); err != nil {
			return err
		}
		for _, svc := range composeServiceNames(u.ComposeTemplate) {
			if owner, ok := services[svc]; ok {
				return fmt.Errorf("compose 服务名 %s 重复（%s 与 %s）", svc, owner, u.Code)
			}
			services[svc] = u.Code
		}
	}
	if entry > 1 {
		return fmt.Errorf("应用 %s 主入口单元只能有一个（当前 %d 个标记 entrypoint）", a.Code, entry)
	}
	vnames := map[string]bool{}
	for _, v := range a.Variables {
		if !variableRe.MatchString(v.Name) {
			return fmt.Errorf("变量名须为大写蛇形（如 TENANT_ID）: %s", v.Name)
		}
		if vnames[v.Name] {
			return fmt.Errorf("变量 %s 重复定义", v.Name)
		}
		vnames[v.Name] = true
		if v.When == "" {
			v.When = "install"
		}
		if !varWhens[v.When] {
			return fmt.Errorf("变量 %s 赋值时机须为 build/install", v.Name)
		}
		if v.Source == "" {
			v.Source = "input"
		}
		if !varSources[v.Source] {
			return fmt.Errorf("变量 %s 来源须为 input/const/random", v.Name)
		}
	}
	return nil
}

// ---- 旧格式归一化（v1 单应用 → v2 单元模型） ----

// legacyApp 旧格式读取专用：单条应用记录混装前后端（uiEnabled/uiDockerfile 挂在后端上）
type legacyApp struct {
	Code               string            `json:"code"`
	Name               string            `json:"name"`
	DefaultVersion     string            `json:"defaultVersion"`
	Port               string            `json:"port"`
	HealthPath         string            `json:"healthPath"`
	AppComposeTemplate string            `json:"appComposeTemplate"`
	ImageRepo          string            `json:"imageRepo"`
	JarDockerfile      string            `json:"jarDockerfile"`
	UiEnabled          bool              `json:"uiEnabled"`
	UiImageRepo        string            `json:"uiImageRepo"`
	UiDockerfile       string            `json:"uiDockerfile"`
	ConfigTemplate     string            `json:"configTemplate"`
	ConfDir            string            `json:"confDir"`
	ConfEnv            map[string]string `json:"confEnv"`
	Remark             string            `json:"remark"`
}

func (l *legacyApp) present() bool {
	return strings.TrimSpace(l.AppComposeTemplate) != "" || strings.TrimSpace(l.JarDockerfile) != "" ||
		strings.TrimSpace(l.ImageRepo) != "" || strings.TrimSpace(l.UiDockerfile) != "" || l.UiEnabled
}

// UnmarshalJSON 反序列化即归一：旧字段（appComposeTemplate/jarDockerfile/uiEnabled…）出现且无单元时自动转换为 v2
func (a *App) UnmarshalJSON(data []byte) error {
	type appV2 App
	if err := json.Unmarshal(data, (*appV2)(a)); err != nil {
		return err
	}
	var leg legacyApp
	if err := json.Unmarshal(data, &leg); err != nil {
		return nil // 旧字段解析失败不阻断，按 v2 处理
	}
	if leg.present() && len(a.Units) == 0 {
		normalizeLegacyApp(a, &leg)
	}
	return nil
}

// convertLegacyUnitVars 旧打包占位符 → 单元内置占位符
var legacyUnitVars = strings.NewReplacer(
	"{{APP_IMAGE}}", "{{UNIT_IMAGE}}",
	"{{UI_IMAGE}}", "{{UNIT_IMAGE}}",
	"{{APP_PORT}}", "{{UNIT_PORT}}",
	"{{JAR_FILE}}", "{{ARTIFACT_FILE}}",
)

type composeServiceBlock struct {
	name  string
	block string
}

// splitComposeServices 把应用级 compose 模板按顶层 2 空格缩进切成服务块序列
func splitComposeServices(tpl string) []composeServiceBlock {
	var out []composeServiceBlock
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" && buf.Len() > 0 {
			out = append(out, composeServiceBlock{cur, strings.TrimRight(buf.String(), "\n") + "\n"})
		}
	}
	for _, line := range strings.Split(tpl, "\n") {
		t := strings.TrimLeft(line, " ")
		indent := len(line) - len(t)
		if t != "" && indent == 2 && strings.HasSuffix(t, ":") && !strings.HasPrefix(t, "#") {
			flush()
			cur = strings.TrimSuffix(t, ":")
			buf.Reset()
		}
		if cur != "" {
			buf.WriteString(line + "\n")
		}
	}
	flush()
	return out
}

// normalizeLegacyApp 旧单应用 → 后端单元（入口，继承配置/端口/构建模板）+ 前端单元（如有）
func normalizeLegacyApp(a *App, leg *legacyApp) {
	if a.Name == "" {
		a.Name = leg.Name
	}
	if a.DefaultVersion == "" {
		a.DefaultVersion = leg.DefaultVersion
	}
	if a.Remark == "" {
		a.Remark = leg.Remark
	}
	backend := Unit{
		Code: "app", Name: "后端服务", Role: "backend", Entrypoint: true,
		Source: "artifact", ImageRepo: leg.ImageRepo,
		Dockerfile:     strings.ReplaceAll(leg.JarDockerfile, "{{JAR_FILE}}", "{{ARTIFACT_FILE}}"),
		Port:           leg.Port,
		HealthPath:     leg.HealthPath,
		ConfDir:        leg.ConfDir,
		ConfEnv:        leg.ConfEnv,
		ConfigTemplate: leg.ConfigTemplate,
	}
	if strings.TrimSpace(leg.JarDockerfile) == "" && strings.TrimSpace(leg.ImageRepo) == "" {
		backend.Source = "image_tar"
	}
	var frontend *Unit
	if leg.UiEnabled || strings.TrimSpace(leg.UiDockerfile) != "" {
		frontend = &Unit{Code: "web", Name: "前端站点", Role: "frontend", Source: "artifact",
			ImageRepo: leg.UiImageRepo, Dockerfile: leg.UiDockerfile}
	}
	var backendBlocks []string
	for _, sb := range splitComposeServices(leg.AppComposeTemplate) {
		blk := legacyUnitVars.Replace(sb.block)
		switch {
		case frontend != nil && strings.Contains(sb.block, "{{UI_IMAGE}}"):
			if strings.TrimSpace(frontend.ComposeTemplate) == "" && normalizeUnitCode(sb.name) != "" {
				frontend.Code = normalizeUnitCode(sb.name) // 单元编码跟随服务名（如 nginx）
			}
			frontend.ComposeTemplate += blk
		case strings.Contains(sb.block, "{{APP_IMAGE}}"):
			if len(backendBlocks) == 0 && normalizeUnitCode(sb.name) != "" {
				backend.Code = normalizeUnitCode(sb.name)
			}
			backendBlocks = append(backendBlocks, blk)
		default:
			backendBlocks = append(backendBlocks, blk) // 无法归属的服务块并入后端单元，保证不丢失
		}
	}
	backend.ComposeTemplate = strings.Join(backendBlocks, "")
	a.Units = []Unit{backend}
	if frontend != nil {
		if strings.TrimSpace(frontend.ComposeTemplate) == "" && strings.TrimSpace(leg.AppComposeTemplate) != "" {
			// 启用了前端但应用模板中无 UI 服务块：给出 nginx 骨架，保证单元可编排
			frontend.ComposeTemplate = "  " + frontend.Code + ":\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n"
		}
		a.Units = append(a.Units, *frontend)
	}
}

// ---- 组件（不变，声明驱动：勾选即编排） ----

// Endpoint 组件端点变量映射：安装器按部署模式回填（全栈=compose 服务名 / 复用=127.0.0.1）
type Endpoint struct {
	Placeholder string `json:"placeholder"`
	Label       string `json:"label,omitempty"` // 说明（变量字典与安装器询问时展示）
	Service     string `json:"service"`
	Port        string `json:"port"`
	From        string `json:"from,omitempty"` // host|port|url（缺省 url）
	Scheme      string `json:"scheme,omitempty"`
}

// Credential 组件凭据变量：安装器生成/询问/固定，同源写 .env 与配置渲染
type Credential struct {
	Placeholder string `json:"placeholder"`
	Label       string `json:"label,omitempty"` // 说明（变量字典与安装器询问时展示）
	Env         string `json:"env,omitempty"`
	Generate    string `json:"generate,omitempty"` // ask|random|const（缺省 const）
	Value       string `json:"value,omitempty"`
}

// FileDecl 组件编排附件（随包落位）：content 内联
type FileDecl struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

// ComponentSchema 组件 SQL 预置声明（复用外部基础设施模式下由通用安装器用一次性容器执行；
// 全栈模式由组件 compose 的 initdb 挂载覆盖）。连接参数为占位符名（组件端点/凭据），安装期解析。
type ComponentSchema struct {
	Dir      string `json:"dir"`            // SQL 目录（相对部署包根，如 extras；*.sql 文件名字典序）
	Image    string `json:"image"`          // 执行器镜像（含客户端，如 mysql:8.4）
	Host     string `json:"host"`           // 连接参数占位符名（组件端点/凭据变量名）
	Port     string `json:"port,omitempty"` // 缺省 3306
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// ComponentConfigCenter 组件配置中心声明（可选）：勾选该组件时由安装控制台生成
// plugin-plan → gate（人工审差异）→ plugin-apply → plugin-health 链（deliver-design.md ADR-4）。
// 插件二进制内嵌 console（nacos-config），部署包只带本声明与配置基线（组件 files）。
type ComponentConfigCenter struct {
	Kind          string           `json:"kind"`                    // nacos（v1 唯一）
	Plugin        string           `json:"plugin"`                  // nacos-config
	Namespace     string           `json:"namespace,omitempty"`     // 缺省 public
	IdentityKey   string           `json:"identityKey,omitempty"`   // serverIdentity 头的用户名占位符名（本组件凭据）
	IdentityValue string           `json:"identityValue,omitempty"` // serverIdentity 头的密码占位符名（本组件凭据）
	Configs       []ConfigItemDecl `json:"configs"`                 // 配置基线清单（file=组件 files 落包相对路径）
}

// ConfigItemDecl 配置基线条目：dataId 与包内基线文件的对应
type ConfigItemDecl struct {
	DataID string `json:"dataId"`
	Group  string `json:"group,omitempty"` // 缺省 DEFAULT_GROUP
	File   string `json:"file"`            // 组件 files 声明的落包相对路径（文本内容）
}

// Component 组件声明：勾选即编排（compose 服务块 + 镜像导出 + 应用配置接线三同步，全数据驱动）
type Component struct {
	ID              int64                  `json:"id"`
	Code            string                 `json:"code"`
	Name            string                 `json:"name"`
	Category        string                 `json:"category"` // database|cache|middleware|custom
	DefaultChecked  bool                   `json:"defaultChecked"`
	Images          []string               `json:"images"`
	ComposeServices string                 `json:"composeServices"` // YAML 文本（2 空格缩进服务块，可含多服务）
	Files           []FileDecl             `json:"files"`
	AppConfig       map[string]string      `json:"appConfig"` // topKey → 配置段文本
	Endpoints       []Endpoint             `json:"endpoints"`
	Credentials     []Credential           `json:"credentials"`
	Schema          *ComponentSchema       `json:"schema,omitempty"`       // SQL 预置（随 components.json 透传给安装器）
	ConfigCenter    *ComponentConfigCenter `json:"configCenter,omitempty"` // 配置中心（插件链，随 components.json 透传给安装器）
	// format:4 角色化组件（MySQL 主从等多实例拓扑；进包触发 format:4 升版——旧 console 拒收防半装）
	Roles       []Role `json:"roles,omitempty"`
	SpreadHosts bool   `json:"spreadHosts,omitempty"` // 反亲和：本组件服务安装时必须两两不同主机
	// 资源缺省（ADR-11：包=素材带缺省，console 安装时可逐服务覆盖）：CPU 核数/内存 MB
	Resources *ResourcesDecl `json:"resources,omitempty"`
	Remark    string         `json:"remark"`
}

// ResourcesDecl 组件资源缺省（K8s resources 对应物；可选段不升 format）
type ResourcesDecl struct {
	Cpus  string `json:"cpus,omitempty"`  // CPU 核数（如 "2"、"0.5"）
	MemMB int    `json:"memMB,omitempty"` // 内存上限 MB
}

// ---- format:4 角色声明（编辑模型与 components.json 包格式同构，全数据驱动零特判） ----

// RoleFile 角色配置文件：模板内联（安装期渲染 {{VAR}}/{{端点}}/{{凭据}}/{{角色变量}} 占位符），
// console 经 ComposePayload.Files 落盘、compose volumes 挂载进容器（如 per-role my.cnf）。
type RoleFile struct {
	Path     string `json:"path"`
	Template string `json:"template"`
}

// Probe 角色就绪探针（K8s readinessProbe 对应物）：tcp 缺省；http=端口+路径；exec=容器内命令
//（console 编译为 docker exec 重试环）
type Probe struct {
	Type   string `json:"type"`             // tcp|http|exec
	Port   string `json:"port,omitempty"`
	Path   string `json:"path,omitempty"`
	Script string `json:"script,omitempty"` // exec：容器内 sh -c 脚本（退出 0=就绪）
}

// InitStep 角色初始化步骤（K8s Job 对应物）：角色服务就绪后 docker exec 进容器执行幂等脚本；
// After=前置角色名（如 replica 的 join 等 primary 的建复制账号步骤完成）。
type InitStep struct {
	ID         string `json:"id"`
	After      string `json:"after,omitempty"`
	Script     string `json:"script"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// Role 组件角色（StatefulSet 角色身份对应物）：per-role 服务块/配置文件/就绪探针/初始化链。
// 端点仍声明在组件级（Service 指向具体服务，如 DB_HOST→primary 服务）。
type Role struct {
	Name      string            `json:"name"`
	Services  []string          `json:"services"`
	Vars      map[string]string `json:"vars,omitempty"` // 角色变量（如 MYSQL_SERVER_ID 区分实例）
	Files     []RoleFile        `json:"files,omitempty"`
	Readiness *Probe            `json:"readiness,omitempty"`
	Init      []InitStep        `json:"init,omitempty"`
}

// ---- 物料 / 镜像仓 / 打包记录（不变） ----

// Material 本地物料（大文件在 materials/ 目录，DB 记索引）
type Material struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"` // artifact|jar|ui_zip（制品类）|image_tar|extra
	FileName   string `json:"fileName"`
	StoredPath string `json:"storedPath"`
	Size       int64  `json:"size"`
	AppCode    string `json:"appCode"`
	Version    string `json:"version"`
	Remark     string `json:"remark"`
	CreatedAt  string `json:"createdAt"`
}

// materialIsArtifact 制品类判断（jar/ui_zip 为旧值，兼容 collect-kit 推送与历史数据）
func materialIsArtifact(kind string) bool {
	return kind == "artifact" || kind == "jar" || kind == "ui_zip"
}

// ImageItem 本地镜像仓条目（tar 在 image-store/）
type ImageItem struct {
	ID        int64  `json:"id"`
	FileName  string `json:"fileName"` // safe 命名（ref 中 / : → _）+ .tar
	ImageRef  string `json:"imageRef"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
}

// PackageRec 打包记录（异步任务：status=running|ok|failed）。
// kind=kit（format:3 套件，assemble 产出）| final（最终交付包=套件+交付附件重打包，见 delivery.go）
type PackageRec struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	AppCode      string `json:"appCode"`
	Components   string `json:"components"` // csv
	Standalone   bool   `json:"standalone"` // 单机部署：随包附带 install.sh 薄壳
	Kind         string `json:"kind"`       // kit|final（存量记录缺省 kit）
	DeliveryID   int64  `json:"deliveryId,omitempty"`
	SourcePkgSha string `json:"sourcePkgSha,omitempty"` // final：来源套件指纹 sha256(manifest.json)
	DeliverySha  string `json:"deliverySha,omitempty"`  // final：叠加的附件包 sha256
	Status       string `json:"status"`
	Stage        string `json:"stage"`
	Log          string `json:"log"`
	ZipPath      string `json:"zipPath"`
	CreatedAt    string `json:"createdAt"`
	FinishedAt   string `json:"finishedAt"`
}

// Preset 预设导入/导出格式（收集器/手工维护的 JSON，与应用/组件定义同构）。
// 应用为 v2 单元模型；旧格式应用（单条混装前后端）导入时自动归一。
type Preset struct {
	Version    string            `json:"version,omitempty"`
	ExportedAt string            `json:"exportedAt,omitempty"`
	Apps       []App             `json:"apps,omitempty"`
	Components []Component       `json:"components"`
	Settings   map[string]string `json:"settings,omitempty"`
	Materials  []Material        `json:"materials,omitempty"`
	Images     []ImageItem       `json:"images,omitempty"`
}

// ---- 部署包内声明（components.json format:3，与 deliver-console 的解析结构同构） ----

type UnitDecl struct {
	Code       string            `json:"code"`
	Service    string            `json:"service,omitempty"`  // compose 服务名（编排块首个服务）
	Services   []string          `json:"services,omitempty"` // 本单元全部服务名（format:3：console 按主机切分编排）
	Image      string            `json:"image,omitempty"`
	Port       string            `json:"port,omitempty"`       // format:3：逐单元端口（跨机端点/健康检查）
	HealthPath string            `json:"healthPath,omitempty"` // format:3：逐单元健康路径
	Artifact   string            `json:"artifact,omitempty"`   // format:3：随包制品文件名（artifacts/ 下，systemd 裸机部署用）
	ConfDir    string            `json:"confDir,omitempty"`
	ConfEnv    map[string]string `json:"confEnv,omitempty"`
}

type EntryDecl struct {
	Unit       string `json:"unit"`
	Service    string `json:"service,omitempty"`
	Image      string `json:"image,omitempty"`
	Port       string `json:"port,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
}

// VarDecl 安装时变量声明（应用自定义变量中 When=install 的部分）
type VarDecl struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	Source    string `json:"source"` // input|const|random
	Value     string `json:"value,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"` // 敏感值：安装器询问时提示
	Env       string `json:"env,omitempty"`       // 非空时同源写根 .env
}

type ComponentDecl struct {
	Code         string                 `json:"code"`
	Category     string                 `json:"category,omitempty"`
	Services     []string               `json:"services,omitempty"` // format:3：本组件全部服务名
	Images       []string               `json:"images,omitempty"`   // format:3：本组件镜像清单（console 按主机裁剪拉取）
	Endpoints    []Endpoint             `json:"endpoints,omitempty"`
	Credentials  []Credential           `json:"credentials,omitempty"`
	Schema       *ComponentSchema       `json:"schema,omitempty"`
	ConfigCenter *ComponentConfigCenter `json:"configCenter,omitempty"` // format:3 可选段（ADR-5：可选扩展不升版）
	// format:4 可选段（结构性新能力升版隔离：旧 console 拒收 format:4 防静默半装主从）
	Roles       []Role `json:"roles,omitempty"`
	SpreadHosts bool   `json:"spreadHosts,omitempty"`
	// 可选资源缺省（ADR-11；可选段不升版——旧 console 忽略无害）
	Resources *ResourcesDecl `json:"resources,omitempty"`
}

type ComponentsDecl struct {
	Format     int             `json:"format"` // 3|4（4=含角色化组件等新能力；无新特性恒 3 保持旧 console 可装）
	Name       string          `json:"name,omitempty"`
	Version    string          `json:"version,omitempty"`
	Entry      EntryDecl       `json:"entry"`
	Units      []UnitDecl      `json:"units"`
	Variables  []VarDecl       `json:"variables,omitempty"`
	Components []ComponentDecl `json:"components"`
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseImages(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func parseFiles(s string) []FileDecl {
	var out []FileDecl
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func parseAppConfig(s string) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func parseEndpoints(s string) []Endpoint {
	var out []Endpoint
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func parseCredentials(s string) []Credential {
	var out []Credential
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func parseSchema(s string) *ComponentSchema {
	if s == "" || s == "null" {
		return nil
	}
	var out ComponentSchema
	if json.Unmarshal([]byte(s), &out) != nil || out.Dir == "" {
		return nil
	}
	return &out
}

func parseConfigCenter(s string) *ComponentConfigCenter {
	if s == "" || s == "null" {
		return nil
	}
	var out ComponentConfigCenter
	if json.Unmarshal([]byte(s), &out) != nil || out.Kind == "" {
		return nil
	}
	return &out
}
