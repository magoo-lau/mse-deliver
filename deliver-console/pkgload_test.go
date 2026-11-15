package main

// savePackageZip 单测：并发解包 + 流式哈希合一后的产物正确性——文件清单/包指纹/篡改拦截。
// （e2e 走 HTTP 全链路但包体极小，这里补大文件与校验语义的贴身覆盖）

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildPkgZip 构造 format:3 测试 zip；corrupt 传文件名时该文件写入与 manifest 不符的内容
func buildPkgZip(t *testing.T, path string, corrupt string) map[string]int64 {
	t.Helper()
	decl := ComponentsDecl{Format: declFormat, Name: "load-app", Version: "1.0.0",
		Entry: EntryDecl{Unit: "app", Service: "app", Port: "18098", HealthPath: "/health"},
		Units: []UnitDecl{{Code: "app", Services: []string{"app"}, Port: "18098", HealthPath: "/health"}}}
	compose := "name: load-app\nservices:\n  app:\n    image: app:1.0.0\n"
	big := make([]byte, 8<<20) // 模拟镜像 tar
	_, _ = rand.Read(big)
	files := map[string][]byte{
		composeFile:      []byte(compose),
		componentsFile:   mustJSON(decl),
		"images/app.tar": big,
		"delivery/x.env": []byte("K=V\n"),
	}
	manifest := map[string]string{}
	for rel, content := range files {
		sum := sha256.Sum256(content)
		manifest[rel] = hex.EncodeToString(sum[:])
	}
	if corrupt != "" {
		files[corrupt] = []byte("corrupted!") // 清单哈希仍按原始内容登记
	}
	mj, _ := json.Marshal(map[string]any{"files": manifest})

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	sizes := map[string]int64{}
	for rel, content := range files {
		w, err := zw.Create(rel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
		sizes[rel] = int64(len(content))
	}
	w, err := zw.Create(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(mj); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return sizes
}

// TestSavePackageZipFileIndexAndFingerprint 哈希合一后的产物：文件清单（rel/size/sha 齐备、
// manifest.json 不入清单）、包指纹=sha256(manifest.json)、parsePackageDir 瘦身不再现场产出清单
func TestSavePackageZipFileIndexAndFingerprint(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "in.zip")
	sizes := buildPkgZip(t, zipPath, "")

	pp, err := savePackageZip(root, zipPath)
	if err != nil {
		t.Fatalf("savePackageZip: %v", err)
	}
	// 文件清单：全部包内文件（manifest.json 除外），大小与哈希齐备
	if len(pp.Row.Files) != len(sizes) {
		t.Fatalf("文件清单数量 = %d，期望 %d（%v）", len(pp.Row.Files), len(sizes), pp.Row.Files)
	}
	for _, f := range pp.Row.Files {
		if want, ok := sizes[f.Path]; !ok || f.Size != want {
			t.Errorf("清单 %s size=%d 期望 %d", f.Path, f.Size, want)
		}
		if len(f.SHA256) != 64 || strings.HasSuffix(f.Path, manifestFile) {
			t.Errorf("清单 %s 哈希异常：%q", f.Path, f.SHA256)
		}
	}
	// 包指纹 = sha256(manifest.json)
	mani, _ := os.ReadFile(filepath.Join(pp.PkgDir, manifestFile))
	sum := sha256.Sum256(mani)
	if pp.Row.PkgSha != hex.EncodeToString(sum[:]) {
		t.Fatalf("包指纹不符：%s", pp.Row.PkgSha)
	}
	// parsePackageDir 已瘦身：不再现场产出文件清单（由 DB 行回填）
	pp2, err := parsePackageDir(pp.PkgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pp2.Row.Files) != 0 {
		t.Fatalf("parsePackageDir 不应再产出文件清单，得 %d 条", len(pp2.Row.Files))
	}
}

// TestSavePackageZipFailCleansDir 校验失败必须清理解包目录（防半成品占位导致同包重传被污染）
func TestSavePackageZipFailCleansDir(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "in.zip")
	buildPkgZip(t, zipPath, "images/app.tar")

	if _, err := savePackageZip(root, zipPath); err == nil {
		t.Fatal("内容与 manifest 不符应被拒绝")
	} else if !strings.Contains(err.Error(), "manifest 完整性校验不符") {
		t.Fatalf("错误语义不符：%v", err)
	}
	// 失败必须清理半解包目标目录（packages/ 空父目录无害保留）
	if _, err := os.Stat(filepath.Join(root, "packages", "load-app-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("失败后应清理解包目录：%v", err)
	}
}
