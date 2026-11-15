// 制品上传单测：multipart part 按到达顺序流式处理，浏览器 FormData 按 append 序发送
// （旧 UI 把 file append 在 kind 之前），handler 必须对 part 顺序不敏感；kind 无效须清已落盘文件。
package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newMaterialServer(t *testing.T) *Server {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &Server{store: store, root: t.TempDir()}
}

func postMaterial(t *testing.T, s *Server, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/materials", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	s.handleMaterials(rec, req)
	return rec
}

func TestMaterialUploadPartOrderInsensitive(t *testing.T) {
	s := newMaterialServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "extras.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("hello-extra")); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("kind", "extra")
	_ = mw.WriteField("appCode", "demo")
	_ = mw.WriteField("version", "1.0.0")
	_ = mw.Close()

	rec := postMaterial(t, s, &buf, mw.FormDataContentType())
	if rec.Code != 200 {
		t.Fatalf("file 在前 kind 在后应上传成功: %d %s", rec.Code, rec.Body.String())
	}
	list, err := s.store.ListMaterials("")
	if err != nil || len(list) != 1 {
		t.Fatalf("应落库 1 条: %v %d", err, len(list))
	}
	m := list[0]
	if m.Kind != "extra" || m.FileName != "extras.zip" || m.AppCode != "demo" || m.Version != "1.0.0" || m.Size != 11 {
		t.Fatalf("落库字段不符: %+v", m)
	}
}

func TestMaterialUploadBadKindCleansFile(t *testing.T) {
	s := newMaterialServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "x.bin")
	_, _ = fw.Write([]byte("junk"))
	_ = mw.WriteField("kind", "nope")
	_ = mw.Close()

	rec := postMaterial(t, s, &buf, mw.FormDataContentType())
	if rec.Code != 400 {
		t.Fatalf("kind 无效应 400: %d %s", rec.Code, rec.Body.String())
	}
	ents, err := os.ReadDir(filepath.Join(s.root, materialsDir))
	if err != nil || len(ents) != 0 {
		t.Fatalf("kind 无效应清理已落盘文件: %v %d", err, len(ents))
	}
	if list, _ := s.store.ListMaterials(""); len(list) != 0 {
		t.Fatalf("不应落库: %d", len(list))
	}
}
