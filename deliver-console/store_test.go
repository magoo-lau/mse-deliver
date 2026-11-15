package main

// packages 表 upsert 单测：同 app+version 重复上传=覆盖更新——不再撞 UNIQUE(app, version) 报
// 「登记失败: constraint failed」，且行数不涨、id 跨覆盖稳定（历史会话 package_id 引用依赖此点）、
// 元数据/文件清单/包指纹随新包刷新。

import (
	"testing"
)

func TestUpsertPackageSameAppVersion(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := &packageRow{App: "app-a", Version: "1.0.0", Dir: "/pkgs/app-a-1.0.0",
		Decl: `{"name":"app-a","version":"1.0.0"}`, PkgSha: "sha-old",
		Files: []pkgFile{{Path: "compose.yaml", Size: 10, SHA256: "f-old"}}}
	if err := st.UpsertPackage(first); err != nil {
		t.Fatalf("首次登记: %v", err)
	}
	second := &packageRow{App: "app-a", Version: "1.0.0", Dir: "/pkgs/app-a-1.0.0",
		Decl: `{"name":"app-a","version":"1.0.0","rev":2}`, PkgSha: "sha-new",
		Files: []pkgFile{{Path: "compose.yaml", Size: 20, SHA256: "f-new"},
			{Path: "images/app.tar", Size: 30, SHA256: "f-img"}}}
	if err := st.UpsertPackage(second); err != nil {
		t.Fatalf("同版本覆盖登记: %v", err)
	}

	pkgs, err := st.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("覆盖后应仅 1 行，实际 %d", len(pkgs))
	}
	got := pkgs[0]
	if got.ID != first.ID || got.ID != second.ID {
		t.Fatalf("id 应跨覆盖稳定（首次=%d 覆盖=%d 库内=%d）", first.ID, second.ID, got.ID)
	}
	if got.PkgSha != "sha-new" || got.Decl != second.Decl || len(got.Files) != 2 || got.Files[1].SHA256 != "f-img" {
		t.Fatalf("覆盖后元数据应随新包刷新：pkgSha=%s decl=%s files=%+v", got.PkgSha, got.Decl, got.Files)
	}

	byAV, err := st.PackageByAppVersion("app-a", "1.0.0")
	if err != nil || byAV.ID != got.ID {
		t.Fatalf("PackageByAppVersion 回查失败: row=%+v err=%v", byAV, err)
	}
	if _, err := st.PackageByAppVersion("app-a", "9.9.9"); err != errNotFound {
		t.Fatalf("不存在的 app+version 应返回 errNotFound，实际 %v", err)
	}
}
