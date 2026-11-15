package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ---- composex ----

func TestSplitServicesAndMeta(t *testing.T) {
	compose := `name: mse
services:
  app:
    image: mse/app:1.0
    ports:
      - "8080:8080"
    depends_on:
      - mysql
  web:
    image: mse/web:1.0
    ports:
      - 80:80
  mysql:
    image: mysql:8.4
    ports:
      - 3306
    environment:
      - MYSQL_ROOT_PASSWORD=${DB_PASSWORD:-x}
`
	blocks := splitServices(compose)
	if len(blocks) != 3 {
		t.Fatalf("服务数 = %d，want 3", len(blocks))
	}
	order := serviceOrder(compose)
	if strings.Join(order, ",") != "app,web,mysql" {
		t.Errorf("顺序 = %v", order)
	}
	ports := servicePorts(blocks["app"])
	if ports["8080"] != "8080" {
		t.Errorf("app 端口映射 = %v", ports)
	}
	if p := servicePorts(blocks["mysql"]); p["3306"] != "3306" {
		t.Errorf("mysql 同值发布解析失败: %v", p)
	}
	if got := serviceDeps(blocks["app"]); len(got) != 1 || got[0] != "mysql" {
		t.Errorf("app deps = %v", got)
	}
	if img := serviceImage(blocks["app"]); img != "mse/app:1.0" {
		t.Errorf("image = %q", img)
	}
	resolved := resolveEnvRefs(strings.SplitN(compose, "- MYSQL_ROOT_PASSWORD=", 2)[1], map[string]string{"DB_PASSWORD": "s3cret"})
	if !strings.Contains(resolved, "s3cret") {
		t.Errorf("env 引用未展开: %q", resolved)
	}
	if resolved2 := resolveEnvRefs("${MISSING:-def}", nil); resolved2 != "def" {
		t.Errorf("默认值未生效: %q", resolved2)
	}
}

func TestSafeImageTarName(t *testing.T) {
	if safeImageTarName("mse/app:1.0") != "mse_app_1.0.tar" {
		t.Errorf("tar 名 = %q", safeImageTarName("mse/app:1.0"))
	}
	if safeImageTarName("mysql:8.4") != "mysql_8.4.tar" {
		t.Errorf("tar 名 = %q", safeImageTarName("mysql:8.4"))
	}
}

// ---- 测试包夹具（format:3） ----

func writeTestPackage(t *testing.T, dir string) {
	t.Helper()
	compose := `name: mse
services:
  app:
    image: mse/app:1.0.0
    ports:
      - "8080:8080"
    depends_on:
      - mysql
    volumes:
      - ./data/conf:/app/conf
  web:
    image: mse/web:1.0.0
    ports:
      - 80:80
    depends_on:
      - app
  mysql:
    image: mysql:8.4
    ports:
      - "3306:3306"
    volumes:
      - ./data/mysql:/var/lib/mysql
    environment:
      - MYSQL_ROOT_PASSWORD=${DB_PASSWORD}
`
	decl := ComponentsDecl{Format: 3, Name: "mse", Version: "1.0.0",
		Entry: EntryDecl{Unit: "app", Service: "app", Image: "mse/app:1.0.0", Port: "8080", HealthPath: "/actuator/health"},
		Units: []UnitDecl{
			{Code: "app", Service: "app", Services: []string{"app"}, Image: "mse/app:1.0.0",
				Port: "8080", HealthPath: "/actuator/health", Artifact: "app.jar", ConfDir: "/app/conf"},
			{Code: "web", Service: "web", Services: []string{"web"}, Image: "mse/web:1.0.0", Port: "80"},
		},
		Variables: []VarDecl{{Name: "TENANT_ID", Label: "租户编号", Source: "input", Value: "mse"}},
		Components: []ComponentDecl{{
			Code: "mysql", Category: "database", Services: []string{"mysql"}, Images: []string{"mysql:8.4"},
			Endpoints: []Endpoint{
				{Placeholder: "DB_HOST", Service: "mysql", Port: "3306", From: "host"},
				{Placeholder: "DB_PORT", Service: "mysql", Port: "3306", From: "port"},
				{Placeholder: "DB_URL", Service: "mysql", Port: "3306", From: "url", Scheme: "http"},
			},
			Credentials: []Credential{{Placeholder: "DB_PASSWORD", Env: "DB_PASSWORD", Generate: "random"}},
			Schema:      &SchemaExec{Dir: "extras", Image: "mysql:8.4", Host: "DB_HOST", Port: "DB_PORT", User: "DB_USER", Database: "DB_NAME"},
		}},
	}
	files := map[string]string{
		composeFile:                compose,
		componentsFile:             string(mustJSON(decl)),
		templateFile:               "mse:\n  tenant: {{TENANT_ID}}\n  db: {{DB_URL}}\n  pw: {{DB_PASSWORD}}\n",
		"extras/schema.sql":        "-- init",
		"artifacts/app.jar":        "fake-jar-bytes",
		"images/mse_app_1.0.0.tar": "fake-tar-app",
		"images/mse_web_1.0.0.tar": "fake-tar-web",
		"images/mysql_8.4.tar":     "fake-tar-mysql",
		"delivery/vars.env":        "TENANT_ID=tenant-01\n",
	}
	// manifest 逐文件哈希
	manifest := map[string]string{}
	for rel, content := range files {
		sum := sha256.Sum256([]byte(content))
		manifest[rel] = hex.EncodeToString(sum[:])
	}
	mj, _ := json.Marshal(map[string]any{"files": manifest})
	files[manifestFile] = string(mj)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func testHosts() map[int64]*hostRow {
	return map[int64]*hostRow{
		1: {ID: 1, Name: "node-a", Addr: "10.0.0.1", AgentToken: "ta", Info: HostInfo{Hostname: "node-a", OS: "linux", Docker: true, IPs: []string{"10.0.0.1"}}},
		2: {ID: 2, Name: "node-b", Addr: "10.0.0.2", AgentToken: "tb", Info: HostInfo{Hostname: "node-b", OS: "linux", Docker: true, IPs: []string{"10.0.0.2"}}},
		3: {ID: 3, Name: "node-sys", Addr: "10.0.0.3", AgentToken: "tc", Info: HostInfo{Hostname: "node-sys", OS: "linux", Docker: false, Systemd: true, JavaVer: "21.0.1", IPs: []string{"10.0.0.3"}}},
	}
}

func parseTestPkg(t *testing.T, dir string) *ParsedPackage {
	t.Helper()
	pp, err := parsePackageDir(dir)
	if err != nil {
		t.Fatal("解析测试包失败:", err)
	}
	// 文件清单回填（模拟生产路径：上传时入库、编排时自 DB 行回填——parsePackageDir 已不再现场扫盘）
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == manifestFile {
			return nil
		}
		sum, _ := fileSHA256(path)
		info, _ := d.Info()
		pp.Row.Files = append(pp.Row.Files, pkgFile{Path: rel, Size: info.Size(), SHA256: sum})
		return nil
	})
	// nil=无预计算哈希，走逐文件读盘兜底路径（与上传主路径互补覆盖）
	if bad := pp.verifyManifest(nil); len(bad) > 0 {
		t.Fatal("manifest 校验失败:", bad)
	}
	return pp
}

func allOnHost(hostID int64, withSys bool) *Placement {
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{
		"app": hostID, "web": hostID, "mysql": hostID,
	}, Vars: map[string]string{"TENANT_ID": "tenant-01"}, Ext: map[string]string{}}
	if withSys {
		pl.Mode["app"] = "systemd"
		delete(pl.Host, "mysql")
	}
	return pl
}

// ---- 引擎：默认 DAG / 端点三态 / 物化 ----

