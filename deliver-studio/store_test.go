// deliver-studio 存储层单测：v1 应用表（逐列混装前后端）→ v2（definition JSON 单元模型）自动迁移。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 建旧版 apps 表并插入一条旧格式应用，重新打开触发迁移，断言 v2 结构
func TestStoreMigratesLegacyApps(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 手工重建旧表结构（模拟 v1 库）
	if _, err := store.db.Exec(`DROP TABLE apps`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE apps(
		id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT UNIQUE NOT NULL, name TEXT DEFAULT '',
		default_version TEXT DEFAULT '1.0.0', port TEXT DEFAULT '8080', health_path TEXT DEFAULT '',
		app_compose_template TEXT DEFAULT '', image_repo TEXT DEFAULT '', jar_dockerfile TEXT DEFAULT '',
		ui_enabled INTEGER DEFAULT 0, ui_image_repo TEXT DEFAULT '', ui_dockerfile TEXT DEFAULT '',
		config_template TEXT DEFAULT '', conf_dir TEXT DEFAULT '/app/conf', conf_env TEXT DEFAULT '{}',
		remark TEXT DEFAULT '', updated_at TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO apps(code, name, default_version, port, health_path, app_compose_template,
		image_repo, jar_dockerfile, ui_enabled, ui_image_repo, ui_dockerfile, config_template, conf_dir, conf_env, remark)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"mse-monolith", "ai-mse 租户单体", "1.0.0", "8080", "/actuator/health",
		"  app:\n    image: {{APP_IMAGE}}\n    ports:\n      - \"8080:8080\"\n  nginx:\n    image: {{UI_IMAGE}}\n",
		"mse/mse-monolith", "FROM eclipse-temurin:21-jre-jammy\nCOPY {{JAR_FILE}} app.jar",
		1, "mse/mse-nginx", "FROM nginx:stable-alpine\nCOPY . /usr/share/nginx/html",
		"mse:\n  tenant: {{TENANT_ID}}\n", "/app/conf", `{"SPRING_CONFIG_ADDITIONAL_LOCATION":"file:/app/conf/"}`, "旧数据")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// 重新打开：migrate() 应把旧表归一迁到 v2
	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	a, err := store2.GetApp("mse-monolith")
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("迁移后应用丢失")
	}
	if len(a.Units) != 2 {
		t.Fatalf("应迁移为 2 个单元（后端+前端），实际 %d", len(a.Units))
	}
	be := a.Units[0]
	if !be.Entrypoint || be.Code != "app" || be.ConfDir != "/app/conf" {
		t.Fatalf("后端单元迁移错误: %+v", be)
	}
	fe := a.Units[1]
	if fe.Code != "nginx" || fe.Role != "frontend" {
		t.Fatalf("前端单元迁移错误: %+v", fe)
	}
	// v2 往返：upsert 后再读保持结构
	if err := store2.UpsertApp(a); err != nil {
		t.Fatal(err)
	}
	a2, err := store2.GetApp("mse-monolith")
	if err != nil || a2 == nil {
		t.Fatal(err)
	}
	if len(a2.Units) != 2 || !a2.Units[0].Entrypoint {
		t.Fatalf("v2 upsert 往返失真: %+v", a2)
	}
}

