// deliver-studio 打包引擎单测：buildPlan 纯校验层（不动 docker）——三源完备性/入口/变量交叉校验。
package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planFixture(t *testing.T) (*Store, *Engine, *AssembleRequest) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 物料：假制品（buildPlan 不读文件内容，只需 DB 记录）
	jar := filepath.Join(dir, "fake.jar")
	store.InsertMaterial(&Material{Kind: "jar", FileName: "app.jar", StoredPath: jar, Size: 1})
	var jarID int64
	mats, _ := store.ListMaterials("")
	for _, m := range mats {
		if m.Kind == "jar" {
			jarID = m.ID
		}
	}
	app := &App{Code: "demo", Name: "演示", DefaultVersion: "1.0.0",
		Units: []Unit{
			{Code: "api", Name: "后端", Role: "backend", Entrypoint: true, Source: "artifact",
				ImageRepo: "mse/api", Dockerfile: "FROM x\nCOPY {{ARTIFACT_FILE}} app.jar",
				Port: "8080", ConfigTemplate: "mse:\n  tenant: {{TENANT_ID}}\n  db: {{DB_HOST}}\n",
				ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n    ports:\n      - \"{{UNIT_PORT}}:{{UNIT_PORT}}\"\n"},
			{Code: "web", Name: "前端", Role: "frontend", Source: "image_ref", ImageRef: "mse/web:1.0",
				ComposeTemplate: "  web:\n    image: {{UNIT_IMAGE}}\n    depends_on:\n      - api\n"},
		},
		Variables: []Variable{
			{Name: "TENANT_ID", Label: "租户", When: "install", Source: "input", Value: "mse"},
			{Name: "BUILD_TAG", Label: "构建标签", When: "build", Source: "input", Value: ""},
		}}
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	comp := &Component{Code: "mysql", Category: "database", Images: []string{"mysql:8.4"},
		ComposeServices: "  mysql:\n    image: mysql:8.4\n",
		Endpoints:       []Endpoint{{Placeholder: "DB_HOST", Service: "mysql", Port: "3306", From: "host"}}}
	if err := store.UpsertComponent(comp); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(dir, store)
	req := &AssembleRequest{Name: "demo-kit", Version: "1.0.0", AppCode: "demo",
		Units: []UnitPick{
			{Code: "api", Source: "artifact", MaterialID: jarID},
			{Code: "web", Source: "image_ref", ImageRef: "mse/web:1.0"},
		},
		ComponentCodes: []string{"mysql"},
		BuildVars:      map[string]string{"BUILD_TAG": "prod"}}
	return store, engine, req
}

func TestBuildPlanHappyPath(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.entry.Code != "api" {
		t.Fatalf("入口单元错误: %s", plan.entry.Code)
	}
	if plan.buildVals["BUILD_TAG"] != "prod" {
		t.Fatalf("打包期变量应取请求值: %v", plan.buildVals)
	}
	if !plan.knownVars["DB_HOST"] || !plan.installSet["DB_HOST"] {
		t.Fatal("组件端点占位符应进已知集合")
	}
	if !plan.knownVars["UNIT_IMAGE"] {
		t.Fatal("单元内置占位符应进已知集合")
	}
}

func TestBuildPlanValidations(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()

	cases := []struct {
		name string
		mut  func(r *AssembleRequest)
		want string
	}{
		{"单元全不选", func(r *AssembleRequest) { r.Units = nil }, "至少要选择一个单元"},
		{"未知单元编码", func(r *AssembleRequest) {
			r.Units = append(r.Units, UnitPick{Code: "ghost", Source: "image_ref", ImageRef: "x"})
		}, "不在应用"},
		{"应用包未选", func(r *AssembleRequest) { r.Units[0].MaterialID = 0 }, "未选择应用包"},
		{"单元选择重复", func(r *AssembleRequest) {
			r.Units = append(r.Units, UnitPick{Code: "api", Source: "image_ref", ImageRef: "x"})
		}, "重复"},
		{"未知组件", func(r *AssembleRequest) { r.ComponentCodes = []string{"nope"} }, "组件不存在"},
		{"打包变量缺值", func(r *AssembleRequest) { r.BuildVars = map[string]string{} }, "未赋值"},
	}
	for _, c := range cases {
		r := *req
		r.Units = append([]UnitPick{}, req.Units...)
		r.ComponentCodes = append([]string{}, req.ComponentCodes...)
		c.mut(&r)
		_, err := engine.buildPlan(&r)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 期望错误含 %q，实际: %v", c.name, c.want, err)
		}
	}
}