func TestBuildFreshSameHost(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, allOnHost(1, false), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "fresh" {
		t.Errorf("mode = %s", res.Mode)
	}
	kinds := map[string]string{}
	for _, n := range res.Nodes {
		kinds[n.Key] = n.Kind
	}
	for _, key := range []string{"load-h1", "compose-h1", "wait-entry"} {
		if _, ok := kinds[key]; !ok {
			t.Errorf("缺少节点 %s（实际 %v）", key, kinds)
		}
	}
	// mysql 落机：不应有 schema 节点（initdb 挂载覆盖）
	for key, kind := range kinds {
		if kind == "cmd" {
			t.Errorf("mysql 已落机却出现 schema 节点 %s", key)
		}
	}
	// 端点三态：同机 → 服务名
	var composePayload ComposePayload
	for _, n := range res.Nodes {
		if n.Key == "compose-h1" {
			_ = json.Unmarshal(n.Payload, &composePayload)
		}
	}
	if !strings.Contains(composePayload.Compose, "name: mse-h1") {
		t.Errorf("per-host compose 项目名不符:\n%s", composePayload.Compose)
	}
	if !strings.Contains(composePayload.Files["data/conf/application.yaml"], "db: http://mysql:3306") {
		t.Errorf("同机端点应为服务名: %v", composePayload.Files)
	}
	if !strings.Contains(composePayload.Files["data/conf/application.yaml"], "tenant: tenant-01") {
		t.Errorf("变量渲染缺失（delivery 预填应生效）: %v", composePayload.Files)
	}
	if !strings.Contains(composePayload.Env, "DB_PASSWORD=") {
		t.Errorf(".env 缺少随机凭据: %q", composePayload.Env)
	}
	// 入口 wait：依赖 compose 节点；类型 http
	for _, n := range res.Nodes {
		if n.Key == "wait-entry" {
			var wp WaitPayload
			_ = json.Unmarshal(n.Payload, &wp)
			if wp.Type != "http" || wp.URL != "http://127.0.0.1:8080/actuator/health" {
				t.Errorf("入口 wait = %+v", wp)
			}
			deps := map[string]bool{}
			for _, d := range n.Deps {
				deps[d] = true
			}
			if !deps["compose-h1"] || !deps["compose-h1-base"] {
				t.Errorf("入口 wait deps 应含 base 与 rest 段: %v", n.Deps)
			}
		}
	}
	if res.EntryURL != "http://10.0.0.1:8080" {
		t.Errorf("entryUrl = %s", res.EntryURL)
	}
	// inventory 覆盖全部服务
	if len(res.InvRows) != 3 {
		t.Errorf("invRows = %d（app/web/mysql）", len(res.InvRows))
	}
}

func TestEndpointCrossHost(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	pl := &Placement{Host: map[string]int64{"app": 1, "web": 1, "mysql": 2},
		Vars: map[string]string{"TENANT_ID": "t"}, Mode: map[string]string{}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 主机 1 渲染的配置：DB_URL 跨机 → 10.0.0.2:3306；跨机依赖 wait 门控 compose-h1
	for _, n := range res.Nodes {
		if n.Kind != "compose" {
			continue
		}
		var cp ComposePayload
		_ = json.Unmarshal(n.Payload, &cp)
		if cp.Project == "mse-h1" {
			if got := cp.Files["data/conf/application.yaml"]; !strings.Contains(got, "db: http://10.0.0.2:3306") {
				t.Errorf("跨机端点应为主机地址: %q", got)
			}
		}
	}
	found := false
	for _, n := range res.Nodes {
		// h1 分层后 app（依赖 mysql 的消费方）在 base 段——wait-mysql 门控挂 compose-h1-base
		if n.Key == "compose-h1-base" {
			for _, d := range n.Deps {
				if d == "wait-mysql" {
					found = true
				}
			}
			var cp ComposePayload
			_ = json.Unmarshal(n.Payload, &cp)
			// 悬空剥离：h1 无 mysql，app 块的 depends_on: mysql 必须被剥掉（否则 up 报 undefined service）
			if strings.Contains(cp.Compose, "depends_on") && strings.Contains(cp.Compose, "mysql") &&
				!strings.Contains(cp.Compose, "image: mysql") {
				t.Errorf("h1 compose 残留悬空依赖:\n%s", cp.Compose)
			}
			if strings.Contains(cp.Compose, "- mysql") || strings.Contains(cp.Compose, "[mysql]") {
				t.Errorf("h1 compose 未剥离跨机依赖 mysql:\n%s", cp.Compose)
			}
		}
	}
	if !found {
		t.Error("跨机依赖未生成 wait-mysql 门控（应挂 compose-h1-base）")
	}
}

// 同机就绪分层：app/web/mysql 同机（web→app→mysql）——base={mysql,app} 先行 up + 健康 wait
// （mysql 无 HTTP 健康路径 → TCP 端口探测；app 有 HealthPath → HTTP），rest={web} 等 base 健康后 up
func TestSameHostReadySplit(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, allOnHost(1, false), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]planNode{}
	for _, n := range res.Nodes {
		byKey[n.Key] = n
	}
	base, ok := byKey["compose-h1-base"]
	if !ok {
		t.Fatal("缺少 base 段节点 compose-h1-base")
	}
	rest, ok := byKey["compose-h1"]
	if !ok {
		t.Fatal("缺少 rest 段节点 compose-h1")
	}
	var bp, rp ComposePayload
	_ = json.Unmarshal(base.Payload, &bp)
	_ = json.Unmarshal(rest.Payload, &rp)
	sort.Strings(bp.Services)
	if fmt.Sprint(bp.Services) != fmt.Sprint([]string{"app", "mysql"}) {
		t.Errorf("base 段服务 = %v（应 app+mysql）", bp.Services)
	}
	if fmt.Sprint(rp.Services) != fmt.Sprint([]string{"web"}) {
		t.Errorf("rest 段服务 = %v（应 web）", rp.Services)
	}
	// base 依赖 load；rest 依赖 load + base 全部健康 wait
	if !contains(base.Deps, "load-h1") {
		t.Errorf("base deps = %v", base.Deps)
	}
	if !contains(rest.Deps, "wait-mysql") || !contains(rest.Deps, "wait-app") {
		t.Errorf("rest deps 应含 wait-mysql/wait-app: %v", rest.Deps)
	}
	// wait 挂在 base 段后；mysql=TCP 探测 3306、app=HTTP 健康路径
	wm, ok := byKey["wait-mysql"]
	if !ok || !contains(wm.Deps, "compose-h1-base") {
		t.Fatalf("wait-mysql 缺失或未挂 base 段: %+v", wm)
	}
	var wmp WaitPayload
	_ = json.Unmarshal(wm.Payload, &wmp)
	if wmp.Type != "tcp" || wmp.Port != "3306" || wmp.Host != "127.0.0.1" {
		t.Errorf("wait-mysql 应为 TCP 3306 探测: %+v", wmp)
	}
	wa, ok := byKey["wait-app"]
	if !ok {
		t.Fatal("缺少 wait-app")
	}
	var wap WaitPayload
	_ = json.Unmarshal(wa.Payload, &wap)
	if wap.Type != "http" || wap.URL != "http://127.0.0.1:8080/actuator/health" {
		t.Errorf("wait-app 应为 HTTP 探测: %+v", wap)
	}
	// 同机依赖不剥离：compose 文本保留 web→app→mysql 的 depends_on（启动序兜底）
	if !strings.Contains(bp.Compose, "- mysql") || !strings.Contains(bp.Compose, "- app") {
		t.Errorf("同机 depends_on 应保留:\n%s", bp.Compose)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// 组件勾选制（collect-kit 侧 mysql/redis 不再必备）：包内无 mysql 组件时，DB 端点/凭据占位符
// 由应用级安装变量供值——配置渲染成功且取安装表单值（组件缺席=无端点覆盖，变量兜底）
func TestConfigRendersFromAppVarsWithoutComponent(t *testing.T) {
	dir := t.TempDir()
	compose := `name: mse
services:
  app:
    image: mse/app:1.0.0
    ports:
      - "8080:8080"
    volumes:
      - ./data/conf:/app/conf
  web:
    image: mse/web:1.0.0
    ports:
      - 80:80
    depends_on:
      - app
`
	decl := ComponentsDecl{Format: 3, Name: "mse", Version: "1.0.0",
		Entry: EntryDecl{Unit: "app", Service: "app", Image: "mse/app:1.0.0", Port: "8080", HealthPath: "/actuator/health"},
		Units: []UnitDecl{
			{Code: "app", Service: "app", Services: []string{"app"}, Image: "mse/app:1.0.0",
				Port: "8080", HealthPath: "/actuator/health", Artifact: "app.jar", ConfDir: "/app/conf"},
			{Code: "web", Service: "web", Services: []string{"web"}, Image: "mse/web:1.0.0", Port: "80"},
		},
		Variables: []VarDecl{
			{Name: "TENANT_ID", Label: "租户编号", Source: "input", Value: "mse"},
			{Name: "DB_HOST", Label: "MySQL 地址（未勾 mysql 组件时填外部库）", Source: "input", Value: ""},
			{Name: "DB_PASSWORD", Label: "应用库密码", Source: "input", Value: ""},
		},
	}
	files := map[string]string{
		composeFile:                compose,
		componentsFile:             string(mustJSON(decl)),
		templateFile:               "mse:\n  tenant: {{TENANT_ID}}\n  db_host: {{DB_HOST}}\n  pw: {{DB_PASSWORD}}\n",
		"artifacts/app.jar":        "fake-jar-bytes",
		"images/mse_app_1.0.0.tar": "fake-tar-app",
		"images/mse_web_1.0.0.tar": "fake-tar-web",
		"delivery/vars.env":        "TENANT_ID=tenant-01\n",
	}
	manifest := map[string]string{}
	for rel, content := range files {
		sum := sha256.Sum256([]byte(content))
		manifest[rel] = hex.EncodeToString(sum[:])
	}
	mj, _ := json.Marshal(map[string]any{"files": manifest})
	files[manifestFile] = string(mj)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	pp := parseTestPkg(t, dir)
	pl := &Placement{Host: map[string]int64{"app": 1, "web": 1}, Mode: map[string]string{},
		Vars: map[string]string{"TENANT_ID": "t", "DB_HOST": "10.1.2.3", "DB_PASSWORD": "ext-pw"}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatalf("无组件 + 应用变量应渲染成功（占位符由安装变量覆盖）: %v", err)
	}
	for _, n := range res.Nodes {
		if n.Kind != "compose" {
			continue
		}
		var cp ComposePayload
		_ = json.Unmarshal(n.Payload, &cp)
		if got := cp.Files["data/conf/application.yaml"]; got != "" {
			if !strings.Contains(got, "db_host: 10.1.2.3") || !strings.Contains(got, "pw: ext-pw") {
				t.Errorf("配置应取应用安装变量值:\n%s", got)
			}
		}
	}
}

func TestEndpointExternalRequiresExt(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Host: map[string]int64{"app": 1, "web": 1}, // mysql 未落机=外部
		Vars: map[string]string{"TENANT_ID": "t"}, Mode: map[string]string{}}
	if _, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil); err == nil {
		t.Fatal("外部组件未手填端点应报错")
	}
	pl.Ext = map[string]string{"DB_HOST": "192.168.1.100", "DB_PORT": "3307", "DB_URL": "http://192.168.1.100:3307"}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 外部 mysql → schema 节点应生成（cmd，一次性容器）
	hasSchema := false
	for _, n := range res.Nodes {
		if strings.HasPrefix(n.Key, "schema-mysql") && n.Kind == "cmd" {
			hasSchema = true
			var cp CmdPayload
			_ = json.Unmarshal(n.Payload, &cp)
			if !strings.Contains(cp.Script, "mysql -h192.168.1.100 -P3307") {
				t.Errorf("schema 连接参数异常: %s", cp.Script)
			}
		}
	}
	if !hasSchema {
		t.Error("外部组件缺 schema 节点")
	}
}

