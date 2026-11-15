// deliver-studio 模型 v2 单测：旧格式归一化（collect-kit 推旧预设/旧库迁移共用路径）、
// 应用校验、compose 服务块解析、占位符渲染。
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 旧格式应用 JSON（与 collect-kit 旧版 studio-preset.json 同构）
const legacyAppJSON = `{
  "code": "mse-monolith", "name": "ai-mse 租户单体", "defaultVersion": "1.0.0",
  "port": "8080", "healthPath": "/actuator/health",
  "imageRepo": "mse/mse-monolith",
  "jarDockerfile": "FROM eclipse-temurin:21-jre-jammy\nCOPY {{JAR_FILE}} app.jar",
  "uiEnabled": true, "uiImageRepo": "mse/mse-nginx",
  "uiDockerfile": "FROM nginx:stable-alpine\nCOPY . /usr/share/nginx/html",
  "appComposeTemplate": "  app:\n    image: {{APP_IMAGE}}\n    ports:\n      - \"8080:8080\"\n    volumes:\n      - ./data/conf:/app/conf\n  nginx:\n    image: {{UI_IMAGE}}\n    ports:\n      - \"80:80\"\n    depends_on:\n      - app\n",
  "confDir": "/app/conf",
  "confEnv": {"SPRING_CONFIG_ADDITIONAL_LOCATION": "file:/app/conf/"},
  "configTemplate": "mse:\n  tenant-id: {{TENANT_ID}}\n",
  "remark": "旧格式"
}`

func TestUnmarshalLegacyAppNormalizes(t *testing.T) {
	var a App
	if err := json.Unmarshal([]byte(legacyAppJSON), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Units) != 2 {
		t.Fatalf("应归一为 2 个单元（后端+前端），实际 %d", len(a.Units))
	}
	be := a.Units[0]
	fe := a.Units[1]
	if be.Code != "app" || !be.Entrypoint || be.Role != "backend" {
		t.Fatalf("后端单元归一错误: %+v", be)
	}
	if fe.Code != "nginx" || fe.Role != "frontend" || fe.Entrypoint {
		t.Fatalf("前端单元归一错误（编码应跟随服务名 nginx、非入口）: %+v", fe)
	}
	if !strings.Contains(be.Dockerfile, "{{ARTIFACT_FILE}}") || strings.Contains(be.Dockerfile, "{{JAR_FILE}}") {
		t.Fatalf("后端 Dockerfile 占位符未转换: %s", be.Dockerfile)
	}
	if !strings.Contains(be.ComposeTemplate, "{{UNIT_IMAGE}}") || strings.Contains(be.ComposeTemplate, "{{APP_IMAGE}}") {
		t.Fatalf("后端编排块占位符未转换: %s", be.ComposeTemplate)
	}
	if !strings.Contains(fe.ComposeTemplate, "{{UNIT_IMAGE}}") || strings.Contains(fe.ComposeTemplate, "{{UI_IMAGE}}") {
		t.Fatalf("前端编排块占位符未转换: %s", fe.ComposeTemplate)
	}
	if be.ConfDir != "/app/conf" || be.ConfEnv["SPRING_CONFIG_ADDITIONAL_LOCATION"] == "" {
		t.Fatalf("配置挂载未继承到后端单元: %+v", be)
	}
	if !strings.Contains(be.ConfigTemplate, "{{TENANT_ID}}") {
		t.Fatalf("配置模板未继承到后端单元")
	}
	// 归一结果应通过应用校验
	if err := validateApp(&a); err != nil {
		t.Fatalf("归一结果未通过校验: %v", err)
	}
	// v2 序列化应干净（无旧字段）
	out, _ := json.Marshal(&a)
	for _, legacy := range []string{"uiEnabled", "jarDockerfile", "appComposeTemplate", "{{APP_IMAGE}}"} {
		if strings.Contains(string(out), legacy) {
			t.Fatalf("v2 序列化仍含旧字段/旧占位符 %s: %s", legacy, out)
		}
	}
}