// 配置模板引用未勾选组件的占位符 → 硬拦截
func TestBuildPlanUnknownPlaceholder(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	req.ComponentCodes = []string{} // 去掉 mysql，DB_HOST 变未知
	_, err := engine.buildPlan(req)
	if err == nil || !strings.Contains(err.Error(), "DB_HOST") {
		t.Fatalf("应拦截未定义占位符 DB_HOST: %v", err)
	}
}

// depends_on 引用未勾选组件的服务 → 硬拦截（与图形编排画布连线口径一致的兜底校验）
func TestBuildPlanDanglingDependsOn(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	// api 单元声明 depends_on: mysql，但不勾选 mysql 组件
	app, err := store.GetApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	app.Units[0].ComposeTemplate = "  api:\n    image: {{UNIT_IMAGE}}\n    depends_on:\n      - mysql\n"
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes = []string{}
	_, err = engine.buildPlan(req)
	if err == nil || !strings.Contains(err.Error(), "depends_on 引用了不存在的服务 mysql") {
		t.Fatalf("应拦截悬空 depends_on: %v", err)
	}
}

// 跨块 depends_on（单元 → 已勾选组件服务）→ 正常通过
func TestBuildPlanCrossBlockDependsOn(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	app, err := store.GetApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	app.Units[0].ComposeTemplate = "  api:\n    image: {{UNIT_IMAGE}}\n    depends_on:\n      - mysql\n"
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("跨块 depends_on 应通过: %v", err)
	}
}