func TestSystemdPlacement(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	pl := &Placement{Host: map[string]int64{"app": 3, "web": 1, "mysql": 2},
		Mode: map[string]string{"app": "systemd"},
		Vars: map[string]string{"TENANT_ID": "t"},
		Ext:  map[string]string{"DB_HOST": "10.0.0.2", "DB_PORT": "3306", "DB_URL": "http://10.0.0.2:3306"}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sysp SystemdPayload
	found := false
	for _, n := range res.Nodes {
		if n.Kind == "systemd" {
			found = true
			_ = json.Unmarshal(n.Payload, &sysp)
		}
	}
	if !found {
		t.Fatal("缺少 systemd 节点")
	}
	if sysp.Unit != "mse-app.service" || !strings.Contains(sysp.UnitFile, "ExecStart=java -jar {{APP_DIR}}/artifacts/app.jar") {
		t.Errorf("unit 物化异常: %s", sysp.UnitFile)
	}
	if !strings.Contains(sysp.UnitFile, "Environment=APP_PORT=8080") {
		t.Errorf("unit 缺 APP_PORT: %s", sysp.UnitFile)
	}
	// systemd 主机 load 应含制品 jar 而非 app 镜像 tar
	for _, n := range res.Nodes {
		if n.Kind == "load" && n.Host == 3 {
			var lp LoadPayload
			_ = json.Unmarshal(n.Payload, &lp)
			if len(lp.Files) != 1 || lp.Files[0].Path != "artifacts/app.jar" {
				t.Errorf("systemd 主机 load = %+v", lp.Files)
			}
		}
	}
	// docker 主机承载 systemd 单元应被拒绝
	bad := &Placement{Host: map[string]int64{"app": 1, "web": 1, "mysql": 1},
		Mode: map[string]string{"app": "systemd"}, Vars: map[string]string{"TENANT_ID": "t"}}
	if _, err := BuildSessionPlan(pp, bad, hosts, nil, nil, "fresh", nil); err == nil {
		t.Error("docker 主机承载 systemd 应报错")
	}
}

// ---- 增量 diff ----

func TestIncrementalDiff(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	pl := allOnHost(1, false)
	fresh, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 库存=基线一致 → 增量计划为空（自动回落说明在 BuildSessionPlan 内处理 inventory 空，
	// 这里给满量库存）
	pl2 := allOnHost(1, false)
	pl2.Vars["DB_PASSWORD"] = fresh.Vars["DB_PASSWORD"] // 上次会话凭据预填（UI 行为）
	same, err := BuildSessionPlan(pp, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Nodes) != 0 {
		t.Errorf("未变化时增量节点应为 0，实际 %d（%v）", len(same.Nodes), keys(same.Nodes))
	}
	if same.Mode != "incremental" {
		t.Errorf("mode = %s", same.Mode)
	}
	// 库存过期（块哈希不符）→ 对应主机 compose/load 回归计划
	stale := []invRow{}
	for _, r := range fresh.InvRows {
		r.Detail = strings.Replace(r.Detail, `"blockHash":"`, `"blockHash":"x`, 1)
		stale = append(stale, r)
	}
	changed, err := BuildSessionPlan(pp, pl, hosts, stale, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, n := range changed.Nodes {
		kinds[n.Key] = true
	}
	if !kinds["compose-h1"] || !kinds["load-h1"] {
		t.Errorf("变更后应保留 load/compose 节点: %v", kinds)
	}
}

func keys(nodes []planNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Key)
	}
	return out
}

// ---- DAG 校验与自定义覆盖 ----

func TestValidateDagCycle(t *testing.T) {
	nodes := []planNode{
		{Key: "a", Deps: []string{"b"}},
		{Key: "b", Deps: []string{"a"}},
	}
	if err := validateDag(nodes); err == nil {
		t.Error("环未检出")
	}
	nodes = []planNode{{Key: "a", Deps: []string{"missing"}}}
	if err := validateDag(nodes); err == nil {
		t.Error("悬空依赖未检出")
	}
}

