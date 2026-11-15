// 登录与会话单测：密码哈希/校验、登录对错、放行清单、token 过期与滑动续期、改密吊销全部会话。
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestAuth(t *testing.T) (*Auth, string) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pass := "test-pass-123"
	store.PutSetting(adminSettingKey, newPassRecord(pass))
	return NewAuth(store), pass
}

func postJSON(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func loginOK(t *testing.T, a *Auth, pass string) string {
	t.Helper()
	rec := postJSON(a.handleLogin, map[string]string{"user": "admin", "password": pass})
	if rec.Code != 200 {
		t.Fatalf("登录应成功: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatal("登录未返回 token")
	}
	return out.Token
}

func TestPasswordHashAndVerify(t *testing.T) {
	record := newPassRecord("s3cret-pass")
	if !verifyPassword(record, "s3cret-pass") {
		t.Fatal("正确密码应通过")
	}
	if verifyPassword(record, "wrong") || verifyPassword(record, "") {
		t.Fatal("错误密码不应通过")
	}
	if verifyPassword("not-a-record", "x") {
		t.Fatal("畸形记录应校验失败")
	}
	// 同密码两次生成记录不同（随机 salt）
	if newPassRecord("s3cret-pass") == record {
		t.Fatal("salt 应随机")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	a, _ := newTestAuth(t)
	if rec := postJSON(a.handleLogin, map[string]string{"password": "bad"}); rec.Code != 401 {
		t.Fatalf("错误密码应 401: %d", rec.Code)
	}
	if rec := postJSON(a.handleLogin, map[string]string{"user": "hacker", "password": "test-pass-123"}); rec.Code != 401 {
		t.Fatalf("非 admin 账号应 401: %d", rec.Code)
	}
}

func TestMiddlewareAllowAndBlock(t *testing.T) {
	a, pass := newTestAuth(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mw := a.Middleware(inner)

	// 非 /api 路径（SPA 静态资源）直通
	if rec := httptest.NewRecorder(); func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		mw.ServeHTTP(rec, req)
		return rec
	}().Code != 200 {
		t.Fatal("静态资源应放行")
	}
	// 无 token 访问受保护 API → 401
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401: %d", rec.Code)
	}
	// Bearer token 通过
	token := loginOK(t, a, pass)
	req = httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("带 token 应通过: %d", rec.Code)
	}
	// X-Studio-Token 等价（collect-kit 推送形态）
	req = httptest.NewRequest(http.MethodPost, "/api/components/import", nil)
	req.Header.Set("X-Studio-Token", token)
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("X-Studio-Token 应通过: %d", rec.Code)
	}
}

func TestTokenExpiryAndSlidingRenewal(t *testing.T) {
	a, pass := newTestAuth(t)
	token, _ := a.issue()
	// 手工把过期时间拨到过去 → 失效
	a.mu.Lock()
	a.tokens[token] = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if a.valid(token) {
		t.Fatal("过期 token 应无效")
	}
	// 续期：touch 只对未过期 token 生效
	a.mu.Lock()
	a.tokens[token] = time.Now().Add(time.Minute)
	a.mu.Unlock()
	a.touch(token)
	a.mu.Lock()
	exp := a.tokens[token]
	a.mu.Unlock()
	if !exp.After(time.Now().Add(tokenTTL - time.Minute)) {
		t.Fatal("touch 应滑动续期至完整 TTL")
	}
	// 登录→登出→token 失效
	tok := loginOK(t, a, pass)
	a.revoke(tok)
	if a.valid(tok) {
		t.Fatal("登出后 token 应失效")
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	a, pass := newTestAuth(t)
	token := loginOK(t, a, pass)

	// 旧密错误拒绝
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/settings/password",
		bytes.NewReader([]byte(`{"oldPassword":"bad","newPassword":"new-pass-456"}`)))
	a.handlePasswordChange(rec, req)
	if rec.Code != 400 {
		t.Fatalf("旧密错误应 400: %d", rec.Code)
	}
	// 正确改密
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/settings/password",
		bytes.NewReader([]byte(`{"oldPassword":"test-pass-123","newPassword":"new-pass-456"}`)))
	a.handlePasswordChange(rec, req)
	if rec.Code != 200 {
		t.Fatalf("改密应成功: %d %s", rec.Code, rec.Body.String())
	}
	// 全部会话吊销 + 旧密码登录失败 + 新密码可登录
	if a.valid(token) {
		t.Fatal("改密后旧会话应全部吊销")
	}
	if r := postJSON(a.handleLogin, map[string]string{"password": pass}); r.Code != 401 {
		t.Fatal("旧密码应登录失败")
	}
	loginOK(t, a, "new-pass-456")
	// 过短新密拒绝
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/settings/password",
		bytes.NewReader([]byte(`{"oldPassword":"new-pass-456","newPassword":"123"}`)))
	a.handlePasswordChange(rec, req)
	if rec.Code != 400 {
		t.Fatal("过短新密应 400")
	}
}

func TestEnsureAdminPasswordFirstStartOnly(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pw := EnsureAdminPassword(store)
	if pw == "" || len(pw) != 10 {
		t.Fatalf("首启应生成 10 位随机密码: %q", pw)
	}
	if EnsureAdminPassword(store) != "" {
		t.Fatal("二次启动不应再生成（已设密码）")
	}
	a := NewAuth(store)
	rec := postJSON(a.handleLogin, map[string]string{"password": pw})
	if rec.Code != 200 {
		t.Fatalf("首启密码应可登录: %d %s", rec.Code, rec.Body.String())
	}
}