// 配置中心声明校验：基线文件必须在组件附件内、身份头占位符须为本组件凭据、空基线拦截
func TestValidateConfigCenter(t *testing.T) {
	base := &Component{Code: "nacos", ComposeServices: "  nacos:\n    image: nacos/nacos-server:v3.0.0\n",
		Files:       []FileDecl{{Path: "nacos/configs/app.yaml", Content: "mse:\n  a: 1\n"}},
		Credentials: []Credential{{Placeholder: "NACOS_IDENTITY_VALUE", Generate: "const", Value: "security"}}}
	cc := func() *ComponentConfigCenter {
		return &ComponentConfigCenter{Kind: "nacos", Plugin: "nacos-config", IdentityValue: "NACOS_IDENTITY_VALUE",
			Configs: []ConfigItemDecl{{DataID: "app.yaml", File: "nacos/configs/app.yaml"}}}
	}
	c := *base
	c.ConfigCenter = cc()
	if err := validateConfigCenter(&c); err != nil {
		t.Fatalf("合法声明应通过: %v", err)
	}

	c = *base
	c.ConfigCenter = cc()
	c.ConfigCenter.Configs = nil
	if err := validateConfigCenter(&c); err == nil || !strings.Contains(err.Error(), "基线清单为空") {
		t.Fatalf("空基线应拦截: %v", err)
	}

	c = *base
	c.ConfigCenter = cc()
	c.ConfigCenter.Configs[0].File = "nacos/configs/missing.yaml"
	if err := validateConfigCenter(&c); err == nil || !strings.Contains(err.Error(), "不在组件附件") {
		t.Fatalf("基线文件缺失应拦截: %v", err)
	}

	c = *base
	c.ConfigCenter = cc()
	c.ConfigCenter.IdentityValue = "NOT_A_CRED"
	if err := validateConfigCenter(&c); err == nil || !strings.Contains(err.Error(), "不是本组件凭据") {
		t.Fatalf("身份头占位符未登记凭据应拦截: %v", err)
	}

	c = *base
	c.ConfigCenter = cc()
	c.ConfigCenter.Kind = "apollo"
	if err := validateConfigCenter(&c); err == nil || !strings.Contains(err.Error(), "nacos") {
		t.Fatalf("未知 kind 应拦截: %v", err)
	}

	// 打包链路：勾选带合法 configCenter 的组件通过，非法组件拦截
	store, engine, req := planFixture(t)
	defer store.Close()
	ok := &Component{Code: "nacos-ok", ComposeServices: "  nacos:\n    image: nacos/nacos-server:v3.0.0\n",
		Files:       []FileDecl{{Path: "nacos/configs/app.yaml", Content: "a: 1\n"}},
		Credentials: []Credential{{Placeholder: "NACOS_IDENTITY_VALUE", Generate: "const", Value: "security"}},
		ConfigCenter: &ComponentConfigCenter{Kind: "nacos", Plugin: "nacos-config",
			Configs: []ConfigItemDecl{{DataID: "app.yaml", File: "nacos/configs/app.yaml"}}}}
	if err := store.UpsertComponent(ok); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes = append(req.ComponentCodes, "nacos-ok")
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("合法 configCenter 组件打包校验应通过: %v", err)
	}
	bad := *ok
	bad.Code = "nacos-bad"
	bad.ConfigCenter = &ComponentConfigCenter{Kind: "nacos", Plugin: "nacos-config"} // 空 configs
	if err := store.UpsertComponent(&bad); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes[len(req.ComponentCodes)-1] = "nacos-bad"
	_, err := engine.buildPlan(req)
	if err == nil || !strings.Contains(err.Error(), "配置中心声明无效") {
		t.Fatalf("非法 configCenter 组件应拦截打包: %v", err)
	}
}

// 单机部署（随包 install.sh）边界：勾选配置中心组件硬拦截；无配置中心组合通过
func TestBuildPlanStandaloneConfigCenterRejected(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	cc := &Component{Code: "nacos-cc", ComposeServices: "  nacos:\n    image: nacos/nacos-server:v3.0.0\n",
		Files:       []FileDecl{{Path: "nacos/configs/app.yaml", Content: "a: 1\n"}},
		Credentials: []Credential{{Placeholder: "NACOS_IDENTITY_VALUE", Generate: "const", Value: "security"}},
		ConfigCenter: &ComponentConfigCenter{Kind: "nacos", Plugin: "nacos-config",
			Configs: []ConfigItemDecl{{DataID: "app.yaml", File: "nacos/configs/app.yaml"}}}}
	if err := store.UpsertComponent(cc); err != nil {
		t.Fatal(err)
	}

	// 无配置中心（仅 mysql）：standalone 通过
	req.Standalone = true
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("无配置中心组件的单机打包应通过: %v", err)
	}

	// 勾选配置中心组件：standalone 拦截，非 standalone 通过
	req.ComponentCodes = append(req.ComponentCodes, "nacos-cc")
	_, err := engine.buildPlan(req)
	if err == nil || !strings.Contains(err.Error(), "单机部署") {
		t.Fatalf("standalone×configCenter 应拦截: %v", err)
	}
	req.Standalone = false
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("非 standalone 勾选 configCenter 应通过: %v", err)
	}
}