func TestOverlayDagCustomCmdGate(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	custom := []DagNode{
		{ID: "load-h1", Kind: "load"},
		{ID: "compose-h1", Kind: "compose", Deps: []string{"load-h1", "gate-1"}},
		{ID: "gate-1", Kind: "gate", Title: "确认 DB 就绪"},
		{ID: "cmd-1", Kind: "cmd", Host: 1, Script: "echo hi", Deps: []string{"compose-h1"}},
		{ID: "wait-entry", Kind: "wait", Deps: []string{"compose-h1"}},
	}
	res, err := BuildSessionPlan(pp, allOnHost(1, false), testHosts(), nil, custom, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, n := range res.Nodes {
		seen[n.Key] = n.Kind
	}
	if seen["gate-1"] != "gate" || seen["cmd-1"] != "cmd" {
		t.Errorf("自定义节点缺失: %v", seen)
	}
	if _, ok := seen["wait-web"]; ok {
		t.Errorf("自定义 DAG 应替换自动 DAG（wait-web 不应存在）: %v", seen)
	}
}

// ---- zip 上传校验 ----

func TestSavePackageZip(t *testing.T) {
	src := t.TempDir()
	writeTestPackage(t, src)
	zipPath := filepath.Join(t.TempDir(), "mse-1.0.0.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	_ = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		entry, _ := zw.Create(filepath.ToSlash(rel))
		data, _ := os.ReadFile(path)
		_, _ = entry.Write(data)
		return nil
	})
	zw.Close()
	zf.Close()

	root := t.TempDir()
	pp, err := savePackageZip(root, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if pp.Row.App != "mse" || pp.Decl.Entry.Unit != "app" {
		t.Errorf("解析异常: %+v", pp.Row)
	}
	if pp.Prefill["TENANT_ID"] != "tenant-01" {
		t.Errorf("delivery 预填未提取: %v", pp.Prefill)
	}
	// 损坏一个文件再上传 → manifest 强校验拒绝
	_ = os.WriteFile(filepath.Join(pp.PkgDir, componentsFile), []byte("tampered"), 0644)
	zf2, _ := os.Create(zipPath)
	zw2 := zip.NewWriter(zf2)
	_ = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		entry, _ := zw2.Create(filepath.ToSlash(rel))
		data, _ := os.ReadFile(path)
		if rel == componentsFile {
			data = []byte("tampered")
		}
		_, _ = entry.Write(data)
		return nil
	})
	zw2.Close()
	zf2.Close()
	if _, err := savePackageZip(root, zipPath); err == nil {
		t.Error("篡改包应被 manifest 校验拒绝")
	}
}

// ---- 存储层 ----

func TestStoreBasics(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &hostRow{Name: "node-a", AgentToken: "tok-a", Info: HostInfo{Hostname: "node-a", Docker: true}}
	if err := st.UpsertHost(h); err != nil {
		t.Fatal(err)
	}
	if h.ID == 0 {
		t.Fatal("未分配 ID")
	}
	got, err := st.HostByToken("tok-a")
	if err != nil || got.Name != "node-a" {
		t.Fatalf("HostByToken = %v, %v", got, err)
	}
	if err := st.TouchHost(h.ID, nil); err != nil {
		t.Fatal(err)
	}
	_ = st.PutSetting("k", "v")
	if st.GetSetting("k", "") != "v" {
		t.Error("settings 读写异常")
	}
	// 会话与日志
	nodes := []planNode{{Key: "n1", Kind: "load", Title: "t", Host: h.ID}}
	ss := &sessionRow{App: "mse", Version: "1", PackageID: 1, Mode: "fresh", Plan: "{}"}
	if err := st.InsertSession(ss, nodes); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLogs(ss.ID, "n1", []string{"l1", "l2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLogs(ss.ID, "n1", []string{"l3"}); err != nil {
		t.Fatal(err)
	}
	lines, err := st.LogsAfter(ss.ID, 0, 10, "")
	if err != nil || len(lines) != 3 || lines[2].Line != "l3" {
		t.Fatalf("日志顺序/条数异常: %+v %v", lines, err)
	}
	if err := st.UpsertInventory([]invRow{{App: "mse", HostID: h.ID, Service: "app", Kind: "compose", Detail: "{}"}}); err != nil {
		t.Fatal(err)
	}
	inv, _ := st.InventoryByApp("mse")
	if len(inv) != 1 || inv[0].Service != "app" {
		t.Fatalf("inventory 异常: %+v", inv)
	}
}

// ---- 配置中心插件链（configCenter 声明组件：plan → gate → apply → health）----

func writeTestPkgCC(t *testing.T, dir string) {
	t.Helper()
	compose := `name: mse-cc
services:
  app:
    image: mse/app:1.0.0
    ports:
      - "18099:18099"
    depends_on:
      - nacos
  nacos:
    image: nacos/nacos-server:v3.0.0
    ports:
      - "8848:8848"
`
	decl := ComponentsDecl{Format: 3, Name: "mse-cc", Version: "1.0.0",
		Entry: EntryDecl{Unit: "app", Service: "app", Port: "18099"},
		Units: []UnitDecl{{Code: "app", Service: "app", Services: []string{"app"}, Image: "mse/app:1.0.0", Port: "18099"}},
		Components: []ComponentDecl{{
			Code: "nacos", Category: "middleware", Services: []string{"nacos"}, Images: []string{"nacos/nacos-server:v3.0.0"},
			Endpoints:   []Endpoint{{Placeholder: "NACOS_ADDR", Service: "nacos", Port: "8848", From: "url", Scheme: "http"}},
			Credentials: []Credential{{Placeholder: "NACOS_IDENTITY_VALUE", Env: "NACOS_IDENTITY_VALUE", Generate: "const", Value: "security"}},
			ConfigCenter: &ConfigCenterDecl{Kind: "nacos", Plugin: "nacos-config", IdentityValue: "NACOS_IDENTITY_VALUE",
				Configs: []ConfigItemDecl{{DataID: "app.yaml", File: "nacos/configs/app.yaml"}}},
		}},
	}
	files := map[string]string{
		composeFile:                            compose,
		componentsFile:                         string(mustJSON(decl)),
		"nacos/configs/app.yaml":               "server:\n  nacos: {{NACOS_ADDR}}\n  identity: {{NACOS_IDENTITY_VALUE}}\n",
		"images/mse_app_1.0.0.tar":             "fake-tar-app",
		"images/nacos_nacos-server_v3.0.0.tar": "fake-tar-nacos",
	}
	manifest := map[string]string{}
	for rel, content := range files {
		sum := sha256.Sum256([]byte(content))
		manifest[rel] = hex.EncodeToString(sum[:])
	}
	mj, _ := json.Marshal(map[string]any{"files": manifest})
	files[manifestFile] = string(mj)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func ccNode(t *testing.T, res *buildResult, key string) planNode {
	t.Helper()
	for _, n := range res.Nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("缺少节点 %s（实际 keys 见断言输出）", key)
	return planNode{}
}

func TestConfigCenterChainCrossHost(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "nacos": 2},
		Vars: map[string]string{}, Ext: map[string]string{}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := ccNode(t, res, "cc-plan-nacos")
	gate := ccNode(t, res, "gate-cc-nacos")
	apply := ccNode(t, res, "cc-apply-nacos")
	health := ccNode(t, res, "cc-health-nacos")
	if plan.Kind != "plugin" || gate.Kind != "gate" || apply.Kind != "plugin" || health.Kind != "plugin" {
		t.Fatalf("链节点类型异常: %s/%s/%s/%s", plan.Kind, gate.Kind, apply.Kind, health.Kind)
	}
	// nacos 跨机依赖：wait-nacos 生成，cc-plan 后置于其上；app 编排节点后置于 cc-apply
	if !containsDep(plan.Deps, "wait-nacos") {
		t.Errorf("cc-plan 应依赖 wait-nacos，实际 %v", plan.Deps)
	}
	comp := ccNode(t, res, "compose-h1")
	if !containsDep(comp.Deps, "cc-apply-nacos") || !containsDep(comp.Deps, "wait-nacos") {
		t.Errorf("compose-h1 应依赖 cc-apply-nacos 与 wait-nacos，实际 %v", comp.Deps)
	}
	// apply 载荷：op/mode/基线内容/连接参数（跨机视角 + 身份凭据解析）
	var p PluginPayload
	if err := json.Unmarshal(apply.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Op != "apply" || p.Mode != "idempotent" || p.Plugin != "nacos-config" || p.OS != "linux" {
		t.Fatalf("apply 载荷异常: %+v", p)
	}
	// 链落组件主机（host2）：同机宿主视角 = 127.0.0.1:发布端口（插件进程在宿主 OS）
	if p.Config["serverAddr"] != "http://127.0.0.1:8848" || p.Config["identityValue"] != "security" || p.Config["namespace"] != "public" {
		t.Errorf("apply 连接参数异常: %v", p.Config)
	}
	// 基线渲染：NACOS_ADDR 全局形态（跨机=主机地址:发布端口，非服务名）+ 组件凭据参与渲染
	wantCC := "server:\n  nacos: http://10.0.0.2:8848\n  identity: security\n"
	if len(p.Items) != 1 || p.Items[0].DataID != "app.yaml" || p.Items[0].Content != wantCC {
		t.Errorf("基线条目异常: %+v", p.Items)
	}
	// plan/health 载荷
	var hp PluginPayload
	_ = json.Unmarshal(health.Payload, &hp)
	if hp.Op != "health" || len(hp.Items) != 0 {
		t.Errorf("health 载荷异常: op=%s items=%d", hp.Op, len(hp.Items))
	}
}

func containsDep(deps []string, key string) bool {
	for _, d := range deps {
		if d == key {
			return true
		}
	}
	return false
}

func TestConfigCenterExternal(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	// nacos 未落机=外部复用：端点手填；插件链落首个单元主机
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1},
		Vars: map[string]string{}, Ext: map[string]string{"NACOS_ADDR": "http://ext-nacos:8848"}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := ccNode(t, res, "cc-plan-nacos")
	if plan.Host != 1 || len(plan.Deps) != 0 {
		t.Errorf("外部配置中心：链应落单元主机 1 且无前置依赖，实际 host=%d deps=%v", plan.Host, plan.Deps)
	}
	var p PluginPayload
	_ = json.Unmarshal(ccNode(t, res, "cc-apply-nacos").Payload, &p)
	if p.Config["serverAddr"] != "http://ext-nacos:8848" {
		t.Errorf("外部端点应取手填值，实际 %v", p.Config)
	}
}

// 增量会话：配置中心插件链恒保留（基线差异不在 inventory 哈希内，靠 plan 重新比对发现）
func TestConfigCenterIncrementalKeepsChain(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "nacos": 2},
		Vars: map[string]string{}, Ext: map[string]string{}}
	fresh, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := BuildSessionPlan(pp, pl, testHosts(), fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Mode != "incremental" {
		t.Fatalf("mode = %s", inc.Mode)
	}
	seen := map[string]bool{}
	for _, n := range inc.Nodes {
		seen[n.Key] = true
	}
	for _, key := range []string{"cc-plan-nacos", "gate-cc-nacos", "cc-apply-nacos", "cc-health-nacos"} {
		if !seen[key] {
			t.Errorf("增量计划应保留 %s（实际 %v）", key, seen)
		}
	}
}

