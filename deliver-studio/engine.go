// deliver-studio 打包引擎：应用单元组合（三源制品）+ 勾选组件 → format:3 部署包（compose/镜像/制品/声明/配置模板/清单），
// 消费方为 deliver-console 安装控制台。
// 全程数据驱动：单元如何构建起容器来自单元定义模板，组件编排来自组件声明，引擎零业务知识。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	imageStoreDir = "image-store"
	materialsDir  = "materials"
	packagesDir   = "packages"
	stagingDir    = "staging"
	deliveriesDir = "deliveries"
)

// UnitPick 打包请求中的单元镜像来源选择（三选一：应用包构建 / 镜像包导入 / 镜像地址导出）
type UnitPick struct {
	Code         string `json:"code"`         // 应用内单元编码
	Source       string `json:"source"`       // artifact|image_tar|image_ref（缺省用单元定义里的 Source）
	MaterialID   int64  `json:"materialId"`   // artifact=应用包；image_tar=镜像包
	ImageRef     string `json:"imageRef"`     // image_ref 的镜像地址（缺省用单元定义 ImageRef）
	WithArtifact bool   `json:"withArtifact"` // format:3：制品随包附带（artifacts/，供 console systemd 裸机部署）
}

// AssembleRequest 打包请求（POST /api/packages）
type AssembleRequest struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	AppCode          string            `json:"appCode"`
	Units            []UnitPick        `json:"units"`
	ComponentCodes   []string          `json:"componentCodes"`
	ExtraMaterialIDs []int64           `json:"extraMaterialIds"`
	BuildVars        map[string]string `json:"buildVars"`
	Standalone       bool              `json:"standalone"` // 单机部署：随包附带 install.sh 薄壳（ADR-1 增补边界，见 installer.go）
	// ConfigCenterInclude 配置基线 DataID 白名单（可选）：非空时 configCenter 组件只落清单内 DataID
	// ——微服务形态按选中业务裁剪专属 Data ID（共享基线 common/db/dubbo 恒在清单即可）
	ConfigCenterInclude []string `json:"configCenterInclude,omitempty"`
}

// assembleTask 运行中任务的活日志（锁只护本任务日志切片——完成回调教训：绝不嵌套持锁回调）
type assembleTask struct {
	mu    sync.Mutex
	lines []string
}

type Engine struct {
	root  string
	store *Store
	mu    sync.Mutex
	tasks map[int64]*assembleTask
}

func NewEngine(root string, store *Store) *Engine {
	for _, d := range []string{imageStoreDir, materialsDir, packagesDir, stagingDir, deliveriesDir} {
		_ = os.MkdirAll(filepath.Join(root, d), 0755)
	}
	return &Engine{root: root, store: store, tasks: map[int64]*assembleTask{}}
}

// ---- 镜像名 → 安全 tar 文件名（与 deliver-console/组件声明约定一致） ----

func safeImageName(ref string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(ref)
}

var nameSan = regexp.MustCompile(`[^a-z0-9-]+`)

func composeProjectName(name string) string {
	s := nameSan.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(s, "-")
}

// ---- 打包计划（校验先行：任何配置错误在动 docker 之前拦截） ----

type assemblePlan struct {
	app        *App
	units      []*Unit // 进包单元子集（= pick 的单元；pick 子集语义支撑微服务形态按业务裁剪）
	entry      *Unit
	picks      map[string]*UnitPick // 单元编码 → 制品选择
	comps      []*Component
	buildVals  map[string]string // 打包期变量终值（请求值 > 定义默认值）
	knownVars  map[string]bool   // 模板可用的全部占位符（错误信息列出）
	installSet map[string]bool   // 安装期占位符（组件端点/凭据 + 应用 install 变量）
	notes      []string          // 非阻断提示（随日志输出）
}