func TestStoreAppsV2Direct(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cols, err := store.tableColumns("apps")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["definition"] {
		t.Fatal("新库 apps 表应为 v2（definition 列）")
	}
	a := &App{Code: "demo", Name: "演示", DefaultVersion: "1.0.0",
		Units: []Unit{{Code: "api", Role: "backend", Entrypoint: true, Source: "artifact",
			ImageRepo: "mse/demo", ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"}},
		Variables: []Variable{{Name: "TENANT_ID", Label: "租户", When: "install", Source: "input", Value: "mse"}}}
	if err := store.UpsertApp(a); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetApp("demo")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if len(got.Units) != 1 || got.Units[0].Code != "api" || len(got.Variables) != 1 {
		t.Fatalf("读取失真: %+v", got)
	}
	if got.Units[0].Entrypoint != true {
		t.Fatal("入口标记丢失")
	}
	_ = filepath.Join(dir, "studio.db")
}

// 组件 schema 声明（SQL 预置）落库往返：UpsertComponent 带 Schema → GetComponent 读回保留；
// 旧库（无 schema 列）重开自动 ALTER 补列
func TestStoreComponentSchemaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &Component{Code: "mysql", Name: "MySQL 8.4", Category: "database",
		Schema: &ComponentSchema{Dir: "extras", Image: "mysql:8.4", Host: "DB_HOST",
			Port: "DB_PORT", User: "DB_USER", Password: "DB_PASSWORD", Database: "DB_NAME"}}
	if err := store.UpsertComponent(c); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetComponent("mysql")
	if err != nil || got == nil {
		t.Fatalf("读回失败: %v %v", got, err)
	}
	if got.Schema == nil || got.Schema.Dir != "extras" || got.Schema.Host != "DB_HOST" {
		t.Fatalf("schema 声明落库往返丢失: %+v", got.Schema)
	}
	// 无 Schema 组件往返为 nil（不误造）
	plain := &Component{Code: "redis", Name: "Redis 7", Category: "cache"}
	if err := store.UpsertComponent(plain); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.GetComponent("redis"); r.Schema != nil {
		t.Fatalf("无 schema 组件应读回 nil: %+v", r.Schema)
	}
	// 模拟旧库：删列后重开触发 migrateComponents 补列（SQLite 无 DROP COLUMN 前置——重建旧结构表）
	if _, err := store.db.Exec(`DROP TABLE components`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE components(
		id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT UNIQUE NOT NULL, name TEXT DEFAULT '',
		category TEXT DEFAULT 'middleware', default_checked INTEGER DEFAULT 0,
		images TEXT DEFAULT '[]', compose_services TEXT DEFAULT '', files TEXT DEFAULT '[]',
		app_config TEXT DEFAULT '{}', endpoints TEXT DEFAULT '[]', credentials TEXT DEFAULT '[]',
		remark TEXT DEFAULT '', updated_at TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if err := store2.UpsertComponent(c); err != nil {
		t.Fatalf("旧库迁移后应可写 schema: %v", err)
	}
	if r, _ := store2.GetComponent("mysql"); r.Schema == nil || r.Schema.Dir != "extras" {
		t.Fatalf("迁移库 schema 往返不符: %+v", r.Schema)
	}
}

// 打包记录 standalone 列往返 + 旧库（无该列）重开自动 ALTER 补列
func TestStorePackageStandaloneRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.InsertPackage(&PackageRec{Name: "s-kit", Version: "1.0.0", AppCode: "demo", Standalone: true, Status: "running"})
	got, err := store.GetPackage(1)
	if err != nil || got == nil {
		t.Fatalf("读回失败: %v %v", got, err)
	}
	if !got.Standalone {
		t.Fatal("standalone 落库往返丢失")
	}
	list, _ := store.ListPackages()
	if len(list) == 0 || !list[0].Standalone {
		t.Fatal("列表读取 standalone 丢失")
	}
	// 无该列的历史行读回 false（不误标）
	store.InsertPackage(&PackageRec{Name: "plain-kit", Version: "1.0.0", AppCode: "demo", Status: "running"})
	if p, _ := store.GetPackage(2); p == nil || p.Standalone {
		t.Fatal("非单机包应读回 false")
	}
	store.Close()
	// 模拟旧库：重建无 standalone 列的 packages 表，重开触发 migratePackages 补列
	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	store2.db.Exec(`DROP TABLE packages`)
	store2.db.Exec(`CREATE TABLE packages(
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, version TEXT DEFAULT '',
		app_code TEXT DEFAULT '', components TEXT DEFAULT '', status TEXT DEFAULT 'ok',
		stage TEXT DEFAULT '', log TEXT DEFAULT '', zip_path TEXT DEFAULT '',
		created_at TEXT DEFAULT '', finished_at TEXT DEFAULT '')`)
	store2.Close()
	store3, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	if err := store3.InsertPackage(&PackageRec{Name: "old-kit", Standalone: true, Status: "running"}); err != nil {
		t.Fatalf("旧库迁移后应可写 standalone: %v", err)
	}
	if p, _ := store3.GetPackage(1); p == nil || !p.Standalone {
		t.Fatal("迁移库 standalone 往返不符")
	}
}

// 制品同身份去重：类型+文件名+应用+版本相同重复入库只保留最新一条，旧落盘文件一并清除；
// 版本不同或类型不同不算同身份，各自独立保留
func TestStoreMaterialDedupe(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	matDir := filepath.Join(dir, "materials")
	if err := os.MkdirAll(matDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(matDir, name)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("1-old.zip", "old")
	ins := func(path, version, kind string) *Material {
		m := &Material{Kind: kind, FileName: "mse-ui-1.0.0.zip", StoredPath: path,
			Size: 3, AppCode: "mse-monolith", Version: version, Remark: "t"}
		if err := store.InsertMaterial(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := ins(old, "1.0.0", "artifact")
	// 同身份重推：旧记录与旧文件被清，只剩最新
	newPath := write("2-new.zip", "new")
	keep := ins(newPath, "1.0.0", "artifact")
	list, err := store.ListMaterials("artifact")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != keep.ID || list[0].StoredPath != newPath {
		t.Fatalf("同身份应只保留最新一条: %+v", list)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("旧落盘文件应被清除: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("最新落盘文件不应被误删: %v", err)
	}
	// 版本不同：独立保留
	ins(write("3-v2.zip", "v2"), "1.0.1", "artifact")
	if list, _ = store.ListMaterials("artifact"); len(list) != 2 {
		t.Fatalf("不同版本应各自保留，实际 %d 条", len(list))
	}
	// 类型不同：独立保留
	ins(write("4-extra.zip", "extra"), "1.0.0", "extra")
	if list, _ = store.ListMaterials("extra"); len(list) != 1 {
		t.Fatalf("类型应互相独立，extra 实际 %d 条", len(list))
	}
	if list, _ = store.ListMaterials("artifact"); len(list) != 2 {
		t.Fatalf("extra 入库不应影响 artifact，实际 %d 条", len(list))
	}
	_ = first
}

// 制品存量对账：历史重复行（同身份多条）重开库后收敛为最新一条，旧文件清除
func TestStoreReconcileDuplicateMaterials(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	matDir := filepath.Join(dir, "materials")
	if err := os.MkdirAll(matDir, 0755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 3)
	for i, body := range []string{"a", "b", "c"} {
		p := filepath.Join(matDir, string(rune('1'+i))+"-dup.zip")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
		// 绕过 InsertMaterial 直插，模拟旧版本累积的重复行
		if _, err := store.db.Exec(`INSERT INTO materials(kind, file_name, stored_path, size, app_code, version, remark, created_at)
			VALUES('artifact','mse-ui-1.0.0.zip',?,3,'mse-monolith','1.0.0','legacy',?)`, p, string(rune('1'+i))); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	n, err := store2.ReconcileDuplicateMaterials()
	if err != nil || n != 2 {
		t.Fatalf("存量对账应清除 2 条，实际 n=%d err=%v", n, err)
	}
	list, err := store2.ListMaterials("artifact")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].StoredPath != paths[2] {
		t.Fatalf("存量重复应收敛为最新一条: %+v", list)
	}
	for _, p := range paths[:2] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("存量旧文件应被清除: %s %v", p, err)
		}
	}
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("保留文件不应被误删: %v", err)
	}
	// 幂等：再跑一遍零清除
	if n, err := store2.ReconcileDuplicateMaterials(); err != nil || n != 0 {
		t.Fatalf("对账应幂等，实际 n=%d err=%v", n, err)
	}
}