// 自定义 DAG 把 apply 节点改为 force 模式（覆盖线上差异）
func TestConfigCenterOverlayForce(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "nacos": 2},
		Vars: map[string]string{}, Ext: map[string]string{}}
	dag := []DagNode{
		{ID: "cc-plan-nacos", Kind: "plugin"},
		{ID: "gate-cc-nacos", Kind: "gate", Deps: []string{"cc-plan-nacos"}},
		{ID: "cc-apply-nacos", Kind: "plugin", Mode: "force", Deps: []string{"gate-cc-nacos"}},
	}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, dag, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var p PluginPayload
	if err := json.Unmarshal(ccNode(t, res, "cc-apply-nacos").Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Mode != "force" {
		t.Fatalf("自定义 DAG 应把 apply 改为 force，实际 %q", p.Mode)
	}
	// 非法模式拦截
	dag[2].Mode = "bogus"
	if _, err := BuildSessionPlan(pp, pl, testHosts(), nil, dag, "fresh", nil); err == nil {
		t.Fatal("非法 mode 应拦截")
	}
}

// ---- 自动安装 Docker（placement.dockerInstall：无 docker 的 Linux 主机不拒收，头部注入安装节点）----

func planNodeByKey(t *testing.T, res *buildResult, key string) planNode {
	t.Helper()
	for _, n := range res.Nodes {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("缺少节点 %s（实际 %v）", key, res.Nodes)
	return planNode{}
}

func depHas(deps []string, key string) bool {
	for _, d := range deps {
		if d == key {
			return true
		}
	}
	return false
}

func TestDockerAutoInstallPlan(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := allOnHost(3, false) // 主机 3 无 docker（linux）
	pl.DockerInstall = true
	opts := &DockerOpts{ScriptURL: "https://example.com/get-docker.sh", Mirrors: []string{"https://mirror.example.com"}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", opts)
	if err != nil {
		t.Fatal(err)
	}
	dn := planNodeByKey(t, res, "docker-install-h3")
	if dn.Kind != "docker" || dn.Host != 3 || dn.TimeoutSec != 1800 {
		t.Fatalf("安装节点 = %+v", dn)
	}
	var dp DockerPayload
	if err := json.Unmarshal(dn.Payload, &dp); err != nil {
		t.Fatal(err)
	}
	if dp.ScriptURL != opts.ScriptURL || len(dp.Mirrors) != 1 || dp.MediaName != "" {
		t.Fatalf("安装载荷 = %+v", dp)
	}
	// 装载/编排节点挂安装依赖；warning 提示自动安装
	if !depHas(planNodeByKey(t, res, "load-h3").Deps, "docker-install-h3") {
		t.Error("load-h3 应依赖 docker-install-h3")
	}
	if !depHas(planNodeByKey(t, res, "compose-h3").Deps, "load-h3") {
		t.Error("compose-h3 应依赖 load-h3（经传递等 docker 就绪）")
	}
	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "自动安装 Docker") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("缺自动安装 warning（实际 %v）", res.Warnings)
	}
}

// 离线介质优先：匹配 os-arch 的介质进载荷（最新真实版本随行；ScriptURL 仍随行作为兜底说明）
func TestDockerInstallMediaPreferred(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	hosts[3].Info.Arch = "amd64"
	pl := allOnHost(3, false)
	pl.DockerInstall = true
	opts := &DockerOpts{ScriptURL: "https://example.com/get.sh",
		Media: mediaLib{"linux-amd64": {"27.1": DockerMediaMeta{Name: "docker-27.1.tgz", SHA256: "abc123", Size: 456},
			"26.3": DockerMediaMeta{Name: "docker-26.3.tgz", SHA256: "old", Size: 100}}}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", opts)
	if err != nil {
		t.Fatal(err)
	}
	var dp DockerPayload
	if err := json.Unmarshal(planNodeByKey(t, res, "docker-install-h3").Payload, &dp); err != nil {
		t.Fatal(err)
	}
	if dp.MediaName != "docker-27.1.tgz" || dp.MediaSHA256 != "abc123" || dp.MediaSize != 456 {
		t.Fatalf("介质未优先 = %+v", dp)
	}
	if dp.Version != "27.1" {
		t.Fatalf("载荷应带最新介质版本 27.1 = %+v", dp)
	}
}

// ---- docker compose 插件自动补装（toolkit 任务注入矩阵） ----

// 已有 docker 缺 compose（DockerInstall 开关开 + 有安装源）→ 注入 toolkit 节点，compose 节点后置
func TestComposeAutoInstallPlan(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := allOnHost(1, false) // 主机 1 已有 docker、能力档案无 compose
	pl.DockerInstall = true
	hosts := testHosts()
	hosts[1].Info.Arch = "amd64"
	opts := &DockerOpts{ScriptURL: "https://example.com/get.sh",
		Compose: &ComposeOpts{Version: "v2.30.3",
			Media: mediaLib{"linux-amd64": {"v2.30.3": DockerMediaMeta{Name: "docker-compose", SHA256: "abc", Size: 64}}}}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", opts)
	if err != nil {
		t.Fatal(err)
	}
	cn := planNodeByKey(t, res, "compose-install-h1")
	if cn.Kind != "toolkit" || cn.Host != 1 {
		t.Fatalf("compose 安装节点 = %+v", cn)
	}
	var tp ToolkitPayload
	if err := json.Unmarshal(cn.Payload, &tp); err != nil {
		t.Fatal(err)
	}
	if tp.Tool != "compose" || tp.MediaVer != "v2.30.3" || tp.MediaSHA256 != "abc" || tp.OnlineURL != "" {
		t.Fatalf("compose 载荷 = %+v", tp)
	}
	if !depHas(planNodeByKey(t, res, "compose-h1").Deps, "compose-install-h1") {
		t.Error("compose-h1 应依赖 compose-install-h1")
	}
	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "docker compose") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("缺 compose 补装 warning（实际 %v）", res.Warnings)
	}
}