// buildPlan 打包前硬校验：单元/入口/服务名/三源物料/变量完备性/占位符交叉校验
func (e *Engine) buildPlan(req *AssembleRequest) (*assemblePlan, error) {
	app, err := e.store.GetApp(req.AppCode)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("应用不存在: %s", req.AppCode)
	}
	if len(app.Units) == 0 {
		return nil, fmt.Errorf("应用 %s 没有服务单元", app.Code)
	}
	plan := &assemblePlan{app: app, picks: map[string]*UnitPick{},
		buildVals: map[string]string{}, knownVars: map[string]bool{}, installSet: map[string]bool{}}

	// 单元选择：pick 子集语义——pick 的单元进包（微服务形态按业务勾选），未 pick 不进；
	// 编码须存在于应用定义（未知编码报错）；至少 pick 一个单元
	for i := range req.Units {
		p := req.Units[i]
		if plan.picks[p.Code] != nil {
			return nil, fmt.Errorf("单元选择重复: %s", p.Code)
		}
		known := false
		for j := range app.Units {
			if app.Units[j].Code == p.Code {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("单元选择 %s 不在应用 %s 的单元定义中", p.Code, app.Code)
		}
		plan.picks[p.Code] = &req.Units[i]
	}
	for i := range app.Units {
		if plan.picks[app.Units[i].Code] != nil {
			plan.units = append(plan.units, &app.Units[i])
		}
	}
	if len(plan.units) == 0 {
		return nil, fmt.Errorf("应用 %s 至少要选择一个单元（当前 units 为空）", app.Code)
	}

	// 入口单元：显式标记优先（仅在进包单元中找），缺省第一个进包单元（记提示）
	for _, u := range plan.units {
		if u.Entrypoint {
			plan.entry = u
			break
		}
	}
	if plan.entry == nil {
		plan.entry = plan.units[0]
		plan.notes = append(plan.notes, fmt.Sprintf("未标记主入口单元，缺省取第一个进包单元 %s", plan.entry.Code))
	}

	// compose 服务名唯一性（单元块 × 组件块）
	owner := map[string]string{}
	claim := func(svc, who string) error {
		if prev, ok := owner[svc]; ok {
			return fmt.Errorf("compose 服务名 %s 重复（%s 与 %s）", svc, prev, who)
		}
		owner[svc] = who
		return nil
	}
	for _, u := range plan.units {
		if strings.TrimSpace(u.ComposeTemplate) == "" {
			return nil, fmt.Errorf("单元 %s 缺少编排服务块", u.Code)
		}
		if err := validateComposeBlock(u.ComposeTemplate, "单元 "+u.Code+" "); err != nil {
			return nil, err
		}
		for _, svc := range composeServiceNames(u.ComposeTemplate) {
			if err := claim(svc, "单元 "+u.Code); err != nil {
				return nil, err
			}
		}
	}
	for _, code := range req.ComponentCodes {
		c, err := e.store.GetComponent(code)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, fmt.Errorf("组件不存在: %s", code)
		}
		for _, svc := range composeServiceNames(c.ComposeServices) {
			if err := claim(svc, "组件 "+code); err != nil {
				return nil, err
			}
		}
		// 配置基线 DataID 白名单：非空时裁剪 configCenter.configs（未选业务的专属 Data ID 不落包）
		if len(req.ConfigCenterInclude) > 0 && c.ConfigCenter != nil {
			keep := c.ConfigCenter.Configs[:0:0]
			for _, cfg := range c.ConfigCenter.Configs {
				for _, id := range req.ConfigCenterInclude {
					if cfg.DataID == id {
						keep = append(keep, cfg)
						break
					}
				}
			}
			if len(keep) == 0 {
				return nil, fmt.Errorf("组件 %s 的配置基线经 configCenterInclude 过滤后为空——共享基线（common/db 等）应保留在清单内", c.Code)
			}
			c.ConfigCenter.Configs = keep
		}
		if err := validateConfigCenter(c); err != nil {
			return nil, fmt.Errorf("组件 %s 配置中心声明无效: %w", code, err)
		}
		plan.comps = append(plan.comps, c)
	}

	// depends_on 悬空目标校验：编排块引用的依赖服务必须存在于合并后的服务集合（进包单元块 + 已勾选组件块）——
	// 与图形编排画布的连线口径一致（画布只能连到存在的节点，代码模式由此校验兜底）
	for _, u := range plan.units {
		for from, deps := range composeDependsOn(u.ComposeTemplate) {
			for _, to := range deps {
				if _, ok := owner[to]; !ok {
					return nil, fmt.Errorf("单元 %s 服务 %s 的 depends_on 引用了不存在的服务 %s——请勾选对应组件或在编排中修正引用", u.Code, from, to)
				}
			}
		}
	}
	for _, c := range plan.comps {
		for from, deps := range composeDependsOn(c.ComposeServices) {
			for _, to := range deps {
				if _, ok := owner[to]; !ok {
					return nil, fmt.Errorf("组件 %s 服务 %s 的 depends_on 引用了不存在的服务 %s——请勾选其依赖组件或修正声明", c.Code, from, to)
				}
			}
		}
	}

	// 单元三源完备性
	for _, u := range plan.units {
		pick := plan.picks[u.Code]
		src := pick.Source
		if src == "" {
			src = u.Source
		}
		switch src {
		case "artifact":
			if strings.TrimSpace(u.Dockerfile) == "" {
				return nil, fmt.Errorf("单元 %s（应用包构建）未配置 Dockerfile 模板", u.Code)
			}
			if strings.TrimSpace(u.ImageRepo) == "" {
				return nil, fmt.Errorf("单元 %s（应用包构建）未配置镜像名", u.Code)
			}
			if pick.MaterialID <= 0 {
				return nil, fmt.Errorf("单元 %s（应用包构建）未选择应用包", u.Code)
			}
			// zip 应用包 × {{ARTIFACT_FILE}} 整包引用互斥：zip 按前端惯例解压为构建上下文（原包
			// 不保留，resolveUnitImage），而整包引用（后端 fat-jar 模板 COPY {{ARTIFACT_FILE}}）需要
			// 原包在场——组合必在 docker build 才爆（COPY not found，实测包 #10），打包期前置拦截并指路
			if m, mErr := e.materialArtifact(pick.MaterialID); mErr == nil &&
				strings.EqualFold(filepath.Ext(m.StoredPath), ".zip") &&
				strings.Contains(u.Dockerfile, "{{"+builtinArtifactFile+"}}") {
				return nil, fmt.Errorf("单元 %s 不能选 zip 应用包 %s——本单元 Dockerfile 以 {{ARTIFACT_FILE}} 引用整包（后端 fat-jar 形态），而 zip 会按前端惯例解压为构建上下文（原包不保留、COPY 必找不到）：请为该单元选择 jar/fat-jar 应用包；UI zip 请配给 Dockerfile 用 COPY . 的前端单元", u.Code, m.FileName)
			}
		case "image_tar":
			if pick.MaterialID <= 0 {
				return nil, fmt.Errorf("单元 %s（镜像包导入）未选择镜像包", u.Code)
			}
		case "image_ref":
			if strings.TrimSpace(pick.ImageRef) == "" && strings.TrimSpace(u.ImageRef) == "" {
				return nil, fmt.Errorf("单元 %s（镜像地址导出）未填写镜像地址", u.Code)
			}
		default:
			return nil, fmt.Errorf("单元 %s 镜像来源非法: %s（须 artifact/image_tar/image_ref）", u.Code, src)
		}
	}

	// 单机安装脚本边界：不支持配置中心插件链（cc-plan/gate/apply/health 为 console 专属）——
	// 组合在打包期硬拦截，避免现场装出缺配置的半残栈
	if req.Standalone {
		for _, c := range plan.comps {
			if c.ConfigCenter != nil {
				return nil, fmt.Errorf("勾选「单机部署（随包 install.sh）」时不能勾选配置中心组件 %s——配置中心插件链归 deliver-console，请改用 console 安装或不勾选该组件", c.Code)
			}
		}
	}

	// format:4 角色化组件校验：install.sh 薄壳不解析角色链（主从初始化/探针归 console 编排）；
	// 角色声明完整性（服务归属/唯一性/文件路径防逃逸/探针类型/初始化步骤引用）在此收口
	for _, c := range plan.comps {
		if len(c.Roles) == 0 {
			continue
		}
		if req.Standalone {
			return nil, fmt.Errorf("勾选「单机部署（随包 install.sh）」时不能勾选角色化组件 %s（%d 个角色）——角色初始化链与探针归 deliver-console 编排，请改用 console 安装或不勾选该组件", c.Code, len(c.Roles))
		}
		svcSet := map[string]bool{}
		for _, svc := range composeServiceNames(c.ComposeServices) {
			svcSet[svc] = true
		}
		if c.SpreadHosts && len(svcSet) < 2 {
			return nil, fmt.Errorf("组件 %s 声明了反亲和（spreadHosts）但只有 %d 个服务——无多实例可分散", c.Code, len(svcSet))
		}
		roleNames := map[string]bool{}
		seenSvcs := map[string]bool{}
		for _, r := range c.Roles {
			if r.Name == "" {
				return nil, fmt.Errorf("组件 %s 存在未命名角色", c.Code)
			}
			if roleNames[r.Name] {
				return nil, fmt.Errorf("组件 %s 角色名 %s 重复", c.Code, r.Name)
			}
			roleNames[r.Name] = true
			if len(r.Services) == 0 {
				return nil, fmt.Errorf("组件 %s 角色 %s 未声明服务", c.Code, r.Name)
			}
			for _, svc := range r.Services {
				if !svcSet[svc] {
					return nil, fmt.Errorf("组件 %s 角色 %s 的服务 %s 不在编排服务块中", c.Code, r.Name, svc)
				}
				if seenSvcs[svc] {
					return nil, fmt.Errorf("服务 %s 同时归属组件 %s 的多个角色", svc, c.Code)
				}
				seenSvcs[svc] = true
			}
			for _, f := range r.Files {
				if f.Path == "" || strings.TrimSpace(f.Template) == "" {
					return nil, fmt.Errorf("组件 %s 角色 %s 的配置文件缺少 path/template", c.Code, r.Name)
				}
				if err := safeRolePath(f.Path); err != nil {
					return nil, fmt.Errorf("组件 %s 角色 %s 配置文件路径非法 %q: %w", c.Code, r.Name, f.Path, err)
				}
			}
			if p := r.Readiness; p != nil {
				switch p.Type {
				case "", "tcp", "http":
				case "exec":
					if strings.TrimSpace(p.Script) == "" {
						return nil, fmt.Errorf("组件 %s 角色 %s 的 exec 探针缺少 script", c.Code, r.Name)
					}
				default:
					return nil, fmt.Errorf("组件 %s 角色 %s 探针类型非法 %q（tcp|http|exec）", c.Code, r.Name, p.Type)
				}
			}
			initIDs := map[string]bool{}
			for _, st := range r.Init {
				if st.ID == "" || strings.TrimSpace(st.Script) == "" {
					return nil, fmt.Errorf("组件 %s 角色 %s 初始化步骤缺少 id/script", c.Code, r.Name)
				}
				if initIDs[st.ID] {
					return nil, fmt.Errorf("组件 %s 角色 %s 初始化步骤 id %s 重复", c.Code, r.Name, st.ID)
				}
				initIDs[st.ID] = true
			}
		}
		for _, r := range c.Roles {
			for _, st := range r.Init {
				if st.After != "" && !roleNames[st.After] {
					return nil, fmt.Errorf("组件 %s 角色 %s 初始化步骤 %s 引用了不存在的角色 %q", c.Code, r.Name, st.ID, st.After)
				}
			}
		}
	}

	// 端点/凭据占位符跨组件冲突：同一占位符只能由一个勾选组件提供（如 mysql 与 mysql-ha 的
	// DB_HOST 同源互斥——双勾会在安装表单/配置渲染产生双定义）
	{
		epOwner := map[string]string{}
		credOwner := map[string]string{}
		for _, c := range plan.comps {
			for _, ep := range c.Endpoints {
				if prev, ok := epOwner[ep.Placeholder]; ok {
					return nil, fmt.Errorf("端点占位符 %s 同时由组件 %s 与 %s 提供——两者互斥，只能勾选其一", ep.Placeholder, prev, c.Code)
				}
				epOwner[ep.Placeholder] = c.Code
			}
			for _, cred := range c.Credentials {
				if prev, ok := credOwner[cred.Placeholder]; ok {
					return nil, fmt.Errorf("凭据占位符 %s 同时由组件 %s 与 %s 提供——两者互斥，只能勾选其一", cred.Placeholder, prev, c.Code)
				}
				credOwner[cred.Placeholder] = c.Code
			}
		}
	}

	// 变量：打包期必须有值（请求 > 默认）；已知占位符集合 = 应用变量 + 组件端点/凭据 + 单元内置
	for _, v := range app.Variables {
		if plan.knownVars[v.Name] {
			return nil, fmt.Errorf("变量 %s 重复定义", v.Name)
		}
		plan.knownVars[v.Name] = true
		if v.When == "install" {
			plan.installSet[v.Name] = true
			continue
		}
		val := strings.TrimSpace(req.BuildVars[v.Name])
		if val == "" {
			val = v.Value
		}
		if val == "" {
			return nil, fmt.Errorf("打包时变量 %s（%s）未赋值——请在打包向导中填写", v.Name, orDefault(v.Label, v.Name))
		}
		plan.buildVals[v.Name] = val
	}
	plan.knownVars[builtinUnitImage] = true
	plan.knownVars[builtinUnitPort] = true
	plan.knownVars[builtinArtifactFile] = true
	for _, c := range plan.comps {
		for _, ep := range c.Endpoints {
			plan.knownVars[ep.Placeholder] = true
			plan.installSet[ep.Placeholder] = true
		}
		for _, cr := range c.Credentials {
			plan.knownVars[cr.Placeholder] = true
			plan.installSet[cr.Placeholder] = true
		}
	}
	// 配置基线占位符校验：基线文件在安装期由 console 渲染——只允许安装期占位符（组件端点/凭据 +
	// 应用 install 变量）；打包期变量不参与（基线不烘焙，留占位符原文）
	for _, c := range plan.comps {
		if c.ConfigCenter == nil {
			continue
		}
		for _, cfg := range c.ConfigCenter.Configs {
			content := ""
			for _, f := range c.Files {
				if f.Path == cfg.File {
					content = f.Content
					break
				}
			}
			for _, name := range scanPlaceholders(content) {
				if !plan.installSet[name] {
					return nil, fmt.Errorf("组件 %s 配置基线 %s（%s）引用了安装期不可解析的占位符 {{%s}}——基线只允许组件端点/凭据与应用 install 变量", c.Code, cfg.DataID, cfg.File, name)
				}
			}
		}
	}

	// 变量引用交叉校验：Dockerfile/编排块只允许单元内置+打包时变量；配置模板另允许安装时变量
	for _, u := range plan.units {
		for _, text := range []string{u.Dockerfile, u.ComposeTemplate} {
			for _, name := range scanPlaceholders(text) {
				if name == builtinUnitImage || name == builtinUnitPort || name == builtinArtifactFile {
					continue
				}
				if _, ok := plan.buildVals[name]; ok {
					continue
				}
				if _, ok := plan.installSet[name]; ok {
					return nil, fmt.Errorf("单元 %s 的 Dockerfile/编排块引用了安装时变量 {{%s}}——这两处只在打包时渲染，请改用打包时变量（build）或编排 ${%s} .env 写法", u.Code, name, name)
				}
				return nil, fmt.Errorf("单元 %s 引用了未定义变量 {{%s}}——请先在「变量」页签登记后再引用（已知变量: %s）",
					u.Code, name, strings.Join(sortedBoolKeys(plan.knownVars), ", "))
			}
		}
		for _, name := range scanPlaceholders(u.ConfigTemplate) {
			if !plan.knownVars[name] {
				return nil, fmt.Errorf("单元 %s 配置模板引用了未定义变量 {{%s}}——请先在「变量」页签登记后再引用（已知变量: %s）",
					u.Code, name, strings.Join(sortedBoolKeys(plan.knownVars), ", "))
			}
		}
	}
	return plan, nil
}

