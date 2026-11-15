// topology_test.go —— console 组态器：format:3 老包（单 mysql 组件）经 placement.instances
// 组态主从（内置拓扑模板展开→format:4 角色链编译）+ 资源限额注入/diff 的引擎测试。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// haConfiguredPlacement format:3 测试包（writeTestPackage：app/web/mysql）+ mysql 组件组态
// mysql-ha-1（1主1从）：主库与 app 同机、从库跨机——顺带验证原 mysql 放置/依赖被接管清理
func haConfiguredPlacement(primaryHost, replicaHost int64) *Placement {
	pl := allOnHost(primaryHost, false) // app/web/mysql 全在 primaryHost（mysql 原服务随后被组态清理）
	pl.Host["mysql-primary"] = primaryHost
	pl.Host["mysql-replica"] = replicaHost
	pl.Instances = []InstanceSpec{
		{Component: "mysql", Form: "mysql-ha-1", Service: "mysql-primary", Role: "primary"},
		{Component: "mysql", Form: "mysql-ha-1", Service: "mysql-replica", Role: "replica"},
	}
	return pl
}

// TestTopologyMySQLHAFromPlainPackage format:3 老包免重打包组态主从全链：
// 实例展开/角色链/端点重定向/凭据补充/依赖重写/反亲和/GTID 脚本
func TestTopologyMySQLHAFromPlainPackage(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	res, err := BuildSessionPlan(pp, haConfiguredPlacement(1, 2), testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]planNode{}
	for _, n := range res.Nodes {
		byKey[n.Key] = n
	}
	// 实例角色链：主库建号（After 目标=主库实例名）；从库 join 依赖主库建号 + 从库就绪
	ip := byKey["init-mysql-primary-repl-user"]
	if ip.Host != 1 {
		t.Fatalf("缺少主库初始化节点或主机不符: %+v", ip)
	}
	ij, ok := byKey["init-mysql-replica-join"]
	if !ok {
		t.Fatal("缺少 init-mysql-replica-join")
	}
	if !contains(ij.Deps, "init-mysql-primary-repl-user") {
		t.Errorf("join 应依赖主库建号: %v", ij.Deps)
	}
	var ijp CmdPayload
	_ = json.Unmarshal(ij.Payload, &ijp)
	if !strings.Contains(ijp.Script, "docker exec mse-h2-mysql-replica-1") ||
		!strings.Contains(ijp.Script, "SOURCE_AUTO_POSITION=1") || !strings.Contains(ijp.Script, "10.0.0.1") {
		t.Errorf("join 应渲染跨机主库地址与 GTID 自动定位:\n%s", ijp.Script)
	}
	// 从库 exec 探针（cmd 重试环）
	wr, ok := byKey["wait-mysql-replica"]
	if !ok || wr.Kind != "cmd" {
		t.Fatalf("从库 exec 探针应为 cmd 节点: %+v", wr)
	}
	// 实例服务块：主库块挂 extras initdb、从库不挂；卷按实例名；conf.d 挂载就位
	var cp1, cp2 ComposePayload
	_ = json.Unmarshal(byKey["compose-h1"].Payload, &cp1)
	_ = json.Unmarshal(byKey["compose-h2"].Payload, &cp2)
	if !strings.Contains(cp1.Compose, "mysql-primary:") || !strings.Contains(cp1.Compose, "/docker-entrypoint-initdb.d") {
		t.Errorf("主库块应挂 extras initdb:\n%s", cp1.Compose)
	}
	if strings.Contains(cp2.Compose, "docker-entrypoint-initdb.d") || !strings.Contains(cp2.Compose, "mysql-replica:") {
		t.Errorf("从库块不应挂 initdb（防预建库复制冲突）:\n%s", cp2.Compose)
	}
	// per-role my.cnf：server-id 区分、read_only 只在从库
	if got := cp1.Files["data/mysql-primary/conf.d/my.cnf"]; !strings.Contains(got, "server-id=1") || !strings.Contains(got, "log-bin") {
		t.Errorf("主库 my.cnf 渲染不符: %q", got)
	}
	if got := cp2.Files["data/mysql-replica/conf.d/my.cnf"]; !strings.Contains(got, "server-id=1002") || !strings.Contains(got, "read_only=ON") {
		t.Errorf("从库 my.cnf 渲染不符: %q", got)
	}
	// 端点重定向：app 配置的 DB_URL 渲染为主库实例（同机=服务名形态）
	if got := cp1.Files["data/conf/application.yaml"]; !strings.Contains(got, "db: http://mysql-primary:3306") {
		t.Errorf("端点应重定向主库实例: %q", got)
	}
	// 依赖重写：app depends_on mysql → mysql-primary（同机就绪分层生效——主库进 base 层）
	if base := byKey["compose-h1-base"]; !strings.Contains(base.Title, "基础层") {
		t.Errorf("app 依赖主库应形成就绪分层（base 含 mysql-primary）: %q", base.Title)
	}
	// 模板凭据自动补充：.env 含复制账号密码（random）
	if !strings.Contains(cp2.Env, "MYSQL_REPL_PASSWORD=") || !strings.Contains(cp2.Env, "MYSQL_REPL_USER=repl") {
		t.Errorf("模板复制账号凭据应补进 .env: %q", cp2.Env)
	}
	// 原服务不再存在：无 mysql 服务、无对 mysql 的放置残留
	for _, n := range res.Nodes {
		if n.InvCompose != nil && contains(n.InvCompose, "mysql") && !contains(n.InvCompose, "mysql-primary") {
			t.Errorf("原 mysql 服务不应再登记 inventory: %v", n.InvCompose)
		}
	}
	// 从库镜像装载（组件镜像按实例主机裁剪）
	if !strings.Contains(string(byKey["load-h2"].Payload), "mysql_8.4.tar") {
		t.Errorf("从库主机应装载 mysql 镜像: %s", byKey["load-h2"].Title)
	}
}