// 单机包装配端到端（免 docker：单元走 image_tar 纯复制 + 组件不带 images）：
// install.sh 落包、manifest 纳管、zip 内执行位 0755；未勾选 standalone 则三者皆无
func TestAssembleStandaloneInstallSh(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tarPath := filepath.Join(dir, "mse-api.tar")
	if err := os.WriteFile(tarPath, []byte("fake-tar"), 0644); err != nil {
		t.Fatal(err)
	}
	store.InsertMaterial(&Material{Kind: "image_tar", FileName: "mse-api.tar", StoredPath: tarPath, Size: 8})
	var tarID int64
	mats, _ := store.ListMaterials("image_tar")
	if len(mats) != 1 {
		t.Fatal("镜像包物料未落库")
	}
	tarID = mats[0].ID
	app := &App{Code: "sapp", Name: "单机演示", DefaultVersion: "1.0.0",
		Units: []Unit{
			{Code: "api", Name: "后端", Role: "backend", Entrypoint: true, Source: "image_tar",
				Port: "8080", HealthPath: "/actuator/health", ConfigTemplate: "mse:\n  db: {{DB_HOST}}\n",
				ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n    ports:\n      - \"18080:8080\"\n"},
		},
		Variables: []Variable{{Name: "TENANT_ID", When: "install", Source: "input", Value: "mse"}}}
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	// 组件零镜像（免 docker）：凭据 + 端点仍进 install.sh 烘焙
	comp := &Component{Code: "redis", Category: "cache",
		ComposeServices: "  redis:\n    image: redis:7\n",
		Endpoints:       []Endpoint{{Placeholder: "DB_HOST", Service: "redis", Port: "6379", From: "host"}}}
	if err := store.UpsertComponent(comp); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(dir, store)
	req := &AssembleRequest{Name: "s-e2e", Version: "1.0.0", AppCode: "sapp", Standalone: true,
		Units:          []UnitPick{{Code: "api", Source: "image_tar", MaterialID: tarID}},
		ComponentCodes: []string{"redis"}}
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.assemble(req, plan, func(string, ...any) {}); err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	pkgDir := filepath.Join(dir, packagesDir, "s-e2e-1.0.0")
	sh, err := os.ReadFile(filepath.Join(pkgDir, "install.sh"))
	if err != nil {
		t.Fatalf("install.sh 未落包: %v", err)
	}
	for _, want := range []string{"# s-e2e v1.0.0 单机安装脚本", "wait_http 'api' 'http://127.0.0.1:18080/actuator/health' 600", "V_DB_HOST='redis'", "prompt_var TENANT_ID"} {
		if !strings.Contains(string(sh), want) {
			t.Errorf("install.sh 缺关键段: %q", want)
		}
	}
	// manifest 纳管
	mj, err := os.ReadFile(filepath.Join(pkgDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mj), `"install.sh"`) {
		t.Error("manifest.json 应含 install.sh 哈希")
	}
	// zip 执行位 0755（.sh 规则）
	zr, err := zip.OpenReader(filepath.Join(dir, packagesDir, "s-e2e-1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		if f.Name == "install.sh" {
			found = true
			if f.Mode().Perm() != 0755 {
				t.Errorf("zip 内 install.sh 权限应为 0755，实际 %v", f.Mode().Perm())
			}
		}
	}
	if !found {
		t.Error("zip 内缺 install.sh")
	}

	// 对照组：未勾选 standalone 的包不含 install.sh
	req2 := *req
	req2.Standalone = false
	req2.Name = "s-e2e-plain"
	plan2, err := engine.buildPlan(&req2)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.assemble(&req2, plan2, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, packagesDir, "s-e2e-plain-1.0.0", "install.sh")); !os.IsNotExist(err) {
		t.Error("未勾选单机部署不应附带 install.sh")
	}
}

// ---- pick 子集语义（微服务形态按业务裁剪单元）与配置基线 ----

// 子集 pick：web 不进包也能成包；进包单元数与入口随之收敛（全 pick 行为不变，见 HappyPath）
func TestBuildPlanUnitSubset(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	req.Units = req.Units[:1] // 只 pick api（entrypoint）
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.units) != 1 || plan.units[0].Code != "api" {
		t.Fatalf("进包单元应为 [api]: %v", plan.units)
	}
	if plan.entry.Code != "api" {
		t.Fatalf("入口单元 = %s", plan.entry.Code)
	}
}

