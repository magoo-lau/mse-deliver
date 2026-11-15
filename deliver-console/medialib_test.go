package main

// 版本化介质库单测：版本比较/最新选取/介质选取/旧单介质迁移/compose 在线直链渲染。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerEqual(t *testing.T) {
	if !verEqual("27.5.1", "v27.5.1") || !verEqual(" 2.30.3 ", "v2.30.3") {
		t.Error("前导 v/空白应归一相等")
	}
	if verEqual("", "1") || verEqual("1", "") || verEqual("", "") {
		t.Error("空串不等于任何值")
	}
	if verEqual("2.30.3", "2.31.0") {
		t.Error("不同版本不应相等")
	}
}

func TestVerLess(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"27.9.3", "27.10.1", true},   // 数字段数值比较（非字典序）
		{"27.10.1", "27.9.3", false},  //
		{"2.30.3", "v2.31.0", true},   // 前导 v 归一
		{"v2.30.3", "2.30.3", false},  // 归一后相等
		{"legacy", "1.0", true},       // legacy 恒最小
		{"1.0", "legacy", false},      //
		{"legacy", "legacy", false},   //
		{"27.5.1", "27.5.1", false},   //
		{"1.2", "1.2.1", true},        // 前缀相同短版本小
	}
	for _, c := range cases {
		if got := verLess(c.a, c.b); got != c.less {
			t.Errorf("verLess(%q, %q) = %v，期望 %v", c.a, c.b, got, c.less)
		}
	}
}

func TestLatestMediaVer(t *testing.T) {
	vers := map[string]DockerMediaMeta{"27.9.3": {}, "27.10.1": {}, "v28.0.0": {}}
	if got := latestMediaVer(vers); got != "v28.0.0" {
		t.Errorf("最新版本 = %q，期望 v28.0.0", got)
	}
	only := map[string]DockerMediaMeta{mediaLegacyVer: {}, "27.1": {}}
	if got := latestMediaVer(only); got != "27.1" {
		t.Errorf("真实版本优先 = %q", got)
	}
	if got := latestMediaVer(map[string]DockerMediaMeta{mediaLegacyVer: {}}); got != mediaLegacyVer {
		t.Errorf("仅 legacy 时返回 legacy = %q", got)
	}
	if got := latestMediaVer(map[string]DockerMediaMeta{}); got != "" {
		t.Errorf("空库应返回空 = %q", got)
	}
}

func TestPickMedia(t *testing.T) {
	lib := mediaLib{"linux-amd64": {
		"27.5.1":   DockerMediaMeta{SHA256: "aaa"},
		"27.10.1":  DockerMediaMeta{SHA256: "bbb"},
		mediaLegacyVer: DockerMediaMeta{SHA256: "ccc"},
	}}
	if m, ver := pickMedia(lib, "linux-amd64", ""); ver != "27.10.1" || m.SHA256 != "bbb" {
		t.Errorf("缺省取最新 = %q %+v", ver, m)
	}
	if m, ver := pickMedia(lib, "linux-amd64", "27.5.1"); ver != "27.5.1" || m.SHA256 != "aaa" {
		t.Errorf("指定版本精确取 = %q %+v", ver, m)
	}
	if m, ver := pickMedia(lib, "linux-amd64", mediaLegacyVer); ver != "" || m.SHA256 != "ccc" {
		t.Errorf("legacy 不带版本 = %q %+v", ver, m)
	}
	if m, _ := pickMedia(lib, "linux-amd64", "9.9.9"); m.SHA256 != "" {
		t.Error("未命中版本应返回零值")
	}
	if m, _ := pickMedia(lib, "linux-arm64", ""); m.SHA256 != "" {
		t.Error("空槽位应返回零值")
	}
}

// 旧单介质数据迁移：settings docker_media + docker-media/{slot}.tar.gz → legacy 版本条目 + 文件挪位，幂等
func TestLegacyDockerMediaMigration(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	oldDir := filepath.Join(root, "docker-media")
	if err := os.MkdirAll(oldDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "linux-amd64.tar.gz"), []byte("old-media"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSetting(legacyMediaKey, `{"linux-amd64":{"name":"old.tgz","sha256":"x","size":9,"uploadedAt":1}}`); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: store, root: root}
	lib := srv.mediaLib(mediaToolDocker)
	if _, ok := lib["linux-amd64"][mediaLegacyVer]; !ok {
		t.Fatalf("迁移后应有 legacy 条目 = %v", lib)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "linux-amd64.tar.gz")); !os.IsNotExist(err) {
		t.Error("旧介质文件应已挪走")
	}
	if _, err := os.Stat(srv.mediaFilePath(mediaToolDocker, "linux-amd64", mediaLegacyVer)); err != nil {
		t.Error("新路径介质文件缺失:", err)
	}
	if store.GetSetting(legacyMediaKey, "") != "" {
		t.Error("旧 settings 键应清除")
	}
	// 幂等：重复读取不产生重复条目
	lib2 := srv.mediaLib(mediaToolDocker)
	if len(lib2["linux-amd64"]) != 1 {
		t.Errorf("重复迁移产生多余条目 = %v", lib2)
	}
}

func TestComposePlatformAndRender(t *testing.T) {
	if got := composePlatform("amd64"); got != "linux-x86_64" {
		t.Errorf("amd64 平台段 = %q", got)
	}
	if got := composePlatform("arm64"); got != "linux-aarch64" {
		t.Errorf("arm64 平台段 = %q", got)
	}
	if got := composePlatform("386"); got != "" {
		t.Errorf("未知架构应空 = %q", got)
	}
	got := renderComposeURL(defaultComposeTemplate, "v2.30.3", "amd64")
	want := "https://github.com/docker/compose/releases/download/v2.30.3/docker-compose-linux-x86_64"
	if got != want {
		t.Errorf("渲染 = %q", got)
	}
	if got := renderComposeURL("https://mirror.example.com/compose", "v1", "amd64"); got != "https://mirror.example.com/compose" {
		t.Errorf("无占位符模板应原样返回 = %q", got)
	}
}