// TestTopologyAntiAffinity 组态实例同机被反亲和硬拦
func TestTopologyAntiAffinity(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	_, err := BuildSessionPlan(pp, haConfiguredPlacement(1, 1), testHosts(), nil, nil, "fresh", nil)
	if err == nil || !strings.Contains(err.Error(), "反亲和") {
		t.Fatalf("主从同机应被反亲和硬拦，实际 err=%v", err)
	}
}

// TestTopologyIncrementalOrdering 组态主从的增量升级：镜像升级仅双实例 update + 先从后主接线
func TestTopologyIncrementalOrdering(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeTestPackage(t, dir1) // mysql:8.4 → 改 8.4.5（镜像升级场景）
	compose845, err := os.ReadFile(filepath.Join(dir1, composeFile))
	if err != nil {
		t.Fatal(err)
	}
	overwrite(t, dir1, composeFile, strings.ReplaceAll(string(compose845), "mysql:8.4", "mysql:8.4.5"))
	// 实例块取组件 Images 声明（非 compose 原文）——组件声明同步换版本
	declRaw, err := os.ReadFile(filepath.Join(dir1, componentsFile))
	if err != nil {
		t.Fatal(err)
	}
	overwrite(t, dir1, componentsFile, strings.ReplaceAll(string(declRaw), "mysql:8.4", "mysql:8.4.5"))
	overwrite(t, dir1, "images/mysql_8.4.5.tar", "fake-tar-mysql-845")
	remanifest(t, dir1)
	writeTestPackage(t, dir2)
	pp1 := parseTestPkg(t, dir1)
	pp2 := parseTestPkg(t, dir2) // mysql:8.4
	hosts := testHosts()
	fresh, err := BuildSessionPlan(pp1, haConfiguredPlacement(1, 2), hosts, nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	pl2 := haConfiguredPlacement(1, 2)
	for k, v := range fresh.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	up, err := BuildSessionPlan(pp2, pl2, hosts, fresh.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]svcChange{}
	for _, c := range up.Changes {
		byService[c.Service] = c
	}
	if byService["app"].Action != "nochange" || byService["web"].Action != "nochange" {
		t.Errorf("app/web 应零触碰：%+v/%+v", byService["app"], byService["web"])
	}
	if byService["mysql-primary"].Action != "update" || byService["mysql-replica"].Action != "update" {
		t.Errorf("双实例应 update（镜像变更）：%+v/%+v", byService["mysql-primary"], byService["mysql-replica"])
	}
	if !contains(byService["mysql-primary"].Reasons, "镜像变更") {
		t.Errorf("变更原因应为镜像变更: %+v", byService["mysql-primary"])
	}
	byKey := map[string]planNode{}
	for _, n := range up.Nodes {
		byKey[n.Key] = n
	}
	if c1, ok := byKey["compose-h1"]; ok && !contains(c1.Deps, "wait-mysql-replica") {
		t.Errorf("先从后主：主库宿主编排应后置于从库就绪: %v", c1.Deps)
	}
}

// TestResourceLimitsInjectAndDiff 资源限额注入 + resHash 精确 diff（改限额只 recreate 该服务）
func TestResourceLimitsInjectAndDiff(t *testing.T) {
	dir := t.TempDir()
	writeTestPackage(t, dir)
	pp := parseTestPkg(t, dir)
	pl := allOnHost(1, false)
	pl.Resources = map[string]ResourceSpec{"app": {Cpus: "2", MemMB: 4096}}
	res, err := BuildSessionPlan(pp, pl, testHosts(), nil, nil, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	var cp ComposePayload
	for _, n := range res.Nodes {
		if n.Key == "compose-h1" {
			_ = json.Unmarshal(n.Payload, &cp)
		}
	}
	blocks := splitServices(cp.Compose)
	if !strings.Contains(blocks["app"], "cpus: \"2\"") || !strings.Contains(blocks["app"], "memory: 4096M") {
		t.Errorf("app 块应注入资源限额:\n%s", blocks["app"])
	}
	if strings.Contains(blocks["mysql"], "deploy:") {
		t.Errorf("未设限额的服务不应注入 deploy 段:\n%s", blocks["mysql"])
	}
	// resHash diff：改 app 限额 → 仅 app update（原因=资源限额变更），mysql/web nochange
	pl2 := allOnHost(1, false)
	for k, v := range res.Vars {
		if pl2.Vars[k] == "" {
			pl2.Vars[k] = v
		}
	}
	pl2.Resources = map[string]ResourceSpec{"app": {Cpus: "4", MemMB: 4096}}
	up, err := BuildSessionPlan(pp, pl2, testHosts(), res.InvRows, nil, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range up.Changes {
		switch c.Service {
		case "app":
			if c.Action != "update" || !contains(c.Reasons, "资源限额变更") {
				t.Errorf("app 应因限额变更 update：%+v", c)
			}
		case "mysql", "web":
			if c.Action != "nochange" {
				t.Errorf("%s 应零触碰：%+v", c.Service, c)
			}
		}
	}
}

// TestInjectResourceLimitsSkipExistingDeploy 块内已有 deploy: 段则跳过注入（防 YAML 重复键）
func TestInjectResourceLimitsSkipExistingDeploy(t *testing.T) {
	blk := "  app:\n    image: x\n    deploy:\n      replicas: 1\n"
	out := injectResourceLimits(blk, ResourceSpec{Cpus: "1"})
	if strings.Count(out, "deploy:") != 1 {
		t.Errorf("已有 deploy 段应跳过注入:\n%s", out)
	}
}
