package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeNacos 模拟 Nacos v3 admin API（config 查/写 + core/state 探活），支持 serverIdentity 头校验
type fakeNacos struct {
	mu     sync.Mutex
	cfgs   map[string]string // key: group/dataId
	status int               // health 探活返回码（可设 500 模拟不健康）
	auth   bool              // 是否校验身份头
}

func newFakeNacos() *fakeNacos {
	return &fakeNacos{cfgs: map[string]string{}, status: http.StatusOK}
}

func (f *fakeNacos) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.auth && r.Header.Get("serverIdentity") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/nacos/v3/admin/core/state":
			w.WriteHeader(f.status)
			return
		case r.URL.Path == "/nacos/v3/admin/cs/config" && r.Method == http.MethodGet:
			key := r.URL.Query().Get("groupName") + "/" + r.URL.Query().Get("dataId")
			f.mu.Lock()
			content, ok := f.cfgs[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": content})
		case r.URL.Path == "/nacos/v3/admin/cs/config" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			key := r.PostForm.Get("groupName") + "/" + r.PostForm.Get("dataId")
			f.mu.Lock()
			f.cfgs[key] = r.PostForm.Get("content")
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func startNacos(t *testing.T, f *fakeNacos) string {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func clientFor(t *testing.T, a args) *client {
	t.Helper()
	c, err := newClient(a)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func TestHealthAndRead(t *testing.T) {
	f := newFakeNacos()
	f.cfgs["DEFAULT_GROUP/app.yaml"] = "mse:\n  a: 1\n"
	base := startNacos(t, f)
	ctx := context.Background()

	c := clientFor(t, args{ServerAddr: base})
	if err := c.health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	content, exists, err := c.get(ctx, "app.yaml", "")
	if err != nil || !exists || content != "mse:\n  a: 1\n" {
		t.Fatalf("get: exists=%v content=%q err=%v", exists, content, err)
	}
	if _, exists, err := c.get(ctx, "missing.yaml", ""); err != nil || exists {
		t.Fatalf("get missing: exists=%v err=%v", exists, err)
	}

	f.status = 500
	if err := c.health(ctx); err == nil {
		t.Fatal("health 应失败")
	}
}

func TestPlanApplyIdempotentAndForce(t *testing.T) {
	f := newFakeNacos()
	f.cfgs["DEFAULT_GROUP/same.yaml"] = "a: 1\n"
	f.cfgs["DEFAULT_GROUP/changed.yaml"] = "a: old\n"
	base := startNacos(t, f)
	ctx := context.Background()
	c := clientFor(t, args{ServerAddr: base})

	items := []item{
		{DataID: "new.yaml", Content: "n: 1\n"},
		{DataID: "same.yaml", Content: "a: 1\n"},
		{DataID: "changed.yaml", Content: "a: new\n"},
	}
	// plan：missing / same / differs 三态
	pd, err := c.plan(ctx, items)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	pj, _ := json.Marshal(pd)
	for _, want := range []string{`"missing"`, `"same"`, `"differs"`} {
		if !strings.Contains(string(pj), want) {
			t.Fatalf("plan 结果缺 %s: %s", want, pj)
		}
	}
	// apply 幂等：只写缺失，differs 跳过（保护线上手改）
	ad, err := c.apply(ctx, items, "idempotent")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	am, _ := json.Marshal(ad)
	if !strings.Contains(string(am), `"new.yaml"`) {
		t.Fatalf("apply 应写入 new.yaml: %s", am)
	}
	if !strings.Contains(string(am), `"changed.yaml"`) {
		t.Fatalf("apply 幂等模式 changed.yaml 应进 skippedExists: %s", am)
	}
	if got := f.cfgs["DEFAULT_GROUP/changed.yaml"]; got != "a: old\n" {
		t.Fatalf("幂等模式不应覆盖 changed.yaml，现值 %q", got)
	}
	// apply force：覆盖差异项
	if _, err := c.apply(ctx, items, "force"); err != nil {
		t.Fatalf("apply force: %v", err)
	}
	if got := f.cfgs["DEFAULT_GROUP/changed.yaml"]; got != "a: new\n" {
		t.Fatalf("force 模式应覆盖 changed.yaml，现值 %q", got)
	}
	if got := f.cfgs["DEFAULT_GROUP/new.yaml"]; got != "n: 1\n" {
		t.Fatalf("new.yaml 应已写入，现值 %q", got)
	}
}

func TestIdentityHeaderAndDefaults(t *testing.T) {
	f := newFakeNacos()
	f.auth = true
	base := startNacos(t, f)
	ctx := context.Background()

	// 无身份头：401 报错
	c := clientFor(t, args{ServerAddr: base})
	if err := c.health(ctx); err == nil {
		t.Fatal("无身份头应被拒")
	}
	// 正确身份头：通过
	c2 := clientFor(t, args{ServerAddr: base, IdentityKey: "serverIdentity", IdentityValue: "secret"})
	if err := c2.health(ctx); err != nil {
		t.Fatalf("带身份头 health: %v", err)
	}
	// serverAddr 无 scheme 自动补 http://；namespace/group 缺省 public/DEFAULT_GROUP
	c3 := clientFor(t, args{ServerAddr: strings.TrimPrefix(base, "http://")})
	if c3.ns != "public" || c3.group != "DEFAULT_GROUP" || !strings.HasPrefix(c3.base, "http://") {
		t.Fatalf("缺省补全异常: %+v", c3)
	}
}

func TestDispatchValidation(t *testing.T) {
	if _, err := dispatch(context.Background(), request{Op: "bogus"}); err == nil {
		t.Fatal("未知 op 应报错")
	}
	if _, err := dispatch(context.Background(), request{Op: "read"}); err == nil {
		t.Fatal("read 缺 dataId 应报错")
	}
	if _, err := dispatch(context.Background(), request{Op: "apply", Args: args{ServerAddr: "http://x"}}); err == nil {
		t.Fatal("apply 缺 items 应报错")
	}
	if _, err := dispatch(context.Background(), request{Op: "health", Args: args{}}); err == nil {
		t.Fatal("缺 serverAddr 应报错")
	}
}