// 无 docker 主机：docker-install 与 compose-install 同时注入，后者依赖前者（先引擎后插件）
func TestComposeAfterDockerInstall(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := allOnHost(3, false) // 主机 3 无 docker
	pl.DockerInstall = true
	hosts := testHosts()
	hosts[3].Info.Arch = "amd64"
	opts := &DockerOpts{ScriptURL: "https://example.com/get.sh",
		Compose: &ComposeOpts{Version: "v2.30.3"}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", opts)
	if err != nil {
		t.Fatal(err)
	}
	dn := planNodeByKey(t, res, "docker-install-h3")
	cn := planNodeByKey(t, res, "compose-install-h3")
	if dn.Kind != "docker" || cn.Kind != "toolkit" {
		t.Fatalf("节点 = %+v / %+v", dn, cn)
	}
	if !depHas(cn.Deps, dn.Key) {
		t.Errorf("compose-install 应依赖 docker-install: %+v", cn.Deps)
	}
	if !depHas(planNodeByKey(t, res, "compose-h3").Deps, cn.Key) {
		t.Error("compose-h3 应依赖 compose-install-h3")
	}
	var tp ToolkitPayload
	if err := json.Unmarshal(cn.Payload, &tp); err != nil {
		t.Fatal(err)
	}
	if tp.OnlineURL == "" || !strings.Contains(tp.OnlineURL, "v2.30.3") {
		t.Fatalf("无介质应走在线渲染 = %+v", tp)
	}
}

// 已装 compose / 开关关 / 无安装源 → 均不注入
func TestComposeInstallNotInjected(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	src := &DockerOpts{Compose: &ComposeOpts{Version: "v2.30.3"}}
	// a) compose 已就绪
	hosts := testHosts()
	hosts[1].Info.Compose, hosts[1].Info.ComposeVer = true, "2.30.3"
	pl := allOnHost(1, false)
	pl.DockerInstall = true
	if res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", src); err != nil {
		t.Fatal(err)
	} else if nodeByKey(res, composeInstallKey(1)) != nil {
		t.Error("compose 已装不应注入")
	}
	// b) 开关关（主机有 docker 校验通过）
	pl2 := allOnHost(1, false)
	if res, err := BuildSessionPlan(pp, pl2, testHosts(), nil, nil, "fresh", src); err != nil {
		t.Fatal(err)
	} else if nodeByKey(res, composeInstallKey(1)) != nil {
		t.Error("开关关不应注入")
	}
	// c) 无安装源（opts 无 Compose）
	pl3 := allOnHost(1, false)
	pl3.DockerInstall = true
	if res, err := BuildSessionPlan(pp, pl3, testHosts(), nil, nil, "fresh", &DockerOpts{ScriptURL: "https://x"}); err != nil {
		t.Fatal(err)
	} else if nodeByKey(res, composeInstallKey(1)) != nil {
		t.Error("无安装源不应注入")
	}
}

func nodeByKey(res *buildResult, key string) *planNode {
	for i := range res.Nodes {
		if res.Nodes[i].Key == key {
			return &res.Nodes[i]
		}
	}
	return nil
}

// buildToolkitPayload 版本解析：缺省=最新介质；指定版本精确取；指定版本无介质→在线渲染；无源全空
func TestBuildToolkitPayload(t *testing.T) {
	h := &hostRow{ID: 9, Name: "x", Info: HostInfo{OS: "linux", Arch: "amd64"}}
	opts := &DockerOpts{Compose: &ComposeOpts{Template: defaultComposeTemplate, Version: "v2.30.3",
		Media: mediaLib{"linux-amd64": {
			"v2.30.1": DockerMediaMeta{Name: "a", SHA256: "s1", Size: 1},
			"v2.30.3": DockerMediaMeta{Name: "b", SHA256: "s2", Size: 2}}}}}
	if p := buildToolkitPayload(h, opts, "", false); p.MediaVer != "v2.30.3" || p.MediaSHA256 != "s2" || p.OnlineURL != "" {
		t.Fatalf("缺省应取最新介质 = %+v", p)
	}
	if p := buildToolkitPayload(h, opts, "v2.30.1", true); p.MediaVer != "v2.30.1" || p.MediaSHA256 != "s1" || !p.Force {
		t.Fatalf("指定版本应精确取介质 = %+v", p)
	}
	if p := buildToolkitPayload(h, opts, "v2.31.0", false); p.MediaSHA256 != "" ||
		p.OnlineURL != "https://github.com/docker/compose/releases/download/v2.31.0/docker-compose-linux-x86_64" {
		t.Fatalf("指定版本无介质应在线渲染 = %+v", p)
	}
	if p := buildToolkitPayload(h, &DockerOpts{}, "", false); p.MediaSHA256 != "" || p.OnlineURL != "" {
		t.Fatalf("无源应全空 = %+v", p)
	}
}

// 未开启选项维持原拒收；Windows 主机即使开启也不放行
func TestDockerInstallRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	if _, err := BuildSessionPlan(pp, allOnHost(3, false), testHosts(), nil, nil, "fresh", nil); err == nil ||
		!strings.Contains(err.Error(), "无 docker") {
		t.Fatalf("未开启自动安装应拒收，实际 %v", err)
	}
	hosts := testHosts()
	hosts[3].Info.OS, hosts[3].Info.Systemd = "windows", false
	pl := allOnHost(3, false)
	pl.DockerInstall = true
	if _, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", nil); err == nil ||
		!strings.Contains(err.Error(), "仅支持 Linux") {
		t.Fatalf("Windows 应明确拒绝，实际 %v", err)
	}
}

// 自定义/导入 DAG 丢掉安装节点时的保底注入：enforceDockerNodes 补插并重新接线
func TestOverlayDockerEnforcement(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := allOnHost(3, false)
	pl.DockerInstall = true
	opts := &DockerOpts{ScriptURL: "https://example.com/get-docker.sh"}
	auto, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", opts)
	if err != nil {
		t.Fatal(err)
	}
	var custom []DagNode // 刻意剔除 docker-install 节点（导入旧文件场景）
	for _, n := range auto.Nodes {
		if n.Kind == "docker" {
			continue
		}
		custom = append(custom, DagNode{ID: n.Key, Kind: n.Kind, Title: n.Title, Host: n.Host, Deps: n.Deps})
	}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, custom, "fresh", opts)
	if err != nil {
		t.Fatal("保底注入后自定义 DAG 应通过:", err)
	}
	dn := planNodeByKey(t, res, "docker-install-h3")
	if !depHas(planNodeByKey(t, res, "load-h3").Deps, dn.Key) {
		t.Error("注入后 load-h3 应重挂安装依赖")
	}
}

