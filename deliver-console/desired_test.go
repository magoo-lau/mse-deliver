// desired_test.go —— format:4 角色链（MySQL 主从）/ 服务级哈希 / 统一 diff / 覆盖重装 /
// 逐节点 inventory 回写 的引擎与执行器测试。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPackageHA format:4 主从测试包：app（单元）+ mysql 组件（primary/replica 双角色，
// per-role my.cnf、GTID、初始化链、exec 探针、反亲和）。replica 的 join 用端点占位符跨机寻址主库。
func writeTestPackageHA(t *testing.T, dir string, mysqlImage string) {
	t.Helper()
	appImg := "mse/app:2.0.0"
	compose := fmt.Sprintf(`name: mseha
services:
  app:
    image: %s
    ports:
      - "8080:8080"
    depends_on:
      - mysql-primary
    volumes:
      - ./data/conf:/app/conf
    environment:
      - TENANT_APP_ID=${TENANT_ID}
  mysql-primary:
    image: %s
    ports:
      - "3306:3306"
    volumes:
      - ./data/mysql-primary:/var/lib/mysql
      - ./data/mysql-primary/conf.d:/etc/mysql/conf.d:ro
    environment:
      - MYSQL_ROOT_PASSWORD=${MYSQL_ROOT_PASSWORD}
  mysql-replica:
    image: %s
    ports:
      - "3306:3306"
    volumes:
      - ./data/mysql-replica:/var/lib/mysql
      - ./data/mysql-replica/conf.d:/etc/mysql/conf.d:ro
    environment:
      - MYSQL_ROOT_PASSWORD=${MYSQL_ROOT_PASSWORD}
`, appImg, mysqlImage, mysqlImage)
	primaryCnf := "[mysqld]\nserver-id={{MYSQL_SERVER_ID}}\ngtid_mode=ON\nenforce_gtid_consistency=ON\nlog-bin=binlog\n"
	replicaCnf := "[mysqld]\nserver-id={{MYSQL_SERVER_ID}}\ngtid_mode=ON\nenforce_gtid_consistency=ON\nread_only=ON\n"
	decl := ComponentsDecl{Format: 4, Name: "mseha", Version: "1.0.0",
		Entry: EntryDecl{Unit: "app", Service: "app", Image: appImg, Port: "8080", HealthPath: "/actuator/health"},
		Units: []UnitDecl{
			{Code: "app", Service: "app", Services: []string{"app"}, Image: appImg,
				Port: "8080", HealthPath: "/actuator/health", ConfDir: "/app/conf"},
		},
		Variables: []VarDecl{{Name: "TENANT_ID", Label: "租户编号", Source: "input", Value: "mse"}},
		Components: []ComponentDecl{{
			Code: "mysql", Category: "database",
			Services: []string{"mysql-primary", "mysql-replica"}, Images: []string{mysqlImage},
			SpreadHosts: true,
			Endpoints: []Endpoint{
				{Placeholder: "DB_HOST", Service: "mysql-primary", Port: "3306", From: "host"},
				{Placeholder: "DB_PORT", Service: "mysql-primary", Port: "3306", From: "port"},
			},
			Credentials: []Credential{
				{Placeholder: "MYSQL_ROOT_PASSWORD", Env: "MYSQL_ROOT_PASSWORD", Generate: "random"},
				{Placeholder: "MYSQL_REPL_USER", Env: "MYSQL_REPL_USER", Generate: "const", Value: "repl"},
				{Placeholder: "MYSQL_REPL_PASSWORD", Env: "MYSQL_REPL_PASSWORD", Generate: "random"},
			},
			Roles: []RoleDecl{
				{Name: "primary", Services: []string{"mysql-primary"},
					Vars:  map[string]string{"MYSQL_SERVER_ID": "1"},
					Files: []FileDecl{{Path: "data/mysql-primary/conf.d/my.cnf", Template: primaryCnf}},
					Readiness: &ProbeDecl{Type: "tcp", Port: "3306"},
					Init: []InitDecl{{ID: "repl-user", Script: "mysql -uroot -p{{MYSQL_ROOT_PASSWORD}} -e \"CREATE USER IF NOT EXISTS '{{MYSQL_REPL_USER}}'@'%' IDENTIFIED BY '{{MYSQL_REPL_PASSWORD}}'; GRANT REPLICATION REPLICA ON *.* TO '{{MYSQL_REPL_USER}}'@'%';\""}},
				},
				{Name: "replica", Services: []string{"mysql-replica"},
					Vars:  map[string]string{"MYSQL_SERVER_ID": "2"},
					Files: []FileDecl{{Path: "data/mysql-replica/conf.d/my.cnf", Template: replicaCnf}},
					Readiness: &ProbeDecl{Type: "exec", Script: "mysql -uroot -p{{MYSQL_ROOT_PASSWORD}} -e 'SHOW REPLICA STATUS\\G' | grep -q 'Slave_IO_Running: Yes'"},
					Init: []InitDecl{{ID: "join", After: "primary",
						Script: "mysql -uroot -p{{MYSQL_ROOT_PASSWORD}} -e \"STOP REPLICA; CHANGE REPLICATION SOURCE TO SOURCE_HOST='{{DB_HOST}}', SOURCE_PORT={{DB_PORT}}, SOURCE_USER='{{MYSQL_REPL_USER}}', SOURCE_PASSWORD='{{MYSQL_REPL_PASSWORD}}', SOURCE_AUTO_POSITION=1; START REPLICA;\""}},
				},
			},
		}},
	}
	files := map[string]string{
		composeFile:                compose,
		componentsFile:             string(mustJSON(decl)),
		templateFile:               "mse:\n  tenant: {{TENANT_ID}}\n  db: {{DB_HOST}}\n",
		"images/mse_app_2.0.0.tar": "fake-tar-app",
		"images/" + safeImageTarName(mysqlImage): "fake-tar-mysql",
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

func haPlacement(primaryHost, replicaHost int64) *Placement {
	return &Placement{Mode: map[string]string{}, Host: map[string]int64{
		"app": primaryHost, "mysql-primary": primaryHost, "mysql-replica": replicaHost,
	}, Vars: map[string]string{"TENANT_ID": "tenant-01"}, Ext: map[string]string{}}
}

func TestMySQLHAFreshPlan(t *testing.T) {
	dir := t.TempDir()
	writeTestPackageHA(t, dir, "mysql:8.4")
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, haPlacement(1, 2), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]planNode{}
	for _, n := range res.Nodes {
		byKey[n.Key] = n
	}
	// 主从就绪链：primary tcp 探测；replica exec 探针（cmd 重试环）
	wp, ok := byKey["wait-mysql-primary"]
	if !ok {
		t.Fatal("缺少 wait-mysql-primary")
	}
	var wpp WaitPayload
	_ = json.Unmarshal(wp.Payload, &wpp)
	if wpp.Type != "tcp" || wpp.Port != "3306" {
		t.Errorf("primary 探针应为 tcp 3306: %+v", wpp)
	}
	wr, ok := byKey["wait-mysql-replica"]
	if !ok || wr.Kind != "cmd" {
		t.Fatalf("replica exec 探针应为 cmd 节点: %+v", wr)
	}
	var wrp CmdPayload
	_ = json.Unmarshal(wr.Payload, &wrp)
	if !strings.Contains(wrp.Script, "docker exec mseha-h2-mysql-replica-1") || !strings.Contains(wrp.Script, "SHOW REPLICA STATUS") {
		t.Errorf("replica 探针脚本应 docker exec 进容器执行 SHOW REPLICA STATUS:\n%s", wrp.Script)
	}
	if !strings.Contains(wrp.Script, "seq 1 120") {
		t.Errorf("探针应带重试环:\n%s", wrp.Script)
	}
	// 初始化链：primary 建号（docker exec primary 容器）；replica join 依赖 primary 建号 + replica 就绪
	ip, ok := byKey["init-mysql-primary-repl-user"]
	if !ok || ip.Host != 1 {
		t.Fatalf("缺少 primary 初始化节点或主机不符: %+v", ip)
	}
	var ipp CmdPayload
	_ = json.Unmarshal(ip.Payload, &ipp)
	if !strings.Contains(ipp.Script, "docker exec mseha-h1-mysql-primary-1") || !strings.Contains(ipp.Script, "CREATE USER IF NOT EXISTS") {
		t.Errorf("primary 初始化应 docker exec 建复制账号:\n%s", ipp.Script)
	}
	if !contains(ip.Deps, "wait-mysql-primary") {
		t.Errorf("primary 初始化应依赖 primary 就绪: %v", ip.Deps)
	}
	ij, ok := byKey["init-mysql-replica-join"]
	if !ok {
		t.Fatal("缺少 init-mysql-replica-join")
	}
	if !contains(ij.Deps, "wait-mysql-replica") || !contains(ij.Deps, "init-mysql-primary-repl-user") {
		t.Errorf("join 应依赖 replica 就绪 + primary 建号（After）: %v", ij.Deps)
	}
	var ijp CmdPayload
	_ = json.Unmarshal(ij.Payload, &ijp)
	if !strings.Contains(ijp.Script, "SOURCE_AUTO_POSITION=1") || !strings.Contains(ijp.Script, "10.0.0.1") {
		t.Errorf("join 应渲染跨机主库地址（GTID 自动定位）:\n%s", ijp.Script)
	}
	// 入口确认叠加复制链就绪
	we := byKey["wait-entry"]
	if !contains(we.Deps, "init-mysql-replica-join") || !contains(we.Deps, "wait-mysql-replica") {
		t.Errorf("wait-entry 应依赖初始化链与 exec 探针: %v", we.Deps)
	}
	// per-role 配置文件：双主机各自下发，server-id 区分
	var cp1, cp2 ComposePayload
	_ = json.Unmarshal(byKey["compose-h1"].Payload, &cp1)
	_ = json.Unmarshal(byKey["compose-h2"].Payload, &cp2)
	if got := cp1.Files["data/mysql-primary/conf.d/my.cnf"]; !strings.Contains(got, "server-id=1") || !strings.Contains(got, "gtid_mode=ON") {
		t.Errorf("primary my.cnf 渲染不符: %q", got)
	}
	if got := cp2.Files["data/mysql-replica/conf.d/my.cnf"]; !strings.Contains(got, "server-id=2") || !strings.Contains(got, "read_only=ON") {
		t.Errorf("replica my.cnf 渲染不符: %q", got)
	}
	if _, dup := cp2.Files["data/mysql-primary/conf.d/my.cnf"]; dup {
		t.Error("replica 主机不应下发 primary 配置文件")
	}
	// app 配置仍走单模板通道（format:3 兼容路径）
	if !strings.Contains(cp1.Files["data/conf/application.yaml"], "db: mysql-primary") {
		t.Errorf("app 配置端点应渲染主库跨机地址: %v", cp1.Files)
	}
	// conf-hash label 服务级：primary/replica/app 三者指纹互不相同
	if h1 := labelHashOf(cp1.Compose, "mysql-primary"); h1 == "" || labelHashOf(cp1.Compose, "mysql-primary") == labelHashOf(cp1.Compose, "app") {
		t.Errorf("primary 与 app 的 conf-hash 应不同: %q", cp1.Compose)
	}
	if labelHashOf(cp2.Compose, "mysql-replica") == labelHashOf(cp1.Compose, "mysql-primary") {
		t.Error("replica 与 primary 的 conf-hash 应不同（server-id 不同）")
	}
	// 统一 diff：fresh 全 create
	for _, c := range res.Changes {
		if c.Action != "create" {
			t.Errorf("fresh 变更应为 create：%+v", c)
		}
	}
}

// labelHashOf 从 compose 文本提取指定服务块的 mse.conf-hash label 值（测试助手）
func labelHashOf(composeText, svc string) string {
	blocks := splitServices(composeText)
	blk := blocks[svc]
	for _, line := range strings.Split(blk, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "mse.conf-hash:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func TestMySQLHAAntiAffinity(t *testing.T) {
	dir := t.TempDir()
	writeTestPackageHA(t, dir, "mysql:8.4")
	pp := parseTestPkg(t, dir)
	_, err := BuildSessionPlan(pp, haPlacement(1, 1), testHosts(), nil, nil, "fresh", nil)
	if err == nil || !strings.Contains(err.Error(), "反亲和") {
		t.Fatalf("主从同机应被反亲和硬拦，实际 err=%v", err)
	}
}

func TestMySQLHAIncrementalNoChange(t *testing.T) {
	dir := t.TempDir()
	writeTestPackageHA(t, dir, "mysql:8.4")
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp, haPlacement(1, 2), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl2 := haPlacement(1, 2)
	for k, v := range fresh.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	same, err := BuildSessionPlan(pp, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Nodes) != 0 {
		t.Errorf("零变化增量节点应为 0，实际 %v", keys(same.Nodes))
	}
	for _, c := range same.Changes {
		if c.Action != "nochange" {
			t.Errorf("零变化 diff 应全 nochange：%+v", c)
		}
	}
}

// 镜像升级（Docker 容器升级）：仅 mysql 双角色 update（镜像变更），app nochange；
// 先从后主接线：primary 宿主编排后置于 replica 就绪
func TestMySQLHAUpgradeImageOnly(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeTestPackageHA(t, dir1, "mysql:8.4")
	writeTestPackageHA(t, dir2, "mysql:8.5")
	pp1 := parseTestPkg(t, dir1)
	pp2 := parseTestPkg(t, dir2)
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp1, haPlacement(1, 2), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl2 := haPlacement(1, 2)
	for k, v := range fresh.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	up, err := BuildSessionPlan(pp2, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]svcChange{}
	for _, c := range up.Changes {
		actions[c.Service] = c
	}
	if actions["app"].Action != "nochange" {
		t.Errorf("app 应零触碰：%+v", actions["app"])
	}
	if actions["mysql-primary"].Action != "update" || !contains(actions["mysql-primary"].Reasons, "镜像变更") {
		t.Errorf("primary 应为镜像变更 update：%+v", actions["mysql-primary"])
	}
	if actions["mysql-replica"].Action != "update" {
		t.Errorf("replica 应 update：%+v", actions["mysql-replica"])
	}
	byKey := map[string]planNode{}
	for _, n := range up.Nodes {
		byKey[n.Key] = n
	}
	if _, ok := byKey["compose-h1"]; !ok {
		t.Fatal("primary 宿主编排节点应保留")
	}
	if !contains(byKey["compose-h1"].Deps, "wait-mysql-replica") {
		t.Errorf("先从后主：primary 宿主编排应后置于 replica 就绪: %v", byKey["compose-h1"].Deps)
	}
	if _, ok := byKey["init-mysql-replica-join"]; !ok {
		t.Error("角色变更时初始化链应保留（join 幂等重放）")
	}
}

// 服务级哈希隔离：改 app 专属变量（TENANT_ID）→ 仅 app update，mysql 双角色 nochange
func TestServiceHashIsolation(t *testing.T) {
	dir := t.TempDir()
	writeTestPackageHA(t, dir, "mysql:8.4")
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp, haPlacement(1, 2), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl2 := haPlacement(1, 2)
	pl2.Vars["TENANT_ID"] = "tenant-02" // 变更
	for k, v := range fresh.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	up, err := BuildSessionPlan(pp, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range up.Changes {
		switch c.Service {
		case "app":
			if c.Action != "update" {
				t.Errorf("app 应 update：%+v", c)
			}
		case "mysql-primary", "mysql-replica":
			if c.Action != "nochange" {
				t.Errorf("%s 应 nochange（服务级哈希隔离）：%+v", c.Service, c)
			}
		}
	}
}

// 覆盖重装：全量物化执行（节点不裁剪）+ 迁移清理追加 + 数据保留告示
func TestOverwriteMode(t *testing.T) {
	dir := t.TempDir()
	writeTestPackageHA(t, dir, "mysql:8.4")
	pp := parseTestPkg(t, dir)
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp, haPlacement(1, 2), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl := haPlacement(1, 2) // 相同分配、相同包
	for k, v := range fresh.Vars {
		if pl.Vars[k] == "" {
			pl.Vars[k] = v
		}
	}
	ow, err := BuildSessionPlan(pp, pl, hosts, fresh.InvRows, nil, "overwrite", nil)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]bool{}
	for _, n := range ow.Nodes {
		byKey[n.Key] = true
	}
	for _, key := range []string{"compose-h1", "compose-h2", "load-h1", "load-h2", "init-mysql-primary-repl-user", "init-mysql-replica-join"} {
		if !byKey[key] {
			t.Errorf("覆盖重装应全量保留 %s（实际节点 %v）", key, keys(ow.Nodes))
		}
	}
	if ow.Mode != "overwrite" {
		t.Errorf("mode = %s", ow.Mode)
	}
	// 迁移：app 移到主机 2 → 旧主机 1 清理节点追加
	pl2 := haPlacement(1, 2)
	pl2.Host["app"] = 2
	for k, v := range fresh.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	ow2, err := BuildSessionPlan(pp, pl2, hosts, fresh.InvRows, nil, "overwrite", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range ow2.Nodes {
		if n.Key == "cleanup-h1" && contains(n.InvEvictSvcs, "app") {
			found = true
		}
	}
	if !found {
		t.Errorf("覆盖重装迁移应追加 cleanup-h1（app）: %v", keys(ow2.Nodes))
	}
}

// 逐节点 inventory 回写：节点 ok 即入库（会话未完成/最终失败也保留已成功部分）
func TestSessionPerNodeInventoryWriteback(t *testing.T) {
	srv, _ := newTestServer(t)
	node := planNode{Key: "compose-h1", Kind: "compose", Title: "编排", Host: 1,
		InvCompose: []string{"app"},
		InvDetail:  map[string]string{"app": `{"kind":"compose","blockHash":"x"}`}}
	snap := &planSnapshot{Nodes: []planNode{node}, Mode: "fresh"}
	ss := &sessionRow{App: "mseha", Version: "1.0.0", Mode: "fresh", Plan: string(mustJSON(snap)), Placement: "{}"}
	if err := srv.store.InsertSession(ss, snap.Nodes); err != nil {
		t.Fatal(err)
	}
	srv.StartSession(ss.ID, "mseha", "", snap)
	run := srv.session(ss.ID)
	if run == nil {
		t.Fatal("会话未启动")
	}
	run.Report("compose-h1", "ok", "up 完成", nil)
	inv, err := srv.store.InventoryByApp("mseha")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 || inv[0].Service != "app" || inv[0].HostID != 1 {
		t.Fatalf("节点 ok 应即时回写 inventory: %+v", inv)
	}
	// 驱逐节点 ok 即删行
	cn := planNode{Key: "cleanup-h1", Kind: "cmd", Title: "清理", Host: 1, InvEvictHost: 1, InvEvictSvcs: []string{"app"}}
	run.nodes = append(run.nodes, cn)
	run.byKey[cn.Key] = &run.nodes[len(run.nodes)-1]
	run.status[cn.Key] = "failed" // 先置非终态，模拟驱逐过程
	run.Report("cleanup-h1", "ok", "已清理", nil)
	inv2, _ := srv.store.InventoryByApp("mseha")
	if len(inv2) != 0 {
		t.Fatalf("驱逐成功应删 inventory 行: %+v", inv2)
	}
}

// format:3 旧包行为兼容：无 roles/ConfigFiles 的包在新引擎下配置通道与 conf-hash 与既有断言一致
func TestFormat3CompatConfFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, allOnHost(1, false), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var cp ComposePayload
	for _, n := range res.Nodes {
		if n.Key == "compose-h1" {
			_ = json.Unmarshal(n.Payload, &cp)
		}
	}
	if got := cp.Files["data/conf/application.yaml"]; !strings.Contains(got, "db: http://mysql:3306") {
		t.Errorf("format:3 单模板路径应保持: %q", got)
	}
	// 单元服务有 conf-hash label；无配置的组件服务（mysql）不再注入（服务级口径）
	if labelHashOf(cp.Compose, "app") == "" {
		t.Error("app（有配置）应注入 conf-hash")
	}
	if labelHashOf(cp.Compose, "mysql") != "" {
		t.Error("mysql（无配置文件集）不应注入 conf-hash（服务级口径）")
	}
}