// validateConfigCenter 组件配置中心声明硬校验：kind/plugin 白名单、基线文件必须在组件附件内、
// 身份头占位符必须是本组件凭据（安装期才能解析到值）
func validateConfigCenter(c *Component) error {
	cc := c.ConfigCenter
	if cc == nil {
		return nil
	}
	if cc.Kind != "nacos" || cc.Plugin != "nacos-config" {
		return fmt.Errorf("kind/plugin 须为 nacos/nacos-config（v1 唯一支持的配置中心）")
	}
	if len(cc.Configs) == 0 {
		return fmt.Errorf("configs 基线清单为空")
	}
	fileSet := map[string]string{} // 组件附件路径 → 内容
	for _, f := range c.Files {
		fileSet[f.Path] = f.Content
	}
	credSet := map[string]bool{}
	for _, cr := range c.Credentials {
		credSet[cr.Placeholder] = true
	}
	for _, cfg := range cc.Configs {
		if cfg.DataID == "" {
			return fmt.Errorf("configs 存在空 dataId")
		}
		content, ok := fileSet[cfg.File]
		if !ok || strings.TrimSpace(content) == "" {
			return fmt.Errorf("基线 %s 的文件 %s 不在组件附件（files）或内容为空", cfg.DataID, cfg.File)
		}
	}
	for name, what := range map[string]string{cc.IdentityKey: "identityKey", cc.IdentityValue: "identityValue"} {
		if name == "" {
			continue
		}
		if !credSet[name] {
			return fmt.Errorf("%s=%s 不是本组件凭据占位符（先在「凭据」分区登记）", what, name)
		}
	}
	return nil
}

