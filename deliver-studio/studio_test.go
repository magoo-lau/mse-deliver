package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	app := &App{Code: "demo-app", Name: "演示应用", DefaultVersion: "1.0.0",
		Units: []Unit{
			{Code: "api", Name: "后端", Role: "backend", Entrypoint: true, Source: "artifact",
				ImageRepo: "demo/app", Port: "8080", HealthPath: "/actuator/health",
				ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n", ConfEnv: map[string]string{"K": "V"}},
			{Code: "web", Name: "前端", Role: "frontend", Source: "artifact", ImageRepo: "demo/ui",
				ComposeTemplate: "  web:\n    image: {{UNIT_IMAGE}}\n"},
		},
		Variables: []Variable{{Name: "TENANT_ID", Label: "租户", When: "install", Source: "input", Value: "mse"}}}
	if err := s.UpsertApp(app); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	// upsert = 整行覆盖语义：更新须携带完整对象
	app2 := *app
	app2.Name = "演示应用2"
	if err := s.UpsertApp(&app2); err != nil {
		t.Fatalf("UpsertApp(更新): %v", err)
	}
	got, _ := s.ListApps()
	if len(got) != 1 || got[0].Name != "演示应用2" || len(got[0].Units) != 2 || !got[0].Units[0].Entrypoint {
		t.Fatalf("应用 upsert 后应按编码覆盖: %+v", got)
	}

	comp := &Component{Code: "mysql", Name: "MySQL", Category: "database", DefaultChecked: true,
		Images: []string{"mysql:8.4"}, ComposeServices: "  mysql:\n    image: mysql:8.4\n",
		AppConfig:   map[string]string{"spring": "  datasource:\n    url: {{DB_URL}}\n"},
		Endpoints:   []Endpoint{{Placeholder: "DB_URL", Service: "mysql", Port: "3306", Scheme: "jdbc:mysql"}},
		Credentials: []Credential{{Placeholder: "MYSQL_ROOT_PASSWORD", Env: "MYSQL_ROOT_PASSWORD", Generate: "ask"}}}
	if err := s.UpsertComponent(comp); err != nil {
		t.Fatalf("UpsertComponent: %v", err)
	}
	c, err := s.GetComponent("mysql")
	if err != nil || c == nil {
		t.Fatalf("GetComponent: %v %v", c, err)
	}
	if len(c.Images) != 1 || c.Images[0] != "mysql:8.4" || !c.DefaultChecked || len(c.Endpoints) != 1 {
		t.Fatalf("组件 JSON 字段落取不符: %+v", c)
	}

	m := &Material{Kind: "jar", FileName: "app.jar", StoredPath: filepath.Join(dir, "app.jar"), Size: 10}
	if err := s.InsertMaterial(m); err != nil {
		t.Fatalf("InsertMaterial: %v", err)
	}
	list, _ := s.ListMaterials("")
	if len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("物料落库不符")
	}
	s.PutSetting("k", "v")
	if s.GetSetting("k", "") != "v" {
		t.Fatalf("settings 读写不符")
	}
}

func TestValidateComponent(t *testing.T) {
	ok := &Component{Code: "x", Images: []string{"mysql:8.4"}, ComposeServices: "  mysql:\n    image: mysql:8.4\n"}
	if err := validateComponent(ok); err != nil {
		t.Fatalf("合法组件被拒: %v", err)
	}
	badIndent := &Component{Code: "x", ComposeServices: " mysql:\n   image: x\n"}
	if err := validateComponent(badIndent); err == nil {
		t.Fatal("奇数缩进应被拒")
	}
	badRef := &Component{Code: "x", Images: []string{"a b:c:d"}}
	if err := validateComponent(badRef); err == nil {
		t.Fatal("非法镜像引用应被拒")
	}
	empty := &Component{Code: "x"}
	if err := validateComponent(empty); err == nil {
		t.Fatal("空服务块+空镜像应被拒")
	}
}

func TestNames(t *testing.T) {
	if safeImageName("mse/mse-monolith:1.0.0") != "mse_mse-monolith_1.0.0" {
		t.Fatal("safeImageName")
	}
	if got := composeProjectName("My App 试用"); got != "my-app" {
		t.Fatalf("composeProjectName: %s", got)
	}
	if composeProjectName("---") != "" {
		t.Fatal("全符号包名应归零（引擎侧拒绝）")
	}
}

// TestEmbeddedPreset 内嵌 ai-mse 示例预设可解析且全部组件过校验
func TestEmbeddedPreset(t *testing.T) {
	files, err := embeddedPresets()
	if err != nil {
		t.Fatalf("embeddedPresets: %v", err)
	}
	data, ok := files["ai-mse"]
	if !ok {
		t.Fatalf("内嵌预设应含 ai-mse，实际: %v", files)
	}
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("ai-mse 预设解析失败: %v", err)
	}
	if len(p.Apps) == 0 || len(p.Components) == 0 {
		t.Fatalf("ai-mse 预设应有应用与组件: apps=%d components=%d", len(p.Apps), len(p.Components))
	}
	for i := range p.Components {
		if err := validateComponent(&p.Components[i]); err != nil {
			t.Fatalf("ai-mse 组件 %s 校验失败: %v", p.Components[i].Code, err)
		}
	}
}