// configCenterInclude 白名单过滤 + 基线占位符必须是安装期可解析项（未知硬拦）
func TestBuildPlanConfigCenterInclude(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	nacos := &Component{Code: "nacos", Category: "middleware", Images: []string{"nacos/nacos-server:v3.0.0"},
		ComposeServices: "  nacos:\n    image: nacos/nacos-server:v3.0.0\n",
		Endpoints:       []Endpoint{{Placeholder: "NACOS_ADDR", Service: "nacos", Port: "8848", From: "url", Scheme: "http"}},
		Files: []FileDecl{
			{Path: "deploy/nacos/mse-micro-common.yaml", Content: "spring:\n  db: {{DB_HOST}}\n"},
			{Path: "deploy/nacos/mse-oa.yaml", Content: "server:\n  port: 8090\n"},
		},
		ConfigCenter: &ComponentConfigCenter{Kind: "nacos", Plugin: "nacos-config",
			Configs: []ConfigItemDecl{
				{DataID: "mse-micro-common.yaml", File: "deploy/nacos/mse-micro-common.yaml"},
				{DataID: "mse-oa.yaml", File: "deploy/nacos/mse-oa.yaml"},
			}}}
	if err := store.UpsertComponent(nacos); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes = append(req.ComponentCodes, "nacos")
	// include 过滤：只保留共享基线（mse-oa.yaml 不落包）
	req.ConfigCenterInclude = []string{"mse-micro-common.yaml"}
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.comps {
		if c.Code != "nacos" {
			continue
		}
		if len(c.ConfigCenter.Configs) != 1 || c.ConfigCenter.Configs[0].DataID != "mse-micro-common.yaml" {
			t.Fatalf("configCenterInclude 应裁剪 configs: %+v", c.ConfigCenter.Configs)
		}
	}
	// 过滤后全空 → 硬拦（共享基线应保留在清单内）
	req.ConfigCenterInclude = []string{"mse-ghost.yaml"}
	if _, err := engine.buildPlan(req); err == nil || !strings.Contains(err.Error(), "过滤后为空") {
		t.Fatalf("include 全过滤应报错: %v", err)
	}
	// 基线占位符校验：未知占位符（打包期变量/未定义名）硬拦
	req.ConfigCenterInclude = []string{"mse-micro-common.yaml"}
	nacos.Files = []FileDecl{
		{Path: "deploy/nacos/mse-micro-common.yaml", Content: "x: {{BUILD_TAG}}\n"},
		{Path: "deploy/nacos/mse-oa.yaml", Content: "server:\n  port: 8090\n"},
	}
	if err := store.UpsertComponent(nacos); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.buildPlan(req); err == nil || !strings.Contains(err.Error(), "BUILD_TAG") {
		t.Fatalf("基线引用打包期变量应报错: %v", err)
	}
}

// ---- format:4 角色化组件（MySQL 主从等）：buildPlan 校验矩阵 + 装配产物 ----

func rolesComponent() *Component {
	return &Component{Code: "mysql-ha", Category: "database",
		ComposeServices: "  mysql-primary:\n    image: mysql:8.4\n  mysql-replica:\n    image: mysql:8.4\n",
		Endpoints:  []Endpoint{{Placeholder: "DB_HOST", Service: "mysql-primary", Port: "3306", From: "host"}},
		SpreadHosts: true,
		Roles: []Role{
			{Name: "primary", Services: []string{"mysql-primary"}, Vars: map[string]string{"MYSQL_SERVER_ID": "1"},
				Files:     []RoleFile{{Path: "data/mysql-primary/conf.d/my.cnf", Template: "[mysqld]\nserver-id={{MYSQL_SERVER_ID}}\n"}},
				Readiness: &Probe{Type: "tcp", Port: "3306"},
				Init:      []InitStep{{ID: "repl-user", Script: "mysql -e 'CREATE USER'"}}},
			{Name: "replica", Services: []string{"mysql-replica"},
				Files:     []RoleFile{{Path: "data/mysql-replica/conf.d/my.cnf", Template: "[mysqld]\nserver-id=2\n"}},
				Readiness: &Probe{Type: "exec", Script: "mysql -e 'SHOW REPLICA STATUS'"},
				Init:      []InitStep{{ID: "join", After: "primary", Script: "mysql -e 'CHANGE REPLICATION SOURCE'"}}},
		}}
}