// ---- 异步任务入口 ----

func (e *Engine) StartAssemble(req *AssembleRequest) (*PackageRec, error) {
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.AppCode) == "" {
		return nil, fmt.Errorf("包名与应用编码必填")
	}
	if req.Version == "" {
		req.Version = "1.0.0"
	}
	plan, err := e.buildPlan(req)
	if err != nil {
		return nil, err
	}
	rec := &PackageRec{Name: req.Name, Version: req.Version, AppCode: req.AppCode,
		Components: strings.Join(req.ComponentCodes, ","), Standalone: req.Standalone, Status: "running", Stage: "排队中"}
	if err := e.store.InsertPackage(rec); err != nil {
		return nil, err
	}
	task := &assembleTask{}
	e.mu.Lock()
	e.tasks[rec.ID] = task
	e.mu.Unlock()
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		task.mu.Lock()
		task.lines = append(task.lines, line)
		task.mu.Unlock()
	}
	go func() {
		err := e.assemble(req, plan, logf)
		e.mu.Lock()
		delete(e.tasks, rec.ID)
		e.mu.Unlock()
		if err != nil {
			logf("打包失败 ✘: %v", err)
			e.store.FinishPackage(rec.ID, "failed", "失败", strings.Join(append(task.snapshot(), fmt.Sprintf("打包失败: %v", err)), "\n"), "")
			return
		}
		zipPath := filepath.Join(e.root, packagesDir, composeProjectName(req.Name)+"-"+req.Version+".zip")
		e.store.FinishPackage(rec.ID, "ok", "完成", strings.Join(task.snapshot(), "\n"), zipPath)
		logf("打包完成 ✔ %s", zipPath)
	}()
	return rec, nil
}