// schema cmd 节点经 UI/导入往返（无脚本）应回用引擎物化载荷（修复回归：此前报「需要主机与脚本」）
func TestSchemaCmdRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "web": 1}, // mysql 外部复用
		Vars: map[string]string{"TENANT_ID": "t"},
		Ext:  map[string]string{"DB_HOST": "10.1.1.1", "DB_PORT": "3306", "DB_URL": "http://10.1.1.1:3306"}}
	auto, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var custom []DagNode
	schemaSeen := false
	for _, n := range auto.Nodes {
		if n.Kind == "cmd" {
			schemaSeen = true
		}
		custom = append(custom, DagNode{ID: n.Key, Kind: n.Kind, Title: n.Title, Host: n.Host, Deps: n.Deps}) // Script 不带（UI 往返丢）
	}
	if !schemaSeen {
		t.Fatal("夹具应产出 schema cmd 节点")
	}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, custom, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range res.Nodes {
		if n.Kind != "cmd" {
			continue
		}
		var p CmdPayload
		if err := json.Unmarshal(n.Payload, &p); err != nil || !strings.Contains(p.Script, "docker run") {
			t.Fatalf("schema 载荷应回用引擎物化: %+v %v", p, err)
		}
	}
}

// ---- 组件三态（跳过=不装不复用）与 ask 凭据留空=随机 ----

// addSkipFixture 在标准夹具上叠加 es 组件：声明（端点+ask 凭据）+ compose 块 + 镜像 tar +
// 模板组件段（---  # 组件 es 配置段 标记，验证剔除逻辑与打包引擎写出口径一致），重建 manifest
func addSkipFixture(t *testing.T, dir string) {
	t.Helper()
	var decl ComponentsDecl
	data, err := os.ReadFile(filepath.Join(dir, componentsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decl); err != nil {
		t.Fatal(err)
	}
	decl.Components = append(decl.Components, ComponentDecl{
		Code: "es", Category: "middleware", Services: []string{"elasticsearch"}, Images: []string{"mse/elasticsearch:9.2.8"},
		Endpoints:   []Endpoint{{Placeholder: "ES_URIS", Service: "elasticsearch", Port: "9200", From: "url", Scheme: "http"}},
		Credentials: []Credential{{Placeholder: "ES_SECRET", Env: "ES_SECRET", Generate: "ask"}},
	})
	overwrite(t, dir, componentsFile, string(mustJSON(decl)))
	composeData, _ := os.ReadFile(filepath.Join(dir, composeFile))
	overwrite(t, dir, composeFile, string(composeData)+
		"  elasticsearch:\n    image: mse/elasticsearch:9.2.8\n    ports:\n      - \"9200:9200\"\n")
	overwrite(t, dir, templateFile,
		"mse:\n  tenant: {{TENANT_ID}}\n  db: {{DB_URL}}\n  pw: {{DB_PASSWORD}}\n"+
			"---  # 组件 es 配置段\nspring:\n  elasticsearch:\n    uris: {{ES_URIS}}\n    secret: {{ES_SECRET}}\n")
	overwrite(t, dir, "images/mse_elasticsearch_9.2.8.tar", "fake-tar-es")
	remanifest(t, dir)
}

func overwrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func remanifest(t *testing.T, dir string) {
	t.Helper()
	manifest := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == manifestFile {
			return nil
		}
		data, _ := os.ReadFile(path)
		sum := sha256.Sum256(data)
		manifest[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	mj, _ := json.Marshal(map[string]any{"files": manifest})
	overwrite(t, dir, manifestFile, string(mj))
}

// 跳过组件：配置段剔除（ES_URIS/ES_SECRET 不再渲染）、凭据免填不进 .env、服务不进编排
func TestSkipComponentPlan(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	addSkipFixture(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "web": 1, "mysql": 1},
		Vars: map[string]string{"TENANT_ID": "t"}, Ext: map[string]string{}, Skip: []string{"es"}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal("跳过 es 后不应要求 ES 变量/端点:", err)
	}
	for _, n := range res.Nodes {
		if n.Kind != "compose" {
			continue
		}
		var cp ComposePayload
		_ = json.Unmarshal(n.Payload, &cp)
		if strings.Contains(cp.Compose, "elasticsearch") {
			t.Errorf("跳过组件的服务不应进 compose: %s", cp.Compose)
		}
		conf := cp.Files["data/conf/application.yaml"]
		if strings.Contains(conf, "elasticsearch") || strings.Contains(conf, "ES_") {
			t.Errorf("跳过组件的配置段应剔除: %q", conf)
		}
		if strings.Contains(cp.Env, "ES_SECRET") {
			t.Errorf("跳过组件凭据不应进 .env: %q", cp.Env)
		}
		if !strings.Contains(conf, "db: http://mysql:3306") {
			t.Errorf("未跳过组件渲染应不受影响: %q", conf)
		}
	}
}

// 跳过标记与分配互斥：组件服务仍分配主机 → 硬错误
func TestSkipWithPlacementRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	addSkipFixture(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "web": 1, "mysql": 1, "elasticsearch": 1},
		Vars: map[string]string{"TENANT_ID": "t"}, Ext: map[string]string{}, Skip: []string{"es"}}
	if _, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil); err == nil {
		t.Fatal("跳过组件仍分配服务应报错")
	}
}

// 外部复用（不跳过）：ask 凭据必须填写（外部实例口令无法随机生成）
func TestExternalAskCredRequired(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	addSkipFixture(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "web": 1, "mysql": 1},
		Vars: map[string]string{"TENANT_ID": "t"},
		Ext:  map[string]string{"ES_URIS": "http://10.9.9.9:9200"}}
	_, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err == nil || !strings.Contains(err.Error(), "外部复用") {
		t.Fatalf("外部复用组件 ask 凭据缺值应报错，实际: %v", err)
	}
}

// 落机组件 ask 凭据留空=随机生成（与安装表单提示一致）
func TestPlacedAskCredRandom(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	addSkipFixture(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "web": 1, "mysql": 1, "elasticsearch": 1},
		Vars: map[string]string{"TENANT_ID": "t"}, Ext: map[string]string{}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range res.Nodes {
		if n.Kind != "compose" {
			continue
		}
		var cp ComposePayload
		_ = json.Unmarshal(n.Payload, &cp)
		for _, ln := range strings.Split(cp.Env, "\n") {
			if v, ok := strings.CutPrefix(ln, "ES_SECRET="); ok && len(v) >= 16 {
				found = true
			}
		}
		if cp.Files["data/conf/application.yaml"] != "" && !strings.Contains(cp.Files["data/conf/application.yaml"], "uris: http://elasticsearch:9200") {
			t.Errorf("es 落机端点应为服务名: %q", cp.Files["data/conf/application.yaml"])
		}
	}
	if !found {
		t.Error("落机组件 ask 凭据留空应随机生成并进 .env")
	}
}

// ---- 微服务形态支持：基线全局渲染 / cc 组件不可跳过 / 单元健康等待 ----

// 基线全局形态：nacos 与 app 同机时，Data ID 里的端点也取「主机地址:发布端口」——
// Data ID 被全部消费服务共享，服务名形态仅本机 compose 网络内有效，不能进基线
func TestConfigCenterBaselineGlobalForm(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1, "nacos": 1}, // 同机
		Vars: map[string]string{}, Ext: map[string]string{}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var p PluginPayload
	if err := json.Unmarshal(ccNode(t, res, "cc-apply-nacos").Payload, &p); err != nil {
		t.Fatal(err)
	}
	want := "server:\n  nacos: http://10.0.0.1:8848\n  identity: security\n"
	if p.Items[0].Content != want {
		t.Errorf("同机基线应为主机地址形态（非服务名）:\nwant %q\ngot  %q", want, p.Items[0].Content)
	}
	if strings.Contains(p.Items[0].Content, "nacos:8848") {
		t.Errorf("基线不应出现服务名端点: %q", p.Items[0].Content)
	}
}

