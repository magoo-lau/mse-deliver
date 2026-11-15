// install.sh 薄壳生成器单测：端口映射解析 / 端点单机取值 / 烘焙内容关键段。
package main

import (
	"strings"
	"testing"
)

func TestComposeHostPort(t *testing.T) {
	block := "  api:\n    image: x\n    ports:\n      - \"18080:8080\"\n      - 127.0.0.1:8081:8081\n      - \"9000:9000/udp\"\n    environment:\n      - A=1\n"
	if got := composeHostPort(block, "8080"); got != "18080" {
		t.Fatalf("host:container 解析错误: %s", got)
	}
	if got := composeHostPort(block, "8081"); got != "8081" { // ip:host:container → host
		t.Fatalf("ip:host:container 解析错误: %s", got)
	}
	if got := composeHostPort(block, "9000"); got != "" { // 协议后缀不识别 → 降级
		t.Fatalf("带协议后缀应返回空走降级: %s", got)
	}
	if got := composeHostPort(block, "7000"); got != "" {
		t.Fatalf("未发布端口应返回空: %s", got)
	}
	if got := composeHostPort("  api:\n    image: x\n", "8080"); got != "" {
		t.Fatalf("无 ports 段应返回空: %s", got)
	}
}

func TestEndpointStandaloneValue(t *testing.T) {
	cases := []struct {
		ep   Endpoint
		want string
	}{
		{Endpoint{Placeholder: "DB_PORT", Service: "mysql", Port: "3306", From: "port"}, "3306"},
		{Endpoint{Placeholder: "DB_HOST", Service: "mysql", Port: "3306", From: "host"}, "mysql"},
		{Endpoint{Placeholder: "DB_URL", Service: "mysql", Port: "3306"}, "mysql:3306"},
		{Endpoint{Placeholder: "REDIS_URL", Service: "redis", Port: "6379", Scheme: "redis"}, "redis://redis:6379"},
	}
	for _, c := range cases {
		if got := endpointStandaloneValue(c.ep); got != c.want {
			t.Errorf("%s 单机取值错误: got %s want %s", c.ep.Placeholder, got, c.want)
		}
	}
}

// renderInstallSh 烘焙内容：健康 URL / 变量与凭据三来源 / 端点常量 / .env / 配置替换 / 未发布端口降级 / 入口汇总
func TestRenderInstallShContents(t *testing.T) {
	app := &App{Code: "demo", Variables: []Variable{
		{Name: "TENANT_ID", Label: "租户", When: "install", Source: "input", Value: "mse"},
		{Name: "SITE_NAME", When: "install", Source: "const", Value: "主站"},
		{Name: "APP_SECRET", When: "install", Source: "random"},
		{Name: "BUILD_TAG", When: "build", Source: "input", Value: "prod"},
	}, Units: []Unit{
		{Code: "api", Entrypoint: true, Port: "8080", HealthPath: "/actuator/health",
			ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n    ports:\n      - \"{{UNIT_PORT}}:{{UNIT_PORT}}\"\n"},
		{Code: "web", Port: "80", HealthPath: "/",
			ComposeTemplate: "  web:\n    image: {{UNIT_IMAGE}}\n"},
	}}
	comps := []*Component{
		{Code: "mysql", ComposeServices: "  mysql:\n    image: mysql:8.4\n",
			Endpoints:   []Endpoint{{Placeholder: "DB_HOST", Service: "mysql", Port: "3306", From: "host"}},
			Credentials: []Credential{{Placeholder: "MYSQL_ROOT_PASSWORD", Label: "MySQL root 密码", Generate: "ask"}}},
		{Code: "redis", Credentials: []Credential{
			{Placeholder: "REDIS_PASSWORD", Generate: "random"},
			{Placeholder: "REDIS_MODE", Env: "REDIS_MODE", Generate: "const", Value: "cache"},
		}},
	}
	sh := renderInstallSh("demo-kit", "1.2.0", app, comps, &app.Units[0], true, map[string]string{"BUILD_TAG": "prod"})
	for _, want := range []string{
		"# demo-kit v1.2.0 单机安装脚本",
		"wait_http 'api' 'http://127.0.0.1:8080/actuator/health' 600", // 打包期变量渲染后端口映射成立
		"prompt_var TENANT_ID '租户' 'mse' 0",
		"V_SITE_NAME='主站'", // const 变量烘焙
		"eval 'V_APP_SECRET=${V_APP_SECRET:-$(rand16)}'",
		"prompt_var MYSQL_ROOT_PASSWORD 'MySQL root 密码' '' 1", // ask 凭据询问
		"eval 'V_MYSQL_ROOT_PASSWORD=${V_MYSQL_ROOT_PASSWORD:-$(rand16)}'", // ask 留空随机
		"V_REDIS_MODE='cache'", // const 凭据烘焙
		"V_DB_HOST='mysql'",    // 端点单机取值（from=host=服务名）
		"printf '%s=\"%s\"\\n' 'MYSQL_ROOT_PASSWORD' \"$(getv MYSQL_ROOT_PASSWORD)\"", // .env 凭据行（env 缺省=占位符名）
		"printf '%s=\"%s\"\\n' 'REDIS_MODE' \"$(getv REDIS_MODE)\"",                   // .env 凭据行（显式 env 名）
		"CONF=${CONF//\\{\\{TENANT_ID\\}\\}/$(getv TENANT_ID)}",                       // 配置替换
		"CONF=${CONF//\\{\\{DB_HOST\\}\\}/$(getv DB_HOST)}",
		"docker compose up -d --remove-orphans",
		"sha256sum", // manifest 校验
		"wait_state 'web'", // web 无发布端口 → 容器状态探测降级
		"访问地址：http://%s:8080/", // 入口汇总
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("install.sh 缺关键段: %q\n---- 实际输出 ----\n%s", want, sh)
		}
	}
	// 打包期变量不应出现在脚本里（已烘进镜像/编排，与安装期无关）
	if strings.Contains(sh, "V_BUILD_TAG") || strings.Contains(sh, "prompt_var BUILD_TAG") {
		t.Error("打包期变量不应生成交互段")
	}
	// hasConf=false：无配置渲染段（无 config-template.yaml 的包）
	sh2 := renderInstallSh("demo-kit", "1.2.0", app, comps, &app.Units[0], false, map[string]string{})
	if strings.Contains(sh2, "data/conf/application.yaml") {
		t.Error("hasConf=false 不应渲染 data/conf/application.yaml 段")
	}
}