func (t *assembleTask) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// LiveLog 运行中任务的活日志（已完成任务返回空，调用方回退读 DB 落库日志）
func (e *Engine) LiveLog(id int64) []string {
	e.mu.Lock()
	task := e.tasks[id]
	e.mu.Unlock()
	if task == nil {
		return nil
	}
	return task.snapshot()
}

// ---- 打包主体 ----

func (e *Engine) assemble(req *AssembleRequest, plan *assemblePlan, logf func(string, ...any)) error {
	proj := composeProjectName(req.Name)
	if proj == "" {
		return fmt.Errorf("包名无效")
	}
	pkgDir := filepath.Join(e.root, packagesDir, proj+"-"+req.Version)
	stage := filepath.Join(e.root, stagingDir, fmt.Sprintf("%s-%d", proj, os.Getpid()))
	_ = os.RemoveAll(pkgDir)
	_ = os.RemoveAll(stage)
	defer os.RemoveAll(stage)
	for _, d := range []string{filepath.Join(pkgDir, "images"), filepath.Join(pkgDir, "extras"), stage} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	imagesDir := filepath.Join(pkgDir, "images")
	app := plan.app
	for _, n := range plan.notes {
		logf("提示: %s", n)
	}

	// 1. 逐单元解析镜像（三源），渲染各单元编排块；按需随包附带制品（format:3）
	logf("==> [1/5] 单元镜像（%d 个）", len(plan.units))
	unitImages := map[string]string{}   // 单元编码 → 生效镜像引用
	unitService := map[string]string{}  // 单元编码 → 首个 compose 服务名
	unitArtifact := map[string]string{} // 单元编码 → artifacts/ 下制品文件名（勾选随包时）
	artifactsDir := filepath.Join(pkgDir, "artifacts")
	unitBlocks := make([]string, 0, len(plan.units))
	for _, u := range plan.units {
		pick := plan.picks[u.Code]
		ref, err := e.resolveUnitImage(u, pick, req.Version, plan.buildVals, stage, imagesDir, logf)
		if err != nil {
			return err
		}
		if pick != nil && pick.WithArtifact {
			m, err := e.materialArtifact(pick.MaterialID)
			if err != nil {
				return fmt.Errorf("单元 %s 随包制品缺失（勾选了「制品随包附带」但未选应用包）: %w", u.Code, err)
			}
			if err := os.MkdirAll(artifactsDir, 0755); err != nil {
				return err
			}
			dest := u.Code + strings.ToLower(filepath.Ext(m.FileName))
			if err := copyFile(m.StoredPath, filepath.Join(artifactsDir, dest)); err != nil {
				return err
			}
			unitArtifact[u.Code] = dest
			logf("    单元 %s：制品随包附带 → artifacts/%s", u.Code, dest)
		}
		unitImages[u.Code] = ref
		unitService[u.Code] = firstComposeServiceName(u.ComposeTemplate)
		vals := map[string]string{builtinUnitImage: ref, builtinUnitPort: orDefault(u.Port, "8080")}
		for k, v := range plan.buildVals {
			vals[k] = v
		}
		block := renderPlaceholders(u.ComposeTemplate, vals)
		if left := leftoverPlaceholders(block); len(left) > 0 {
			return fmt.Errorf("单元 %s 编排块引用了不可解析变量 %v", u.Code, left)
		}
		unitBlocks = append(unitBlocks, strings.TrimRight(block, "\n")+"\n")
	}

	// 2. 组件（compose 服务块 + 镜像导出 + 附件 + 角色声明透传）
	logf("==> [2/5] 组件装配（%d 项）", len(plan.comps))
	var mwBlocks strings.Builder
	var files []FileDecl
	var declComponents []ComponentDecl
	hasRoles := false
	for _, c := range plan.comps {
		mwBlocks.WriteString(strings.TrimRight(c.ComposeServices, "\n") + "\n")
		files = append(files, c.Files...)
		for _, ref := range c.Images {
			if err := e.resolveImage(ref, filepath.Join(imagesDir, safeImageName(ref)+".tar"), logf); err != nil {
				return err
			}
		}
		declComponents = append(declComponents, ComponentDecl{Code: c.Code, Category: c.Category,
			Services: composeServiceNames(c.ComposeServices), Images: c.Images,
			Endpoints: c.Endpoints, Credentials: c.Credentials, Schema: c.Schema, ConfigCenter: c.ConfigCenter,
			Roles: c.Roles, SpreadHosts: c.SpreadHosts, Resources: c.Resources})
		if len(c.Roles) > 0 {
			hasRoles = true
			logf("    组件 %s：角色化组件（%d 角色，format:4）", c.Code, len(c.Roles))
		}
		logf("    组件 %s（%s）：服务块 + %d 镜像 + %d 附件", c.Code, c.Category, len(c.Images), len(c.Files))
	}

	// 3. 编排文件
	logf("==> [3/5] 生成编排与声明")
	full := "name: " + proj + "\nservices:\n" + strings.Join(unitBlocks, "") + mwBlocks.String()
	if err := os.WriteFile(filepath.Join(pkgDir, "docker-compose.yml"), []byte(full), 0644); err != nil {
		return err
	}
	// components.json（format:3 驱动文件：入口单元 + 单元清单（含逐单元端口/健康/随包制品）+
	// 安装期变量 + 组件接口（含服务清单与镜像清单）——deliver-console 唯一消费格式）
	entry := plan.entry
	entryDecl := EntryDecl{Unit: entry.Code, Service: unitService[entry.Code], Image: unitImages[entry.Code],
		Port: orDefault(entry.Port, "8080"), HealthPath: entry.HealthPath}
	var unitDecls []UnitDecl
	for _, u := range plan.units {
		unitDecls = append(unitDecls, UnitDecl{Code: u.Code, Service: unitService[u.Code],
			Services: composeServiceNames(u.ComposeTemplate), Image: unitImages[u.Code],
			Port: u.Port, HealthPath: u.HealthPath, Artifact: unitArtifact[u.Code],
			ConfDir: u.ConfDir, ConfEnv: u.ConfEnv})
	}
	var varDecls []VarDecl
	for _, v := range app.Variables {
		if v.When != "install" {
			continue
		}
		varDecls = append(varDecls, VarDecl{Name: v.Name, Label: v.Label,
			Source: orDefault(v.Source, "input"), Value: v.Value, Sensitive: v.Sensitive})
	}
	// format:4 条件升版：包内含角色化组件即升 format:4（旧 console 拒收防静默半装）；否则恒 3
	declFormat := 3
	if hasRoles {
		declFormat = 4
	}
	decl := ComponentsDecl{Format: declFormat, Name: proj, Version: req.Version, Entry: entryDecl,
		Units: unitDecls, Variables: varDecls, Components: declComponents}
	declJSON, _ := json.MarshalIndent(decl, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "components.json"), declJSON, 0644); err != nil {
		return err
	}
	// config-template.yaml = 各单元配置模板（按序，打包时变量烘焙）+ 组件配置段（topKey 分组，追加为独立 YAML 文档）
	var tpl strings.Builder
	for _, u := range plan.units {
		if strings.TrimSpace(u.ConfigTemplate) == "" {
			continue
		}
		tpl.WriteString(strings.TrimRight(renderPlaceholders(u.ConfigTemplate, plan.buildVals), "\n") + "\n")
	}
	for _, c := range plan.comps {
		if len(c.AppConfig) == 0 {
			continue
		}
		for _, top := range sortedKeys(c.AppConfig) {
			section := strings.TrimRight(c.AppConfig[top], "\n")
			if section == "" {
				continue
			}
			tpl.WriteString("---  # 组件 " + c.Code + " 配置段\n")
			tpl.WriteString(top + ":\n" + section + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "config-template.yaml"), []byte(tpl.String()), 0644); err != nil {
		return err
	}
	// 组件附件（落包内相对路径，如 deploy/rocketmq/broker.conf）
	for _, f := range files {
		if f.Content == "" {
			continue
		}
		target := filepath.Join(pkgDir, filepath.FromSlash(f.Path))
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(pkgDir)) {
			return fmt.Errorf("组件附件路径越界: %s", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(f.Content), 0644); err != nil {
			return err
		}
	}
	// extras（附加文件，如 db/schema.sql）
	for _, id := range req.ExtraMaterialIDs {
		m, err := e.store.GetMaterial(id)
		if err != nil || m == nil {
			return fmt.Errorf("附加文件不存在: %d", id)
		}
		// 打包闸门：拦上传闸门上线前已入库的存量交付附件 zip
		if looksLikeDeliveryZip(m.StoredPath, m.FileName) {
			return fmt.Errorf("附加文件「%s」是交付附件包，不能进套件 extras/——请在『打包交付 → 交付附件』上传后经「出交付包」叠加", m.FileName)
		}
		if err := copyFile(m.StoredPath, filepath.Join(pkgDir, "extras", filepath.Base(m.FileName))); err != nil {
			return err
		}
		logf("    附加文件: %s", m.FileName)
	}

	// 单机安装脚本（可选附带）：薄壳首装通道——manifest 逐文件哈希自动覆盖，zip 打包时 .sh 置 0755
	if req.Standalone {
		sh := renderInstallSh(proj, req.Version, app, plan.comps, entry, tpl.Len() > 0, plan.buildVals)
		if err := os.WriteFile(filepath.Join(pkgDir, "install.sh"), []byte(sh), 0755); err != nil {
			return err
		}
		logf("    单机安装脚本: install.sh（校验/load/变量/配置/up/健康门控；升级/多机仍用 deliver-console）")
	}

	// 4. manifest
	logf("==> [4/5] 生成清单")
	manifest := map[string]string{}
	if err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(pkgDir, path)
		sum, err := fileSha256(path)
		if err != nil {
			return err
		}
		manifest[filepath.ToSlash(rel)] = sum
		return nil
	}); err != nil {
		return err
	}
	mj, _ := json.MarshalIndent(map[string]any{"name": proj, "version": req.Version, "files": manifest}, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.json"), mj, 0644); err != nil {
		return err
	}

	// 6. zip
	logf("==> [5/5] 压缩部署包")
	zipPath := filepath.Join(e.root, packagesDir, proj+"-"+req.Version+".zip")
	if err := zipDir(pkgDir, zipPath); err != nil {
		return err
	}
	logf("    %s", zipPath)
	return nil
}

