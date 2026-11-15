// 交付附件（attachments format:1）与最终交付包重打包单测：附件包全量校验、
// 重打包产出（条目/manifest 全覆盖/审计段/执行位）、extras 冲突拦截、sha 幂等、StartDeliver 前置校验。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bytesSha256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// buildDeliveryBundle 构造附件包 zip；mutate 可改文件集与描述符（decl.Files 非空时跳过自动哈希清单，
// 供坏哈希/未登记文件用例手工构造）
func buildDeliveryBundle(t *testing.T, path string, mutate func(files map[string][]byte, decl *AttachmentsDecl)) {
	t.Helper()
	files := map[string][]byte{
		"delivery/vars.env":           []byte("TENANT_ID=1\nTENANT_CODE=demo\nTENANT_NAME=演示\n"),
		"delivery/deploy-package.zip": []byte("PK\x03\x04fake-signed-inner-payload"),
		"extras/schema.sql":           []byte("SET NAMES utf8mb4;\nCREATE TABLE IF NOT EXISTS t1(id INT);\n"),
	}
	decl := &AttachmentsDecl{Format: 1, Tenant: AttachTenant{ID: "1", Code: "demo", Name: "演示租户"},
		LicenseID: "L-001", PackageType: "FULL", Modules: []string{"oa"},
		// 真实签发形态：perBizSchemaVersion 为嵌套对象（biz → 账本版本）——守卫 Go 端按混合类型解析
		Requires: map[string]any{"platformVersion": "0.1.0",
			"perBizSchemaVersion": map[string]any{"oa": "20261001.02", "system": "20260901.01"}}}
	if mutate != nil {
		mutate(files, decl)
	}
	if decl.Files == nil {
		decl.Files = map[string]string{}
		for p, b := range files {
			decl.Files[p] = bytesSha256(b)
		}
	}
	declJSON, err := json.Marshal(decl)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	wf := func(name string, data []byte) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	wf("attachments.json", declJSON)
	for _, p := range sortedKeys2(files) {
		wf(p, files[p])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func sortedKeys2(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestParseDeliveryZip(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.zip")
	buildDeliveryBundle(t, good, nil)
	if _, err := parseDeliveryZip(good); err != nil {
		t.Fatalf("合法附件包应通过: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(files map[string][]byte, decl *AttachmentsDecl)
		want   string
	}{
		{"坏哈希", func(files map[string][]byte, decl *AttachmentsDecl) {
			decl.Files = map[string]string{}
			for p := range files {
				decl.Files[p] = strings.Repeat("0", 64)
			}
		}, "哈希与清单不符"},
		{"缺必备件", func(files map[string][]byte, decl *AttachmentsDecl) {
			delete(files, "delivery/vars.env")
		}, "缺少必备件"},
		{"路径越权", func(files map[string][]byte, decl *AttachmentsDecl) {
			files["components.json"] = []byte("{}")
			decl.Files = map[string]string{}
			for p, b := range files {
				decl.Files[p] = bytesSha256(b)
			}
		}, "越权"},
		{"未登记文件", func(files map[string][]byte, decl *AttachmentsDecl) {
			files["delivery/extra.txt"] = []byte("x")
			decl.Files = map[string]string{}
			for p, b := range files {
				if p == "delivery/extra.txt" {
					continue
				}
				decl.Files[p] = bytesSha256(b)
			}
		}, "未登记"},
		{"包类型非法", func(files map[string][]byte, decl *AttachmentsDecl) {
			decl.PackageType = "PATCH"
		}, "packageType"},
		{"租户编码缺失", func(files map[string][]byte, decl *AttachmentsDecl) {
			decl.Tenant.Code = ""
		}, "tenant.code"},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".zip")
		buildDeliveryBundle(t, p, c.mutate)
		_, err := parseDeliveryZip(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 期望错误含 %q，实际: %v", c.name, c.want, err)
		}
	}

	// 非 zip 内容
	badZip := filepath.Join(dir, "not-a-zip.zip")
	if err := os.WriteFile(badZip, []byte("plain text"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDeliveryZip(badZip); err == nil || !strings.Contains(err.Error(), "zip") {
		t.Errorf("非 zip 附件应被拒绝: %v", err)
	}

	// 最小自造包（仅必备两件、无 extras）可通过——文档「手工自造测试附件样例」口径
	minimal := filepath.Join(dir, "minimal.zip")
	buildDeliveryBundle(t, minimal, func(files map[string][]byte, decl *AttachmentsDecl) {
		delete(files, "extras/schema.sql")
	})
	if _, err := parseDeliveryZip(minimal); err != nil {
		t.Errorf("最小自造附件（vars.env + deploy-package.zip）应通过: %v", err)
	}
}

// deliverFixture 免 docker 产出一个小套件（image_tar 单元 + 零镜像组件 + standalone install.sh）
func deliverFixture(t *testing.T) (*Store, *Engine, *PackageRec, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "mse-api.tar")
	if err := os.WriteFile(tarPath, []byte("fake-tar"), 0644); err != nil {
		t.Fatal(err)
	}
	store.InsertMaterial(&Material{Kind: "image_tar", FileName: "mse-api.tar", StoredPath: tarPath, Size: 8})
	mats, _ := store.ListMaterials("image_tar")
	if len(mats) != 1 {
		t.Fatal("镜像包物料未落库")
	}
	app := &App{Code: "dapp", DefaultVersion: "1.0.0",
		Units: []Unit{{Code: "api", Role: "backend", Entrypoint: true, Source: "image_tar",
			Port: "8080", HealthPath: "/actuator/health",
			ComposeTemplate: "  api:\n    image: {{UNIT_IMAGE}}\n"}},
		// 与附件 vars.env 三键配平（TENANT_ID/CODE/NAME）——附件链对应用的要求：
		// 声明同名安装变量，console 预填命中（缺声明的键在重打包日志出告警，非阻断）
		Variables: []Variable{
			{Name: "TENANT_ID", When: "install", Source: "input", Value: "mse"},
			{Name: "TENANT_CODE", When: "install", Source: "input", Value: ""},
			{Name: "TENANT_NAME", When: "install", Source: "input", Value: ""},
		}}
	if err := store.UpsertApp(app); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(dir, store)
	req := &AssembleRequest{Name: "d-kit", Version: "1.0.0", AppCode: "dapp", Standalone: true,
		Units: []UnitPick{{Code: "api", Source: "image_tar", MaterialID: mats[0].ID}}}
	plan, err := engine.buildPlan(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.assemble(req, plan, func(string, ...any) {}); err != nil {
		t.Fatalf("套件装配失败: %v", err)
	}
	zipPath := filepath.Join(dir, packagesDir, "d-kit-1.0.0.zip")
	if !fileExists(zipPath) {
		t.Fatal("套件 zip 未产出")
	}
	kitSha, err := kitPkgShaFromZip(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	kit := &PackageRec{Name: "d-kit", Version: "1.0.0", AppCode: "dapp", Kind: "kit",
		Status: "ok", ZipPath: zipPath, SourcePkgSha: kitSha}
	return store, engine, kit, dir
}

// 重打包端到端：附件条目落包、attachments.json 溯源、INSTALL.md、manifest 全覆盖 + delivery 审计段、
// install.sh 执行位保留、套件内容原样
func TestDeliverAssemble(t *testing.T) {
	store, engine, kit, dir := deliverFixture(t)
	defer store.Close()
	bundle := filepath.Join(dir, "bundle.zip")
	buildDeliveryBundle(t, bundle, nil)
	sum, err := fileSha256(bundle)
	if err != nil {
		t.Fatal(err)
	}
	dl := &DeliveryRec{TenantCode: "demo", TenantName: "演示租户", LicenseID: "L-001",
		PackageType: "FULL", Modules: "oa",
		RequiresJSON: `{"platformVersion":"0.1.0","perBizSchemaVersion":{"oa":"20261001.02","system":"20260901.01"}}`,
		StoredPath:   bundle, Sha256: sum}
	if err := engine.deliverAssemble(kit, dl, kit.SourcePkgSha, func(string, ...any) {}); err != nil {
		t.Fatalf("重打包失败: %v", err)
	}
	finalZip := filepath.Join(dir, packagesDir, "d-kit-1.0.0-demo.zip")
	zr, err := zip.OpenReader(finalZip)
	if err != nil {
		t.Fatalf("最终交付包未产出: %v", err)
	}
	defer zr.Close()
	entries := map[string][]byte{}
	modes := map[string]os.FileMode{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, _ := f.Open()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(rc)
		rc.Close()
		entries[filepath.ToSlash(f.Name)] = buf.Bytes()
		modes[filepath.ToSlash(f.Name)] = f.Mode().Perm()
	}
	for _, want := range []string{"docker-compose.yml", "components.json", "manifest.json",
		"install.sh", "INSTALL.md", "delivery/vars.env", "delivery/deploy-package.zip",
		"delivery/attachments.json", "extras/schema.sql"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("最终交付包缺条目: %s", want)
		}
	}
	if modes["install.sh"] != 0755 {
		t.Errorf("install.sh 执行位应为 0755，实际 %v", modes["install.sh"])
	}
	// 溯源副本 = 描述符
	var decl AttachmentsDecl
	if err := json.Unmarshal(entries["delivery/attachments.json"], &decl); err != nil {
		t.Fatalf("attachments.json 解析失败: %v", err)
	}
	if decl.Tenant.Code != "demo" || decl.LicenseID != "L-001" || decl.PackageType != "FULL" {
		t.Errorf("附件描述符字段不符: %+v", decl)
	}
	// manifest：files 全覆盖（manifest.json 自身除外）且哈希一致 + delivery 审计段
	var m struct {
		Name     string            `json:"name"`
		Version  string            `json:"version"`
		Delivery map[string]any    `json:"delivery"`
		Files    map[string]string `json:"files"`
	}
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "d-kit" || m.Version != "1.0.0" {
		t.Errorf("manifest name/version 不符: %s %s", m.Name, m.Version)
	}
	if len(m.Files) != len(entries)-1 { // 除 manifest.json 自身
		t.Errorf("manifest files 应覆盖全部条目（期望 %d，实际 %d）", len(entries)-1, len(m.Files))
	}
	for name, want := range m.Files {
		got, ok := entries[name]
		if !ok {
			t.Errorf("manifest 登记的文件在包内不存在: %s", name)
			continue
		}
		if bytesSha256(got) != strings.ToLower(want) {
			t.Errorf("manifest 哈希不符: %s", name)
		}
	}
	if m.Delivery == nil || m.Delivery["tenantCode"] != "demo" ||
		m.Delivery["sourcePkgSha256"] != kit.SourcePkgSha || m.Delivery["attachmentsSha256"] != sum {
		t.Errorf("manifest delivery 审计段不符: %v", m.Delivery)
	}
	// requires 审计段原样回填：嵌套 perBizSchemaVersion 不丢（曾因二次反序列化按扁平 string 静默丢失）
	req, _ := m.Delivery["requires"].(map[string]any)
	perBiz, _ := req["perBizSchemaVersion"].(map[string]any)
	if perBiz == nil || perBiz["oa"] != "20261001.02" {
		t.Errorf("manifest delivery.requires 应原样保留嵌套 perBizSchemaVersion: %v", req)
	}
	// INSTALL.md 含租户与 console 主线
	if !strings.Contains(string(entries["INSTALL.md"]), "demo") ||
		!strings.Contains(string(entries["INSTALL.md"]), "deliver-console") {
		t.Error("INSTALL.md 应含租户编码与 console 安装主线")
	}
}

// 附件 extras/schema.sql 与套件既有同名文件冲突 → 硬拦
func TestDeliverAssembleExtrasConflict(t *testing.T) {
	store, engine, kit, dir := deliverFixture(t)
	defer store.Close()
	// 套件自带 extras/schema.sql（打包时选了同名附加文件）
	kitDir := filepath.Join(dir, packagesDir, "d-kit-1.0.0")
	if err := os.MkdirAll(filepath.Join(kitDir, "extras"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "extras", "schema.sql"), []byte("-- kit sql"), 0644); err != nil {
		t.Fatal(err)
	}
	// 重建套件 zip（assemble 留有目录，直接重压）
	if err := zipDir(kitDir, kit.ZipPath); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.zip")
	buildDeliveryBundle(t, bundle, nil)
	dl := &DeliveryRec{TenantCode: "demo", PackageType: "FULL", StoredPath: bundle, Sha256: "x"}
	err := engine.deliverAssemble(kit, dl, "sha", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("extras 同名冲突应硬拦: %v", err)
	}
}

// 附件变量告警（非阻断）：附件 vars.env 键未在套件安装变量声明 → 重打包日志出提示且出包成功；
// 全命中 → 无提示
func TestAttachmentVarsNotes(t *testing.T) {
	store, engine, kit, dir := deliverFixture(t)
	defer store.Close()
	matched := filepath.Join(dir, "matched.zip")
	buildDeliveryBundle(t, matched, nil)

	// 全命中（夹具已配平三键）→ 无告警
	if notes := attachmentVarsNotes(kit.ZipPath, matched); len(notes) != 0 {
		t.Fatalf("全命中不应有告警: %v", notes)
	}
	// 附件多带未声明键 → 列名告警
	extra := filepath.Join(dir, "extra.zip")
	buildDeliveryBundle(t, extra, func(files map[string][]byte, decl *AttachmentsDecl) {
		files["delivery/vars.env"] = append(append([]byte{}, files["delivery/vars.env"]...),
			[]byte("LICENSE_PUBLIC_KEY=demo-key\nEXTRA_FLAG=1\n")...)
	})
	notes := attachmentVarsNotes(kit.ZipPath, extra)
	if len(notes) != 1 || !strings.Contains(notes[0], "LICENSE_PUBLIC_KEY") || !strings.Contains(notes[0], "EXTRA_FLAG") {
		t.Fatalf("未声明键应列名告警: %v", notes)
	}

	// 端到端：告警进任务日志、出包仍成功（手动输入通道保留）
	sum, err := fileSha256(extra)
	if err != nil {
		t.Fatal(err)
	}
	dl := &DeliveryRec{TenantCode: "demo", PackageType: "FULL", StoredPath: extra, Sha256: sum}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if err := engine.deliverAssemble(kit, dl, kit.SourcePkgSha, logf); err != nil {
		t.Fatalf("未声明附件变量不应阻断重打包: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "未在套件安装变量中声明") || !strings.Contains(joined, "手动输入") {
		t.Errorf("任务日志应含告警指引: %s", joined)
	}
	if !fileExists(filepath.Join(dir, packagesDir, "d-kit-1.0.0-demo.zip")) {
		t.Error("告警场景最终交付包仍应产出")
	}
}

// StartDeliver 前置校验：final 不可再叠加、未完成套件拦截、附件不存在拦截、附件包损坏拦截
func TestStartDeliverValidations(t *testing.T) {
	store, engine, kit, dir := deliverFixture(t)
	defer store.Close()
	if err := store.InsertPackage(kit); err != nil {
		t.Fatal(err)
	}
	// InsertPackage 不落 zip_path（正常链路由 FinishPackage 收尾写），测试补齐
	store.FinishPackage(kit.ID, "ok", "完成", "", kit.ZipPath)
	bundle := filepath.Join(dir, "bundle.zip")
	buildDeliveryBundle(t, bundle, nil)
	dlRec, err := engine.saveDeliveryZip(bundle, "bundle.zip", "push")
	if err != nil {
		t.Fatal(err)
	}
	if dlRec.TenantCode != "demo" || dlRec.Origin != "push" {
		t.Errorf("附件入库字段不符: %+v", dlRec)
	}
	// sha 幂等：同包重传复用记录
	again, err := engine.saveDeliveryZip(bundle, "bundle-again.zip", "upload")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != dlRec.ID {
		t.Errorf("同 sha 附件应复用记录（%d vs %d）", again.ID, dlRec.ID)
	}
	if list, _ := store.ListDeliveries(); len(list) != 1 {
		t.Errorf("同 sha 附件不应堆积（当前 %d 条）", len(list))
	}

	// 正常链路（异步任务提交成功即验证；任务体正确性由 TestDeliverAssemble 覆盖）
	rec, err := engine.StartDeliver(kit.ID, dlRec.ID)
	if err != nil {
		t.Fatalf("合法重打包应可提交: %v", err)
	}
	if rec.Kind != "final" || rec.DeliveryID != dlRec.ID || rec.SourcePkgSha != kit.SourcePkgSha {
		t.Errorf("重打包记录审计字段不符: %+v", rec)
	}
	// 等异步任务收尾（避免后续 store.Close 与临时目录清理竞态）
	for i := 0; i < 100; i++ {
		p, _ := store.GetPackage(rec.ID)
		if p != nil && p.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalRec, _ := store.GetPackage(rec.ID)
	finalRec.Kind = "final"
	if _, err := engine.StartDeliver(finalRec.ID, dlRec.ID); err == nil || !strings.Contains(err.Error(), "不能再叠加") {
		t.Errorf("final 记录再叠加应拦截: %v", err)
	}
	running := &PackageRec{Name: "r", Version: "1", Kind: "kit", Status: "running", ZipPath: kit.ZipPath}
	if err := store.InsertPackage(running); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.StartDeliver(running.ID, dlRec.ID); err == nil || !strings.Contains(err.Error(), "未完成打包") {
		t.Errorf("未完成套件应拦截: %v", err)
	}
	if _, err := engine.StartDeliver(kit.ID, 999); err == nil || !strings.Contains(err.Error(), "附件不存在") {
		t.Errorf("附件不存在应拦截: %v", err)
	}
	// 损坏附件包入库被拒且文件清理
	badBundle := filepath.Join(dir, "bad.zip")
	if err := os.WriteFile(badBundle, []byte("not zip"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.saveDeliveryZip(badBundle, "bad.zip", "upload"); err == nil || !strings.Contains(err.Error(), "zip") {
		t.Errorf("损坏附件包应拒绝入库: %v", err)
	}
	if fileExists(badBundle) {
		t.Error("被拒附件的落盘文件应清理")
	}
}

// writeProbeZip 通用小 zip 构造（looksLikeDeliveryZip 用例：任意条目，无描述符约定）
func writeProbeZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, name := range sortedKeys2(entries) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// looksLikeDeliveryZip 双闸探针：根级 attachments.json 与签发命名（新旧两代、大小写不敏感）两路识别，
// 普通散文件/普通 zip 不误伤
func TestLooksLikeDeliveryZip(t *testing.T) {
	dir := t.TempDir()

	// 文件名无特征但根级含 attachments.json → 识别（防改名绕过）
	plain := filepath.Join(dir, "plain-bundle.zip")
	writeProbeZip(t, plain, map[string][]byte{"attachments.json": []byte(`{"format":1}`)})
	if !looksLikeDeliveryZip(plain, "plain-bundle.zip") {
		t.Error("根级 attachments.json 应识别为交付附件包")
	}
	// 签发命名（tenant- 旧代 / attachments- 新代 / 大小写混合）→ 无需开包即拦
	for _, name := range []string{"tenant-demo-FULL-202610261959-7c4d42.zip", "Attachments-demo-FULL-x.zip"} {
		p := filepath.Join(dir, name)
		writeProbeZip(t, p, map[string][]byte{"whatever.txt": []byte("x")})
		if !looksLikeDeliveryZip(p, name) {
			t.Errorf("签发命名 %s 应识别", name)
		}
	}
	// 普通散文件（非 zip）与普通 zip 不误伤
	schema := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schema, []byte("SELECT 1;"), 0644); err != nil {
		t.Fatal(err)
	}
	if looksLikeDeliveryZip(schema, "schema.sql") {
		t.Error("非 zip 散文件不应误判")
	}
	normal := filepath.Join(dir, "docs-pack.zip")
	writeProbeZip(t, normal, map[string][]byte{"docs/readme.md": []byte("hi")})
	if looksLikeDeliveryZip(normal, "docs-pack.zip") {
		t.Error("普通 zip 不应误判")
	}
}