func TestUnmarshalV2AppRoundTrip(t *testing.T) {
	a := App{Code: "demo", Name: "演示", DefaultVersion: "1.0.0",
		Units: []Unit{{Code: "api", Role: "backend", Entrypoint: true, Source: "artifact",
			ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"}},
		Variables: []Variable{{Name: "TENANT_ID", Label: "租户", When: "install", Source: "input", Value: "mse"}}}
	data, _ := json.Marshal(&a)
	var b App
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Units) != 1 || b.Units[0].Code != "api" || len(b.Variables) != 1 {
		t.Fatalf("v2 往返失真: %+v", b)
	}
}

func TestValidateAppRules(t *testing.T) {
	cases := []struct {
		name string
		app  App
		want string // 期望错误关键词，空=应通过
	}{
		{"无单元", App{Code: "demo"}, "服务单元"},
		{"单元编码重复", App{Code: "demo", Units: []Unit{
			{Code: "api", Entrypoint: true, ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"},
			{Code: "api", ComposeTemplate: "  api2:\n    image: {{UNIT_IMAGE}}\n"}}}, "重复"},
		{"双入口", App{Code: "demo", Units: []Unit{
			{Code: "api", Entrypoint: true, ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"},
			{Code: "web", Entrypoint: true, ComposeTemplate: "  web:\n    image: {{UNIT_IMAGE}}\n"}}}, "主入口"},
		{"服务名冲突", App{Code: "demo", Units: []Unit{
			{Code: "api", Entrypoint: true, ComposeTemplate: "  svc:\n    image: {{UNIT_IMAGE}}\n"},
			{Code: "web", ComposeTemplate: "  svc:\n    image: {{UNIT_IMAGE}}\n"}}}, "重复"},
		{"缺编排块", App{Code: "demo", Units: []Unit{{Code: "api", Entrypoint: true}}}, "编排服务块"},
		{"编排缩进非法", App{Code: "demo", Units: []Unit{{Code: "api", Entrypoint: true,
			ComposeTemplate: "  api:\n   image: x\n"}}}, "缩进"},
		{"变量名非法", App{Code: "demo", Units: []Unit{{Code: "api", Entrypoint: true,
			ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"}},
			Variables: []Variable{{Name: "tenant-id", When: "install", Source: "input"}}}, "大写蛇形"},
	}
	for _, c := range cases {
		err := validateApp(&c.app)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: 应通过但报错: %v", c.name, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 期望错误含 %q，实际: %v", c.name, c.want, err)
		}
	}
}

func TestComposeServiceNames(t *testing.T) {
	block := "  mysql:\n    image: mysql:8.4\n  redis:\n    image: redis:7\n"
	names := composeServiceNames(block)
	if len(names) != 2 || names[0] != "mysql" || names[1] != "redis" {
		t.Fatalf("服务名解析错误: %v", names)
	}
	if firstComposeServiceName(block) != "mysql" {
		t.Fatalf("首个服务名错误")
	}
}

func TestRenderAndLeftover(t *testing.T) {
	out := renderPlaceholders("image: {{UNIT_IMAGE}} port={{UNIT_PORT}} keep={{INSTALL_VAR}}",
		map[string]string{"UNIT_IMAGE": "mse/app:1.0.0", "UNIT_PORT": "8080"})
	if !strings.Contains(out, "mse/app:1.0.0") || !strings.Contains(out, "port=8080") {
		t.Fatalf("渲染错误: %s", out)
	}
	left := leftoverPlaceholders(out)
	if len(left) != 1 || left[0] != "INSTALL_VAR" {
		t.Fatalf("残留占位符解析错误: %v", left)
	}
}

// 组件 schema 声明（SQL 预置）JSON 往返：预设导入 → Component.Schema 保留 → components.json 透传
func TestComponentSchemaRoundTrip(t *testing.T) {
	const presetJSON = `{"components": [{
	  "code": "mysql", "name": "MySQL 8.4", "category": "database", "images": ["mysql:8.4"],
	  "schema": {"dir": "extras", "image": "mysql:8.4", "host": "DB_HOST", "port": "DB_PORT",
	             "user": "DB_USER", "password": "DB_PASSWORD", "database": "DB_NAME"}
	}]}`
	var p struct {
		Components []Component `json:"components"`
	}
	if err := json.Unmarshal([]byte(presetJSON), &p); err != nil {
		t.Fatal(err)
	}
	s := p.Components[0].Schema
	if s == nil || s.Dir != "extras" || s.Image != "mysql:8.4" || s.Host != "DB_HOST" || s.Database != "DB_NAME" {
		t.Fatalf("schema 声明解析不符: %+v", s)
	}
	out, _ := json.Marshal(ComponentDecl{Code: "mysql", Schema: s})
	if !strings.Contains(string(out), `"schema":{"dir":"extras","image":"mysql:8.4"`) {
		t.Fatalf("components.json 未透传 schema: %s", out)
	}
}

// composeDependsOn 解析：块列表与 flow 列表两种写法（与前端 composeYaml.modelEdges 同口径）
func TestComposeDependsOn(t *testing.T) {
	blockList := "  web:\n    depends_on:\n      - app\n  app:\n    image: x\n"
	got := composeDependsOn(blockList)
	if len(got["web"]) != 1 || got["web"][0] != "app" {
		t.Fatalf("块列表解析错误: %v", got)
	}
	if len(got["app"]) != 0 {
		t.Fatalf("无依赖服务应为空: %v", got)
	}
	flow := "  a:\n    depends_on: [b, c]\n  b:\n    image: x\n"
	got = composeDependsOn(flow)
	if len(got["a"]) != 2 || got["a"][0] != "b" || got["a"][1] != "c" {
		t.Fatalf("flow 列表解析错误: %v", got)
	}
}