// resolveUnitImage 解析单元镜像三源：制品现场构建 / 预构建镜像 tar / 本机镜像引用
func (e *Engine) resolveUnitImage(u *Unit, pick *UnitPick, version string, buildVals map[string]string,
	stage, imagesDir string, logf func(string, ...any)) (string, error) {
	src := pick.Source
	if src == "" {
		src = u.Source
	}
	switch src {
	case "image_tar":
		m, err := e.material(pick.MaterialID, "image_tar")
		if err != nil {
			return "", err
		}
		if err := copyFile(m.StoredPath, filepath.Join(imagesDir, filepath.Base(m.FileName))); err != nil {
			return "", err
		}
		ref := strings.TrimSuffix(filepath.Base(m.FileName), ".tar")
		logf("    单元 %s ← 镜像包: %s", u.Code, m.FileName)
		return ref, nil
	case "image_ref":
		ref := strings.TrimSpace(pick.ImageRef)
		if ref == "" {
			ref = strings.TrimSpace(u.ImageRef)
		}
		if err := e.resolveImage(ref, filepath.Join(imagesDir, safeImageName(ref)+".tar"), logf); err != nil {
			return "", err
		}
		return ref, nil
	default: // artifact
		m, err := e.materialArtifact(pick.MaterialID)
		if err != nil {
			return "", err
		}
		buildDir := filepath.Join(stage, u.Code)
		if err := os.MkdirAll(buildDir, 0755); err != nil {
			return "", err
		}
		artifact := filepath.Base(m.StoredPath)
		if err := copyFile(m.StoredPath, filepath.Join(buildDir, artifact)); err != nil {
			return "", err
		}
		// zip 应用包自动解压进构建上下文（保持 `COPY . <目标>` 模板可用；解压后移除原包不混入镜像）
		if strings.EqualFold(filepath.Ext(artifact), ".zip") {
			if err := unzipDir(filepath.Join(buildDir, artifact), buildDir); err != nil {
				return "", fmt.Errorf("单元 %s 应用包 %s 解压失败: %w", u.Code, artifact, err)
			}
			_ = os.Remove(filepath.Join(buildDir, artifact))
		}
		ref := u.ImageRepo + ":" + version
		vals := map[string]string{builtinArtifactFile: artifact, builtinUnitImage: ref, builtinUnitPort: orDefault(u.Port, "8080")}
		for k, v := range buildVals {
			vals[k] = v
		}
		dockerfile := renderPlaceholders(u.Dockerfile, vals)
		if left := leftoverPlaceholders(dockerfile); len(left) > 0 {
			return "", fmt.Errorf("单元 %s Dockerfile 引用了不可解析变量 %v（Dockerfile 仅支持单元内置与打包时变量）", u.Code, left)
		}
		if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
			return "", err
		}
		if err := dockerRun(logf, "build", "-t", ref, buildDir); err != nil {
			return "", fmt.Errorf("单元 %s 镜像构建失败: %w", u.Code, err)
		}
		if err := dockerRun(logf, "save", ref, "-o", filepath.Join(imagesDir, safeImageName(ref)+".tar")); err != nil {
			return "", err
		}
		logf("    单元 %s：%s → images/%s.tar（应用包 %s 现场构建）", u.Code, ref, safeImageName(ref), m.FileName)
		return ref, nil
	}
}