func TestBuildPlanRolesValidations(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	// fixture 自带 mysql 组件（DB_HOST 端点）——换掉组件选择，避免端点冲突干扰本用例
	if err := store.UpsertComponent(rolesComponent()); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes = []string{"mysql-ha"}
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("合法角色化组件应通过: %v", err)
	}
	// standalone × 角色化组件互斥
	sa := *req
	sa.Standalone = true
	if _, err := engine.buildPlan(&sa); err == nil || !strings.Contains(err.Error(), "单机部署") {
		t.Fatalf("standalone×roles 应硬拦: %v", err)
	}
	// 角色服务不在编排块
	bad := rolesComponent()
	bad.Roles[0].Services = []string{"mysql-x"}
	if err := store.UpsertComponent(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.buildPlan(req); err == nil || !strings.Contains(err.Error(), "不在编排服务块") {
		t.Fatalf("角色服务缺块应报错: %v", err)
	}
	// spreadHosts 单服务无意义
	single := rolesComponent()
	single.ComposeServices = "  mysql-primary:\n    image: mysql:8.4\n"
	single.Roles = []Role{{Name: "primary", Services: []string{"mysql-primary"}, Init: []InitStep{{ID: "x", Script: "true"}}}}
	if err := store.UpsertComponent(single); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.buildPlan(req); err == nil || !strings.Contains(err.Error(), "反亲和") {
		t.Fatalf("spreadHosts 单服务应报错: %v", err)
	}
	// 端点占位符跨组件冲突（mysql × mysql-ha 同供 DB_HOST）
	if err := store.UpsertComponent(rolesComponent()); err != nil {
		t.Fatal(err)
	}
	req.ComponentCodes = []string{"mysql", "mysql-ha"}
	if _, err := engine.buildPlan(req); err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("同端点占位符双组件应报错: %v", err)
	}
}

