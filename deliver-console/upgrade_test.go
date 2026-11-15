package main

// agent 自升级（console 侧）：注入守卫矩阵 + 二进制分发端点 + 手动 pending 下发链路。
// agentStamp 为构建期注入变量（测试构建为空）——用例内直接改写并 Cleanup 还原；
// 内嵌 agent-bin/* 为随仓库跟踪的真实二进制（>100KB），sha 断言真实可用。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newUpgradeTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return &Server{store: st, queues: map[int64]chan *Task{}, sess: map[int64]*sessionRun{},
		pendingAgentUpgrade: map[int64]bool{}}
}

func withAgentStamp(t *testing.T, stamp string) {
	t.Helper()
	orig := agentStamp
	agentStamp = stamp
	t.Cleanup(func() { agentStamp = orig })
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestBuildUpgradeDirectiveGuards(t *testing.T) {
	if agentBinSHA("linux") == "" {
		t.Fatal("内嵌 linux agent 二进制不可用（占位/缺失）——升级注入整体失效，请完整构建")
	}
	withAgentStamp(t, "202610292105")
	s := newUpgradeTestServer(t)

	base := func() *hostRow {
		return &hostRow{Name: "h1", Info: HostInfo{Hostname: "h1", OS: "linux", Arch: "amd64",
			AgentVersion: "202610270919"}}
	}
	cases := []struct {
		name   string
		mutate func(*hostRow)
		auto   string // settings 开关："1"/"0"/""（不设置）
		pend   bool
		want   bool
	}{
		{"本机节点不下发", func(h *hostRow) { h.Local = true }, "", true, false},
		{"已吊销不下发", func(h *hostRow) { h.Revoked = true }, "", true, false},
		{"OS 未知不下发", func(h *hostRow) { h.Info.OS = "" }, "", true, false},
		{"版本未知不下发", func(h *hostRow) { h.Info.AgentVersion = "" }, "", true, false},
		{"版本一致不下发", func(h *hostRow) { h.Info.AgentVersion = "202610292105" }, "", true, false},
		{"agent 新于内嵌（console 回退）不下发", func(h *hostRow) { h.Info.AgentVersion = "202612312359" }, "", true, false},
		{"开关关且无 pending 不下发", nil, "0", false, false},
		{"开关关 + 手动 pending 下发", nil, "0", true, true},
		{"开关开即下发（无需 pending）", nil, "1", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.auto != "" {
				if err := s.store.PutSetting(agentAutoUpgradeKey, c.auto); err != nil {
					t.Fatal(err)
				}
			}
			h := base()
			if c.mutate != nil {
				c.mutate(h)
			}
			d := s.buildUpgradeDirective(h, c.pend)
			if (d != nil) != c.want {
				t.Fatalf("directive = %+v, want 非nil=%v", d, c.want)
			}
			if d != nil {
				if d.Version != "202610292105" || d.OS != "linux" || d.SHA256 != agentBinSHA("linux") {
					t.Errorf("指令字段异常: %+v", d)
				}
			}
		})
	}

	// 内嵌戳缺失（测试/dev 构建语义）：恒不下发
	withAgentStamp(t, "")
	if d := s.buildUpgradeDirective(base(), true); d != nil {
		t.Fatalf("agentStamp 空（dev 构建）不应下发: %+v", d)
	}
}

func TestHandleAgentBinary(t *testing.T) {
	s := newUpgradeTestServer(t)
	h := &hostRow{Name: "t1", AgentToken: "tok1", Info: HostInfo{Hostname: "t1", OS: "linux"}}
	if err := s.store.UpsertHost(h); err != nil {
		t.Fatal(err)
	}
	do := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("X-Agent-Token", token)
		}
		w := httptest.NewRecorder()
		s.handleAgentBinary(w, req)
		return w
	}
	if w := do("/api/agent/binary/linux", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无令牌 = %d, want 401", w.Code)
	}
	if w := do("/api/agent/binary/linux", "bad"); w.Code != http.StatusUnauthorized {
		t.Errorf("错令牌 = %d, want 401", w.Code)
	}
	if w := do("/api/agent/binary/darwin", "tok1"); w.Code != http.StatusBadRequest {
		t.Errorf("非法 OS = %d, want 400", w.Code)
	}
	w := do("/api/agent/binary/linux", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("正常下载 = %d: %s", w.Code, w.Body.String())
	}
	if got := shaHex(w.Body.Bytes()); got != agentBinSHA("linux") {
		t.Errorf("下载字节 sha 不符：got %s want %s", got, agentBinSHA("linux"))
	}
	if w2 := do("/api/agent/binary/windows", "tok1"); w2.Code != http.StatusOK {
		t.Errorf("windows 下载 = %d, want 200", w2.Code)
	}
}

func TestPollUpgradeDirectiveManualFlow(t *testing.T) {
	withAgentStamp(t, "202610292105")
	s := newUpgradeTestServer(t)
	h := &hostRow{Name: "t1", AgentToken: "tok1", Info: HostInfo{Hostname: "t1", OS: "linux", Arch: "amd64",
		AgentVersion: "202610270919"}}
	if err := s.store.UpsertHost(h); err != nil {
		t.Fatal(err)
	}
	poll := func() PollResp {
		req := httptest.NewRequest(http.MethodGet, "/api/agent/poll?wait=0", nil)
		req.Header.Set("X-Agent-Token", "tok1")
		w := httptest.NewRecorder()
		s.handleAgentPoll(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("poll = %d: %s", w.Code, w.Body.String())
		}
		var resp PollResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := poll(); r.Upgrade != nil {
		t.Fatalf("开关关且无 pending 不应注入: %+v", r.Upgrade)
	}
	// 手动按钮置位 → 注入一次成功；pending 保留（版本未翻绿前持续重发自愈）
	s.setPendingUpgrade(h.ID)
	r := poll()
	if r.Upgrade == nil || r.Upgrade.Version != "202610292105" {
		t.Fatalf("pending 置位后应注入: %+v", r.Upgrade)
	}
	if !s.hasPendingUpgrade(h.ID) {
		t.Fatal("版本未翻绿前 pending 应保留（失败自愈重发）")
	}
	// agent 升级成功重新注册（版本翻绿）→ 不再注入且 pending 被清位
	h.Info.AgentVersion = "202610292105"
	if err := s.store.UpsertHost(h); err != nil {
		t.Fatal(err)
	}
	if r := poll(); r.Upgrade != nil {
		t.Fatalf("版本达标后不应注入: %+v", r.Upgrade)
	}
	if s.hasPendingUpgrade(h.ID) {
		t.Fatal("版本达标后 pending 应被清位")
	}
	// 自动开关路径：旧版本 + 开关开 → 无需 pending 即注入
	h.Info.AgentVersion = "202610280000"
	_ = s.store.UpsertHost(h)
	_ = s.store.PutSetting(agentAutoUpgradeKey, "1")
	if r := poll(); r.Upgrade == nil {
		t.Fatal("自动开关开应注入")
	}
}
