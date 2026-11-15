// 大文件下载票单测：登录态签票 → 浏览器原生下载器免 token 头直 GET（GB 级包不再走 blob 内存）。
// 覆盖：无票无 token 拦 401；票放行（isPublicPath 形态级 + handler 票校验）；错票/过期票 401 作废。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageDownloadTicket(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.PutSetting(adminSettingKey, newPassRecord("p"))
	auth := NewAuth(store)
	zipPath := filepath.Join(dir, "demo-kit.zip")
	if err := os.WriteFile(zipPath, []byte("zip-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	rec := &PackageRec{Name: "demo-kit", Version: "1.0.0", AppCode: "demo", Status: "ok"}
	if err := store.InsertPackage(rec); err != nil {
		t.Fatal(err)
	}
	store.FinishPackage(rec.ID, "ok", "完成", "", zipPath)
	srv := &Server{store: store}

	// 复刻 main.go 的 /api/packages/ 路由分发（download/ticket 两分支）
	mux := http.NewServeMux()
	mux.HandleFunc("/api/packages/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		if strings.HasSuffix(path, "/download") {
			if id, ok := pathID(strings.TrimSuffix(path, "/download")); ok {
				srv.handlePackageDownload(w, r, id)
				return
			}
		} else if strings.HasSuffix(path, "/ticket") {
			if id, ok := pathID(strings.TrimSuffix(path, "/ticket")); ok {
				srv.handlePackageDownloadTicket(w, r, id)
				return
			}
		}
		w.WriteHeader(404)
	})
	ts := httptest.NewServer(auth.Middleware(mux))
	defer ts.Close()
	token := loginOK(t, auth, "p")

	// ① 无票无 token 的裸 GET → 中间件 401（旧直链口径不变）
	if resp, _ := http.Get(ts.URL + fmt.Sprintf("/api/packages/%d/download", rec.ID)); resp.StatusCode != 401 {
		t.Fatalf("裸 GET 应 401，实际 %d", resp.StatusCode)
	}
	// ② 登录态签票
	req, _ := http.NewRequest(http.MethodPost, ts.URL+fmt.Sprintf("/api/packages/%d/ticket", rec.ID), nil)
	req.Header.Set("X-Studio-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("签票失败: %v %d", err, resp.StatusCode)
	}
	var out struct{ URL string `json:"url"` }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if !strings.Contains(out.URL, "/download?t=") {
		t.Fatalf("票 URL 形态不符: %q", out.URL)
	}
	// ③ 免 token 头直下（浏览器原生下载器路径）：200 + 内容 + Content-Disposition
	dl, err := http.Get(ts.URL + out.URL)
	if err != nil || dl.StatusCode != 200 {
		t.Fatalf("带票下载失败: %v %d", err, dl.StatusCode)
	}
	body := make([]byte, 9)
	_, _ = dl.Body.Read(body)
	dl.Body.Close()
	if string(body) != "zip-bytes" {
		t.Fatalf("下载内容不符: %q", string(body))
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "demo-kit.zip") {
		t.Fatalf("Content-Disposition 缺文件名: %q", cd)
	}
	// ④ 错票 401 且作废
	if resp, _ := http.Get(ts.URL + fmt.Sprintf("/api/packages/%d/download?t=deadbeef", rec.ID)); resp.StatusCode != 401 {
		t.Fatalf("错票应 401，实际 %d", resp.StatusCode)
	}
	// ⑤ 过期票 401 且作废（防重放）
	dlTicketsMu.Lock()
	dlTickets["expired"] = dlTicket{pkgID: rec.ID, exp: time.Now().Add(-time.Minute)}
	dlTicketsMu.Unlock()
	if resp, _ := http.Get(ts.URL + fmt.Sprintf("/api/packages/%d/download?t=expired", rec.ID)); resp.StatusCode != 401 {
		t.Fatalf("过期票应 401，实际 %d", resp.StatusCode)
	}
	dlTicketsMu.Lock()
	_, still := dlTickets["expired"]
	dlTicketsMu.Unlock()
	if still {
		t.Fatal("过期票应被作废删除")
	}
}