// format:4 条件升版：含角色化组件 → format=4 + roles/spreadHosts 透传；无角色恒 3
func TestAssembleFormatConditional(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tarPath := filepath.Join(dir, "mse-api.tar")
	if err := os.WriteFile(tarPath, []byte("fake-tar"), 0644); err != nil {
		t.Fatal(err)
	}
	store.InsertMaterial(&Material{Kind: "image_tar", FileName: "mse-api.tar", StoredPath: tarPath, Size: 8})
	var tarID int64
	mats, _ := store.ListMaterials("image_tar")
	tarID = mats[0].ID
	app := &App{Code: "ha", Name: "主从演示", DefaultVersion: "1.0.0",
		Units: []Unit{
			{Code: "api", Name: "后端", Role: "backend", Entrypoint: true, Source: "image_tar",
				Port: "8080", ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n    ports:\n      - \"18080:8080\"\n"},
		},
		Variables: []Variable{{Name: "TENANT_ID", When: "install", Source: "input", Value: "mse"}}}
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertComponent(rolesComponent()); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(dir, store)
	req := &AssembleRequest{Name: "ha-e2e", Version: "1.0.0", AppCode: "ha",
		Units:          []UnitPick{{Code: "api", Source: "image_tar", MaterialID: tarID}},
		ComponentCodes: []string{"mysql-ha"}}
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.assemble(req, plan, func(string, ...any) {}); err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, packagesDir, "ha-e2e-1.0.0", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decl ComponentsDecl
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatal(err)
	}
	if decl.Format != 4 {
		t.Errorf("含角色化组件应升 format:4，实际 %d", decl.Format)
	}
	if len(decl.Components) != 1 || len(decl.Components[0].Roles) != 2 || !decl.Components[0].SpreadHosts {
		t.Fatalf("roles/spreadHosts 未透传: %+v", decl.Components)
	}
	if decl.Components[0].Roles[1].Init[0].After != "primary" {
		t.Errorf("init After 未透传: %+v", decl.Components[0].Roles[1].Init)
	}
	// 对照组：无角色组件恒 format:3（旧 console 可装）
	plain := &Component{Code: "redis", Category: "cache",
		ComposeServices: "  redis:\n    image: redis:7\n"}
	if err := store.UpsertComponent(plain); err != nil {
		t.Fatal(err)
	}
	req2 := &AssembleRequest{Name: "ha-plain", Version: "1.0.0", AppCode: "ha",
		Units:          []UnitPick{{Code: "api", Source: "image_tar", MaterialID: tarID}},
		ComponentCodes: []string{"redis"}}
	plan2, err := engine.buildPlan(req2)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.assemble(req2, plan2, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(filepath.Join(dir, packagesDir, "ha-plain-1.0.0", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decl2 ComponentsDecl
	_ = json.Unmarshal(raw2, &decl2)
	if decl2.Format != 3 {
		t.Errorf("无角色组件应保持 format:3，实际 %d", decl2.Format)
	}
}

// zip 应用包 × {{ARTIFACT_FILE}} 整包引用互斥：zip 解压为上下文原包不保留，后端 fat-jar 模板
// COPY {{ARTIFACT_FILE}} 必在 docker build 才爆（实测包 #10）——打包期前置拦截并指路
func TestBuildPlanZipArtifactWithWholeFileRef(t *testing.T) {
	store, engine, req := planFixture(t)
	defer store.Close()
	zipPath := filepath.Join(t.TempDir(), "mse-ui-1.0.1.zip")
	if err := os.WriteFile(zipPath, []byte("fake-zip"), 0644); err != nil {
		t.Fatal(err)
	}
	store.InsertMaterial(&Material{Kind: "artifact", FileName: "mse-ui-1.0.1.zip", StoredPath: zipPath, Size: 8})
	var zipID int64
	mats, _ := store.ListMaterials("")
	for _, m := range mats {
		if strings.HasSuffix(m.FileName, ".zip") {
			zipID = m.ID
		}
	}
	if zipID == 0 {
		t.Fatal("zip 材料未落库")
	}
	// api 单元 Dockerfile 为 COPY {{ARTIFACT_FILE}} app.jar（整包引用）→ 选 zip 应用包应硬拦
	req.Units = []UnitPick{{Code: "api", Source: "artifact", MaterialID: zipID},
		{Code: "web", Source: "image_ref", ImageRef: "mse/web:1.0"}}
	_, err := engine.buildPlan(req)
	if err == nil || !strings.Contains(err.Error(), "zip 应用包") {
		t.Fatalf("zip×整包引用应打包期硬拦，实际 err=%v", err)
	}
	// jar 材料（非 zip）不受影响
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("fake-jar"), 0644); err != nil {
		t.Fatal(err)
	}
	store.InsertMaterial(&Material{Kind: "jar", FileName: "app.jar", StoredPath: jarPath, Size: 8})
	var jarID int64
	mats, _ = store.ListMaterials("")
	for _, m := range mats {
		if strings.HasSuffix(m.FileName, ".jar") {
			jarID = m.ID
		}
	}
	req.Units = []UnitPick{{Code: "api", Source: "artifact", MaterialID: jarID},
		{Code: "web", Source: "image_ref", ImageRef: "mse/web:1.0"}}
	if _, err := engine.buildPlan(req); err != nil {
		t.Fatalf("jar 材料应通过：%v", err)
	}
}