// resolveImage 组件镜像解析：本机 docker 有则 save → 本地镜像仓复制 → 报缺
func (e *Engine) resolveImage(ref, dest string, logf func(string, ...any)) error {
	if execSilent("docker", "image", "inspect", ref) == nil {
		if err := dockerRun(logf, "save", ref, "-o", dest); err != nil {
			return fmt.Errorf("镜像 %s 导出失败: %w", ref, err)
		}
		return nil
	}
	store := filepath.Join(e.root, imageStoreDir, safeImageName(ref)+".tar")
	if fileExists(store) {
		if err := copyFile(store, dest); err != nil {
			return err
		}
		logf("    镜像 %s ← 镜像仓 %s", ref, safeImageName(ref)+".tar")
		return nil
	}
	return fmt.Errorf("镜像 %s 不可得（本机 docker 无、镜像仓无 %s.tar）——请先 docker pull 后「从 docker 导入」或上传镜像仓", ref, safeImageName(ref))
}

func (e *Engine) material(id int64, kind string) (*Material, error) {
	m, err := e.store.GetMaterial(id)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Kind != kind {
		return nil, fmt.Errorf("制品不存在或类型不符（期望 %s）: id=%d", kind, id)
	}
	return m, nil
}

// materialArtifact 应用包类制品（artifact + 旧值 jar/ui_zip 兼容）
func (e *Engine) materialArtifact(id int64) (*Material, error) {
	m, err := e.store.GetMaterial(id)
	if err != nil {
		return nil, err
	}
	if m == nil || !materialIsArtifact(m.Kind) {
		return nil, fmt.Errorf("应用包不存在或类型不符（artifact/jar/ui_zip 均为应用包类）: id=%d", id)
	}
	return m, nil
}

// ---- docker / 文件工具 ----

func dockerRun(logf func(string, ...any), args ...string) error {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				logf("    %s", line)
			}
		}
	}
	return err
}

func execSilent(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// unzipDir 解压 zip 到目录（zip-slip 防护）
func unzipDir(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return fmt.Errorf("zip 条目路径非法: %s", f.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// zipDir 打包目录（.sh 置 0755；tar/zip 等已压缩件用 Store）
func zipDir(dir, zipPath string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		name := filepath.ToSlash(rel)
		method := zip.Deflate
		if regexp.MustCompile(`\.(tar|zip|gz|jar|exe)$`).MatchString(name) {
			method = zip.Store
		}
		header := &zip.FileHeader{Name: name, Method: method}
		mode := os.FileMode(0644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0755
		}
		header.SetMode(mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCodes(codes []string) []string {
	out := append([]string{}, codes...)
	sort.Strings(out)
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