// 配置基线载体组件（configCenter 声明）不可跳过
func TestConfigCenterSkipRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestPkgCC(t, dir)
	pp := parseTestPkg(t, dir)
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{"app": 1},
		Vars: map[string]string{}, Ext: map[string]string{"NACOS_ADDR": "http://10.9.9.9:8848"}, Skip: []string{"nacos"}}
	_, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err == nil || !strings.Contains(err.Error(), "不能跳过") {
		t.Fatalf("跳过配置基线载体应报错，实际: %v", err)
	}
}

// 多单元包：非入口单元各生成 wait-unit-<svc> 健康节点，入口 wait 依赖之
// （web 单元本无健康路径——夹具手术给它挂上，验证生成与接线；单体单后端包零新增节点）
func TestWaitUnitNodes(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	var decl ComponentsDecl
	data, _ := os.ReadFile(filepath.Join(dir, componentsFile))
	if err := json.Unmarshal(data, &decl); err != nil {
		t.Fatal(err)
	}
	for i := range decl.Units {
		if decl.Units[i].Code == "web" {
			decl.Units[i].HealthPath = "/healthz"
			decl.Units[i].Port = "80"
		}
	}
	overwrite(t, dir, componentsFile, string(mustJSON(decl)))
	remanifest(t, dir)
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, allOnHost(1, false), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	unitWait := planNodeByKey(t, res, "wait-unit-web")
	if unitWait.Kind != "wait" {
		t.Errorf("wait-unit-web 类型 = %s", unitWait.Kind)
	}
	if !depHas(unitWait.Deps, "compose-h1") {
		t.Errorf("wait-unit-web 应依赖其主机编排: %v", unitWait.Deps)
	}
	entry := planNodeByKey(t, res, "wait-entry")
	if !depHas(entry.Deps, "wait-unit-web") {
		t.Errorf("wait-entry 应依赖单元健康节点: %v", entry.Deps)
	}
}

// 迁移驱逐：服务换宿主（inventory 记 1、新计划分到 2，变量与基线完全一致——盲区场景）→
// 新宿主强制保留 compose 节点 + 旧宿主注入 cleanup 节点（v2/v1 双命名幂等清理）
func TestMigrationEviction(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp, allOnHost(1, false), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl2 := allOnHost(2, false)
	pl2.Vars["DB_PASSWORD"] = fresh.Vars["DB_PASSWORD"]
	mig, err := BuildSessionPlan(pp, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup *planNode
	hasCompose2 := false
	for i := range mig.Nodes {
		if mig.Nodes[i].Key == "compose-h2" {
			hasCompose2 = true
		}
		if mig.Nodes[i].Kind == "cmd" && mig.Nodes[i].InvEvictHost == 1 {
			cleanup = &mig.Nodes[i]
		}
	}
	if !hasCompose2 {
		t.Fatalf("迁移后新宿主必须保留 compose 节点（盲区修复）：%v", keys(mig.Nodes))
	}
	if cleanup == nil {
		t.Fatalf("旧宿主应注入 cleanup 节点：%v", keys(mig.Nodes))
	}
	var p CmdPayload
	if json.Unmarshal(cleanup.Payload, &p) != nil {
		t.Fatal("cleanup 载荷解析失败")
	}
	if !strings.Contains(p.Script, "-h1-mysql-1") || !strings.Contains(p.Script, "-h1-mysql_1") {
		t.Errorf("cleanup 脚本应含 v2/v1 双命名清理：%s", p.Script)
	}
	if !strings.Contains(p.Script, "exit 0") {
		t.Errorf("cleanup 脚本应幂等容错（exit 0）：%s", p.Script)
	}
	if len(cleanup.InvEvictSvcs) == 0 {
		t.Errorf("cleanup 节点应携带驱逐服务清单：%v", cleanup.InvEvictSvcs)
	}
}

// injectConfLabel：配置指纹注入（K8s checksum 模式）——无 labels 段按属性缩进新建；
// 已有 labels 段追加不重建（避免 YAML 重复键）
func TestInjectConfLabel(t *testing.T) {
	blk := "  mysql:\n    image: mysql:8.4\n    ports:\n      - \"3306:3306\"\n"
	out := injectConfLabel(blk, "abc123")
	if !strings.Contains(out, "mse.conf-hash: \"abc123\"") {
		t.Fatalf("应注入 conf-hash: %q", out)
	}
	if !strings.Contains(out, "    labels:\n      mse.conf-hash:") {
		t.Errorf("无 labels 段时应按属性缩进新建: %q", out)
	}
	blk2 := "  app:\n    image: x\n    labels:\n      a: b\n"
	out2 := injectConfLabel(blk2, "h2")
	if strings.Count(out2, "labels:") != 1 || !strings.Contains(out2, "a: b") {
		t.Errorf("已有 labels 段应追加: %q", out2)
	}
	if !strings.Contains(out2, "mse.conf-hash: \"h2\"") {
		t.Errorf("追加行缺失: %q", out2)
	}
}

// 跨机端点门控：app 与 mysql 分机、app 无 depends_on（真实包常见——应用级依赖仅存在于配置
// 端点引用 {{DB_URL}}→mysql）→ 消费方编排节点须依赖 wait-mysql 就绪门控；否则 app 先于 mysql
// 就绪启动 → 崩溃循环 → wait-app 600s 超时（实测会话 #32 根因）
func TestCrossHostEndpointGate(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	// 复写 compose：去掉 app 的 depends_on mysql（应用级依赖仅存在于端点引用——真实包常见）
	compose := `name: mse
services:
  app:
    image: mse/app:1.0.0
    ports:
      - "8080:8080"
    volumes:
      - ./data/conf:/app/conf
  web:
    image: mse/web:1.0.0
    ports:
      - 80:80
    depends_on:
      - app
  mysql:
    image: mysql:8.4
    ports:
      - "3306:3306"
    volumes:
      - ./data/mysql:/var/lib/mysql
    environment:
      - MYSQL_ROOT_PASSWORD=${DB_PASSWORD}
`
	if err := os.WriteFile(filepath.Join(dir, composeFile), []byte(compose), 0644); err != nil {
		t.Fatal(err)
	}
	// manifest 逐文件哈希：同步更新被改写文件的哈希（parseTestPkg 与上传即校验同口径）
	manifestPath := filepath.Join(dir, manifestFile)
	raw, _ := os.ReadFile(manifestPath)
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(compose))
	manifest.Files[composeFile] = hex.EncodeToString(sum[:])
	mj, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, mj, 0644); err != nil {
		t.Fatal(err)
	}
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	// app/web → 主机1；mysql → 主机2（实测 #32 场景：k8s-node1/node2 分机）
	pl := &Placement{Mode: map[string]string{}, Host: map[string]int64{
		"app": 1, "web": 1, "mysql": 2,
	}, Vars: map[string]string{"TENANT_ID": "tenant-01"}, Ext: map[string]string{}}
	res, err := BuildSessionPlan(pp, pl, hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	hasWaitMysql := false
	h1Compose := 0
	for _, n := range res.Nodes {
		if n.Key == "wait-mysql" {
			hasWaitMysql = true
			// wait-mysql 应在 mysql 所在主机（2）执行探测
			if n.Host != 2 {
				t.Errorf("wait-mysql 应在主机2 探测，实际主机 %d", n.Host)
			}
		}
		if n.Kind == "compose" && n.Host == 1 {
			h1Compose++
			if !slices.Contains(n.Deps, "wait-mysql") {
				t.Errorf("主机1 编排节点 %s 应依赖 wait-mysql（跨机端点引用门控）：%v", n.Key, n.Deps)
			}
		}
	}
	if !hasWaitMysql {
		t.Fatalf("跨机端点引用应生成 wait-mysql 门控：%v", keys(res.Nodes))
	}
	if h1Compose == 0 {
		t.Fatal("主机1 应有 compose 节点")
	}
}