// TestApplyPresetIdempotent 预设导入幂等 + replace 语义
func TestApplyPresetIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()
	srv := &Server{store: st, root: dir}

	files, _ := embeddedPresets()
	var p Preset
	if err := json.Unmarshal(files["ai-mse"], &p); err != nil {
		t.Fatalf("解析: %v", err)
	}
	apps1, comps1, err := srv.applyPreset(&p, false)
	if err != nil {
		t.Fatalf("首次导入: %v", err)
	}
	apps2, comps2, err := srv.applyPreset(&p, false)
	if err != nil {
		t.Fatalf("二次导入: %v", err)
	}
	gotApps, _ := st.ListApps()
	gotComps, _ := st.ListComponents()
	if apps1 != apps2 || comps1 != comps2 || len(gotApps) != apps1 || len(gotComps) != comps1 {
		t.Fatalf("重复导入应幂等: %d/%d → %d/%d", apps1, comps1, len(gotApps), len(gotComps))
	}

	// replace=1：先清空再导入
	seed := &Component{Code: "legacy", Images: []string{"x/y:1"}, ComposeServices: "  x:\n    image: x/y:1\n"}
	if err := st.UpsertComponent(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := srv.applyPreset(&p, true); err != nil {
		t.Fatalf("replace 导入: %v", err)
	}
	gotComps, _ = st.ListComponents()
	for _, c := range gotComps {
		if c.Code == "legacy" {
			t.Fatal("replace=1 应清空既有组件")
		}
	}
	if len(gotComps) != comps1 {
		t.Fatalf("replace 后组件数应等于预设: %d != %d", len(gotComps), comps1)
	}
}

// TestExportImportRoundtrip 导出（声明+物料+镜像 zip）→ 导入自动识别回灌
func TestExportImportRoundtrip(t *testing.T) {
	srcDir := t.TempDir()
	src, err := OpenStore(filepath.Join(srcDir, "data"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer src.Close()

	comp := &Component{Code: "mysql", Images: []string{"mysql:8.4"}, ComposeServices: "  mysql:\n    image: mysql:8.4\n"}
	if err := src.UpsertComponent(comp); err != nil {
		t.Fatalf("seed component: %v", err)
	}
	src.PutSetting("defaultVersion", "9.9.9")

	// 造一个物料文件 + 一个镜像 tar，物理文件落在 root 下（导出 zip 会带上）
	matDir := filepath.Join(srcDir, materialsDir)
	imgDir := filepath.Join(srcDir, imageStoreDir)
	if err := os.MkdirAll(matDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(imgDir, 0755); err != nil {
		t.Fatal(err)
	}
	matFile := filepath.Join(matDir, "123-app.jar")
	if err := os.WriteFile(matFile, []byte("jar-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	imgFile := filepath.Join(imgDir, "mysql_8.4.tar")
	if err := os.WriteFile(imgFile, []byte("tar-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := src.InsertMaterial(&Material{Kind: "jar", FileName: "app.jar", StoredPath: matFile, Size: 9, AppCode: "demo", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := src.InsertImage("mysql_8.4.tar", "mysql:8.4", 9); err != nil {
		t.Fatal(err)
	}

	p := Preset{Version: "1", Components: []Component{*comp},
		Settings:  map[string]string{"defaultVersion": "9.9.9"},
		Materials: []Material{{Kind: "jar", FileName: "app.jar", StoredPath: materialsDir + "/123-app.jar", Size: 9, AppCode: "demo", Version: "1.0.0"}},
		Images:    []ImageItem{{FileName: "mysql_8.4.tar", ImageRef: "mysql:8.4", Size: 9}}}
	zipPath := filepath.Join(srcDir, "export-test.zip")
	if err := writeExportZip(&p, srcDir, zipPath); err != nil {
		t.Fatalf("writeExportZip: %v", err)
	}

	// 新库导入（走 handleImport 的同一解析路径）
	dstDir := t.TempDir()
	dst, err := OpenStore(filepath.Join(dstDir, "data"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer dst.Close()
	dstSrv := &Server{store: dst, root: dstDir}
	res, err := dstSrv.importFile(zipPath, "export-test.zip", false)
	if err != nil {
		t.Fatalf("importFile: %v", err)
	}
	if res["components"] != 1 || res["materials"] != 1 || res["images"] != 1 || res["skipped"] != 0 {
		t.Fatalf("导入计数不符: %v", res)
	}
	if got := dst.GetSetting("defaultVersion", ""); got != "9.9.9" {
		t.Fatalf("settings 应随包导入: %q", got)
	}
	gotC, _ := dst.GetComponent("mysql")
	if gotC == nil || len(gotC.Images) != 1 {
		t.Fatalf("组件应回灌: %+v", gotC)
	}
	mats, _ := dst.ListMaterials("")
	if len(mats) != 1 || mats[0].AppCode != "demo" || !fileExists(mats[0].StoredPath) {
		t.Fatalf("物料应回拷并带元数据: %+v", mats)
	}
	imgs, _ := dst.ListImages()
	if len(imgs) != 1 || imgs[0].ImageRef != "mysql:8.4" || !fileExists(filepath.Join(dstDir, imageStoreDir, imgs[0].FileName)) {
		t.Fatalf("镜像应回拷并登记: %+v", imgs)
	}
}
