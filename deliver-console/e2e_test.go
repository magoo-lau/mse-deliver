package main

// 端到端联调（进程级，不依赖 docker——用 cmd+gate 节点验证全链路）：
// 启动 console HTTP 服务 → agent 凭引导令牌注册（模拟目标机，鉴权含错令牌拒绝/吊销 401）→
// 上传 format:3 包 → 自定义 DAG 会话（cmd → gate 人工确认）→ agent 轮询领任务 → 上报 →
// 会话终态 done → 日志/探测文件断言。本机节点（load/compose/systemd）payload 物化已由
// engine_test.go 覆盖；docker 真实拉起在验收环境走本机节点路径人工执行。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	_ = store.PutSetting(adminPassKey, newPassRecord("e2e-pass"))
	auth := NewAuth(store)
	srv := &Server{store: store, auth: auth, root: root,
		queues: map[int64]chan *Task{}, sess: map[int64]*sessionRun{},
		pendingAgentUpgrade: map[int64]bool{}, terminals: map[int64]*terminalRun{}}
	_ = EnsureEnrollToken(store)
	ts := httptest.NewServer(auth.Middleware(srv.testRouter()))
	t.Cleanup(ts.Close)
	return srv, ts
}

// testRouter 与 main.go 路由一致的最小子集（登录/主机/agent/包/会话/docker 安装源）
func (s *Server) testRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", s.auth.handleLogin)
	mux.HandleFunc("/api/whoami", s.auth.handleWhoami)
	mux.HandleFunc("/api/hosts", s.handleHosts)
	mux.HandleFunc("/api/hosts/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/hosts/"), "/")
		id, action, ok := strings.Cut(rest, "/")
		hostID, _ := parseID(id)
		if !ok || hostID == 0 {
			writeErr(w, 404, "not found")
			return
		}
		switch action {
		case "install-docker":
			s.handleHostInstallDocker(w, r, hostID)
		case "install-compose":
			s.handleHostInstallCompose(w, r, hostID)
		case "sessions":
			s.handleHostSessions(w, r, hostID)
		case "upgrade-agent":
			s.handleHostUpgradeAgent(w, r, hostID)
		case "update", "":
			s.handleHostUpdate(w, r, hostID)
		default:
			writeErr(w, 404, "not found")
		}
	})
	mux.HandleFunc("/api/exec", s.handleExec)
	mux.HandleFunc("/api/exec-presets", s.handleExecPresets)
	mux.HandleFunc("/api/terminal/open", s.handleTerminalOpen)
	mux.HandleFunc("/api/terminal/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/terminal/"), "/")
		idStr, action, _ := strings.Cut(rest, "/")
		sid, ok := parseID(idStr)
		if !ok {
			writeErr(w, 404, "not found")
			return
		}
		switch action {
		case "run":
			s.handleTerminalRun(w, r, sid)
		case "stop":
			s.handleTerminalStop(w, r, sid)
		case "state":
			s.handleTerminalState(w, r, sid)
		case "close":
			s.handleTerminalClose(w, r, sid)
		default:
			writeErr(w, 404, "not found")
		}
	})
	mux.HandleFunc("/api/exec-presets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/exec-presets/"), "/")
		if id != "" && r.Method == http.MethodDelete {
			s.handleExecPresetDelete(w, r, id)
			return
		}
		writeErr(w, 404, "not found")
	})
	mux.HandleFunc("/api/agent/register", s.handleAgentRegister)
	mux.HandleFunc("/api/agent/poll", s.handleAgentPoll)
	mux.HandleFunc("/api/agent/report", s.handleAgentReport)
	mux.HandleFunc("/api/agent/binary/", s.handleAgentBinary)
	mux.HandleFunc("/api/settings/agent-upgrade", s.handleAgentUpgradeSetting)
	mux.HandleFunc("/api/agent/media/", s.handleAgentMedia)
	mux.HandleFunc("/api/agent/docker-media/", s.handleAgentDockerMedia)
	mux.HandleFunc("/api/file/", s.handleAgentFile)
	mux.HandleFunc("/api/docker-source", s.handleDockerSource)
	mux.HandleFunc("/api/compose-source", s.handleComposeSource)
	mux.HandleFunc("/api/docker-source/media/", mediaRoute(s, mediaToolDocker))
	mux.HandleFunc("/api/compose-source/media/", mediaRoute(s, mediaToolCompose))
	mux.HandleFunc("/api/packages", s.handlePackages)
	mux.HandleFunc("/api/schemes/autodag", s.handleAutoDag)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
		idStr, tail, _ := strings.Cut(rest, "/")
		id, ok := parseID(idStr)
		if !ok {
			writeErr(w, 404, "not found")
			return
		}
		if tail == "" {
			s.handleSessionDetail(w, r, id)
			return
		}
		if tail == "export" {
			s.handleSessionExport(w, r, id)
			return
		}
		if tail == "logs" {
			s.handleSessionLogs(w, r, id)
			return
		}
		if tail == "abort" {
			s.handleSessionAbort(w, r, id)
			return
		}
		if node, action, ok := strings.Cut(strings.TrimPrefix(tail, "nodes/"), "/"); ok && node != "" && action != "" {
			s.handleNodeAction(w, r, id, node, action)
			return
		}
		writeErr(w, 404, "not found")
	})
	return mux
}

func uiDo(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Console-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// uploadPkg 上传 format:3 测试 zip（含 1 个镜像 tar 供 load 链路 + 1 个随机凭据组件供导出脱敏断言）
func uploadPkg(t *testing.T, ts *httptest.Server, token string) int64 {
	t.Helper()
	tmp := t.TempDir()
	decl := ComponentsDecl{Format: 3, Name: "e2e-app", Version: "1.0.0",
		Entry:     EntryDecl{Unit: "app", Service: "app", Port: "18099", HealthPath: "/health"},
		Units:     []UnitDecl{{Code: "app", Service: "app", Services: []string{"app"}, Image: "e2e/app:1.0.0", Port: "18099", HealthPath: "/health"}},
		Variables: []VarDecl{{Name: "E2E_VAR", Source: "input", Value: "hi"}},
		Components: []ComponentDecl{{Code: "db", Services: []string{"db"},
			Credentials: []Credential{{Placeholder: "DB_PASSWORD", Generate: "random"}}}}}
	compose := "name: e2e-app\nservices:\n  app:\n    image: e2e/app:1.0.0\n    ports:\n      - \"18099:18099\"\n"
	files := map[string]string{
		composeFile:                compose,
		componentsFile:             string(mustJSON(decl)),
		"images/e2e_app_1.0.0.tar": "fake-tar-app",
	}
	manifest := map[string]string{}
	for rel, content := range files {
		manifest[rel] = hashStr(content)
	}
	files[manifestFile] = string(mustJSON(map[string]any{"files": manifest}))

	zipPath := filepath.Join(tmp, "e2e.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	for rel, content := range files {
		w, _ := zw.Create(rel)
		_, _ = w.Write([]byte(content))
	}
	zw.Close()
	f.Close()
	data, _ := os.ReadFile(zipPath)

	var buf bytes.Buffer
	buf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"e2e.zip\"\r\n\r\n")
	buf.Write(data)
	buf.WriteString("\r\n--B--\r\n")
	req, _ := http.NewRequest("POST", ts.URL+"/api/packages", &buf)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	req.Header.Set("X-Console-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("上传失败: %v %v", err, resp)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res.ID == 0 {
		t.Fatal("未返回包 id")
	}
	return res.ID
}

func TestE2EAgentFlow(t *testing.T) {
	srv, ts := newTestServer(t)

	// ① 登录（错密码 401 → 对密码 200）
	if resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"wrong"}`)); resp.StatusCode != 401 {
		t.Fatalf("错密码应 401，实际 %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()
	if login.Token == "" {
		t.Fatal("未取到 token")
	}
	// 未带 token 访问受保护端点 401
	if resp := uiDo(t, "GET", ts.URL+"/api/hosts", "", nil); resp.StatusCode != 401 {
		t.Fatalf("未登录应 401，实际 %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// ② agent 注册（引导令牌换 agent 令牌；错令牌拒绝）
	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-node","os":"linux","docker":true,"ips":["10.9.9.9"]}}`, enroll)
	resp2, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody))
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("注册失败: %v %v", err, resp2)
	}
	var reg RegisterResp
	_ = json.NewDecoder(resp2.Body).Decode(&reg)
	resp2.Body.Close()
	if reg.AgentToken == "" || reg.HostID == 0 {
		t.Fatalf("注册响应异常: %+v", reg)
	}
	if resp3, _ := http.Post(ts.URL+"/api/agent/register", "application/json",
		strings.NewReader(`{"enrollToken":"bad","host":{"hostname":"x"}}`)); resp3.StatusCode != 401 {
		t.Errorf("错引导令牌应 401，实际 %d", resp3.StatusCode)
		resp3.Body.Close()
	}

	// ③ 上传包（manifest 强校验通过即登记）
	pkgID := uploadPkg(t, ts, login.Token)

	// ④ 创建会话：自定义 DAG = cmd（写探测文件，指派 agent 主机）→ gate（人工确认）
	probeFile := filepath.Join(t.TempDir(), "done.txt")
	pl := Placement{Host: map[string]int64{}, Mode: map[string]string{}, Vars: map[string]string{"E2E_VAR": "hi"}}
	dag := []DagNode{
		{ID: "cmd-write", Kind: "cmd", Host: reg.HostID, Script: "echo e2e-ok > " + probeFile, Title: "写探测文件"},
		{ID: "gate-1", Kind: "gate", Title: "人工确认", Deps: []string{"cmd-write"}},
	}
	sessBody, _ := json.Marshal(map[string]any{"packageId": pkgID, "placement": pl, "dag": dag, "mode": "fresh"})
	resp4 := uiDo(t, "POST", ts.URL+"/api/sessions", login.Token, sessBody)
	var sessRes struct {
		ID    int64 `json:"id"`
		Nodes int   `json:"nodes"`
	}
	_ = json.NewDecoder(resp4.Body).Decode(&sessRes)
	resp4.Body.Close()
	if sessRes.ID == 0 || sessRes.Nodes != 2 {
		t.Fatalf("会话创建异常: %+v", sessRes)
	}

	// ⑤ agent 轮询领任务（cmd-write）；gate 等待期不再派发
	poll := func() *Task {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pr PollResp
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Task
	}
	task := poll()
	if task == nil || task.Kind != "cmd" || task.NodeID != "cmd-write" {
		t.Fatalf("agent 应领到 cmd-write，实际 %+v", task)
	}
	if t2 := poll(); t2 != nil {
		t.Fatalf("gate 等待期不应派发任务，实际 %+v", t2)
	}
	// agent 执行（本测试直接落文件模拟）并上报
	report := func(node, status, msg string, logs ...string) {
		body, _ := json.Marshal(Report{SessionID: fmt.Sprint(sessRes.ID), NodeID: node, Status: status, Message: msg, Logs: logs})
		req, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("上报失败 %s: %v %v", node, err, resp)
		}
		resp.Body.Close()
	}
	_ = os.WriteFile(probeFile, []byte("e2e-ok"), 0644)
	report("cmd-write", "running", "", "$ echo e2e-ok > "+probeFile)
	report("cmd-write", "ok", "")

	// ⑥ gate 人工确认 → 会话 done
	gateURL := fmt.Sprintf("%s/api/sessions/%d/nodes/gate-1/gate", ts.URL, sessRes.ID)
	if resp5 := uiDo(t, "POST", gateURL, login.Token, nil); resp5.StatusCode != 200 {
		t.Fatalf("gate 确认失败: %v", resp5.Status)
	} else {
		resp5.Body.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ss, err := srv.store.SessionByID(sessRes.ID)
		if err == nil && ss.Status == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ss, _ := srv.store.SessionByID(sessRes.ID); ss == nil || ss.Status != "done" {
		nodes, _ := srv.store.SessionNodes(sessRes.ID)
		t.Fatalf("会话未完成: %+v / %+v", ss, nodes)
	}
	if data, err := os.ReadFile(probeFile); err != nil || strings.TrimSpace(string(data)) != "e2e-ok" {
		t.Errorf("探测文件内容异常: %q %v", data, err)
	}
	lines, err := srv.store.LogsAfter(sessRes.ID, 0, 100, "")
	if err != nil || !containsLine(lines, "echo e2e-ok") {
		t.Errorf("日志缺失: %+v %v", lines, err)
	}

	// ⑦ 吊销后 agent 401
	_ = srv.store.RevokeHost(reg.HostID)
	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=0", nil)
	req.Header.Set("X-Agent-Token", reg.AgentToken)
	if resp6, err := http.DefaultClient.Do(req); err != nil || resp6.StatusCode != 401 {
		t.Errorf("吊销后应 401: %v %v", err, resp6)
	} else {
		resp6.Body.Close()
	}
}

func containsLine(lines []logLine, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l.Line, sub) {
			return true
		}
	}
	return false
}

// ---- 配置中心插件链端到端（本机节点真 spawn nacos-config 插件 + 假 Nacos）----

// fakeNacosCenter 假 Nacos v3 admin API（无鉴权）：GET 查/POST 写/core/state 探活
type fakeNacosCenter struct {
	mu   sync.Mutex
	cfgs map[string]string
	srv  *httptest.Server
}

func newFakeNacosCenter(t *testing.T) *fakeNacosCenter {
	f := &fakeNacosCenter{cfgs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/nacos/v3/admin/core/state", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/nacos/v3/admin/cs/config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.URL.Query().Get("groupName") + "/" + r.URL.Query().Get("dataId")
		switch r.Method {
		case http.MethodGet:
			content, ok := f.cfgs[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": content})
		case http.MethodPost:
			_ = r.ParseForm()
			f.cfgs[r.PostForm.Get("groupName")+"/"+r.PostForm.Get("dataId")] = r.PostForm.Get("content")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNacosCenter) get(dataID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.cfgs["DEFAULT_GROUP/"+dataID]
	return v, ok
}

// buildNacosPlugin 现场编译 nacos-config 插件（真 spawn 用；无 go 工具链时跳过）
func buildNacosPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, pluginFileName("nacos-config", runtime.GOOS))
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "../deliver-plugins/nacos-config"
	if buf, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("nacos-config 插件编译失败，跳过真 spawn e2e: %v: %s", err, buf)
	}
	return dir
}

func waitNodeStatus(t *testing.T, srv *Server, sessID int64, key, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		nodes, _ := srv.store.SessionNodes(sessID)
		for _, n := range nodes {
			if n.Key == key && n.Status == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("节点 %s 未达 %s", key, want)
}

// 全链路：外部 Nacos（假服务）→ 上传带 configCenter 的包 → 自定义 DAG 仅保留插件链（免 docker）→
// 本机节点真 spawn nacos-config：plan 比对（缺失）→ gate 人工确认 → apply 幂等写入 → health 探活 → done
func TestE2EPluginChainLocal(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.pluginDirOverride = buildNacosPlugin(t)

	// 本机节点（e2e server 不走 ensureLocalHost，手工注册）
	local := &hostRow{Name: "local-e2e", Local: true, AgentToken: randomToken(), Addr: "127.0.0.1",
		Info: HostInfo{Hostname: "local-e2e", OS: runtime.GOOS, Arch: runtime.GOARCH, Docker: true, IPs: []string{"127.0.0.1"}}}
	if err := srv.store.UpsertHost(local); err != nil {
		t.Fatal(err)
	}

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	fake := newFakeNacosCenter(t)

	// 上传带 configCenter 的包（nacos 未落机=外部，端点手填指向假 Nacos）
	decl := ComponentsDecl{Format: 3, Name: "e2e-cc", Version: "1.0.0",
		Entry:     EntryDecl{Unit: "app", Service: "app", Port: "18099"},
		Units:     []UnitDecl{{Code: "app", Service: "app", Services: []string{"app"}, Port: "18099", HealthPath: "/health"}},
		Variables: []VarDecl{{Name: "E2E_VAR", Source: "input", Value: "hi"}},
		Components: []ComponentDecl{{Code: "nacos", Services: []string{"nacos"},
			Endpoints:   []Endpoint{{Placeholder: "NACOS_ADDR", Service: "nacos", Port: "8848", From: "url", Scheme: "http"}},
			Credentials: []Credential{{Placeholder: "NACOS_IDENTITY_VALUE", Generate: "const", Value: "security"}},
			ConfigCenter: &ConfigCenterDecl{Kind: "nacos", Plugin: "nacos-config", IdentityValue: "NACOS_IDENTITY_VALUE",
				Configs: []ConfigItemDecl{{DataID: "app.yaml", File: "nacos/configs/app.yaml"}}}}}}
	compose := "name: e2e-cc\nservices:\n  app:\n    image: e2e/app:1.0.0\n    ports:\n      - \"18099:18099\"\n  nacos:\n    image: nacos/nacos-server:v3.0.0\n    ports:\n      - \"8848:8848\"\n"
	files := map[string]string{
		composeFile:                compose,
		componentsFile:             string(mustJSON(decl)),
		"nacos/configs/app.yaml":   "e2e: {{E2E_VAR}}\nnacos: {{NACOS_ADDR}}\n",
		"images/e2e_app_1.0.0.tar": "x", "images/nacos_nacos-server_v3.0.0.tar": "x",
	}
	manifest := map[string]string{}
	for rel, content := range files {
		manifest[rel] = hashStr(content)
	}
	files[manifestFile] = string(mustJSON(map[string]any{"files": manifest}))
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "cc.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	for rel, content := range files {
		w, _ := zw.Create(rel)
		_, _ = w.Write([]byte(content))
	}
	zw.Close()
	zf.Close()
	data, _ := os.ReadFile(zipPath)
	var buf bytes.Buffer
	buf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"cc.zip\"\r\n\r\n")
	buf.Write(data)
	buf.WriteString("\r\n--B--\r\n")
	req, _ := http.NewRequest("POST", ts.URL+"/api/packages", &buf)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	req.Header.Set("X-Console-Token", login.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("上传失败: %v %v", err, resp)
	} else {
		var res struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if res.ID == 0 {
			t.Fatal("未返回包 id")
		}
		req = nil
		_ = req
		// 会话：仅保留插件链四节点（免 docker load/compose）
		pl := Placement{Host: map[string]int64{"app": local.ID}, Mode: map[string]string{},
			Vars: map[string]string{"E2E_VAR": "hi"}, Ext: map[string]string{"NACOS_ADDR": fake.srv.URL}}
		dag := []DagNode{
			{ID: "cc-plan-nacos", Kind: "plugin"},
			{ID: "gate-cc-nacos", Kind: "gate", Deps: []string{"cc-plan-nacos"}},
			{ID: "cc-apply-nacos", Kind: "plugin", Deps: []string{"gate-cc-nacos"}},
			{ID: "cc-health-nacos", Kind: "plugin", Deps: []string{"cc-apply-nacos"}},
		}
		sessBody, _ := json.Marshal(map[string]any{"packageId": res.ID, "placement": pl, "dag": dag, "mode": "fresh"})
		resp2 := uiDo(t, "POST", ts.URL+"/api/sessions", login.Token, sessBody)
		var sr struct {
			ID    int64 `json:"id"`
			Nodes int   `json:"nodes"`
		}
		_ = json.NewDecoder(resp2.Body).Decode(&sr)
		resp2.Body.Close()
		if sr.ID == 0 || sr.Nodes != 4 {
			t.Fatalf("会话创建异常: %+v", sr)
		}

		// cc-plan：本机真 spawn 插件 → 假 Nacos 比对（基线缺失）
		waitNodeStatus(t, srv, sr.ID, "cc-plan-nacos", "ok", 30*time.Second)
		// gate 人工确认 → apply 幂等写入 → health → done
		if g := uiDo(t, "POST", fmt.Sprintf("%s/api/sessions/%d/nodes/gate-cc-nacos/gate", ts.URL, sr.ID), login.Token, nil); g.StatusCode != 200 {
			t.Fatalf("gate 确认失败: %d", g.StatusCode)
		} else {
			g.Body.Close()
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if ss, _ := srv.store.SessionByID(sr.ID); ss != nil && ss.Status == "done" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if ss, _ := srv.store.SessionByID(sr.ID); ss == nil || ss.Status != "done" {
			nodes, _ := srv.store.SessionNodes(sr.ID)
			t.Fatalf("会话未完成: %+v / %+v", ss, nodes)
		}
		wantBase := "e2e: hi\nnacos: " + fake.srv.URL + "\n"
		if got, ok := fake.get("app.yaml"); !ok || got != wantBase {
			t.Errorf("假 Nacos 应有渲染后的基线 app.yaml（变量/外部端点已代入），实际 ok=%v content=%q", ok, got)
		}
		lines, _ := srv.store.LogsAfter(sr.ID, 0, 200, "")
		if !containsLine(lines, "plan") && !containsLine(lines, "apply") {
			t.Errorf("日志应含插件执行痕迹: %d 行", len(lines))
		}
	}
}

// agent 介质下载 URL 契约：/api/file/{sessionId}/{包内相对路径}。
// 历史缺陷：agent 旧拼法缺 sessionId 段，console 把首段（如 images）当 sessionId 解析 → 400，
// 远端主机 load 节点全数失败。本用例锁死两端契约防回归。
func TestE2EAgentFileContract(t *testing.T) {
	srv, ts := newTestServer(t)

	h := &hostRow{Name: "agent-e2e", AgentToken: randomToken(), Addr: "10.9.9.9",
		Info: HostInfo{Hostname: "agent-e2e", OS: "linux", Docker: true, IPs: []string{"10.9.9.9"}}}
	if err := srv.store.UpsertHost(h); err != nil {
		t.Fatal(err)
	}

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	pkgID := uploadPkg(t, ts, login.Token)
	pkg, err := srv.store.PackageByID(pkgID)
	if err != nil {
		t.Fatal(err)
	}
	tarRel := "images/e2e_app_1.0.0.tar"
	if err := os.MkdirAll(filepath.Join(pkg.Dir, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg.Dir, filepath.FromSlash(tarRel)), []byte("fake-tar"), 0644); err != nil {
		t.Fatal(err)
	}

	// 成立会话（纯 cmd+gate 自定义 DAG 免 placement 校验）
	probe := filepath.Join(t.TempDir(), "done.txt")
	dag := []DagNode{
		{ID: "cmd-1", Kind: "cmd", Host: h.ID, Script: "echo ok > " + probe, Title: "探测"},
		{ID: "gate-1", Kind: "gate", Title: "确认", Deps: []string{"cmd-1"}},
	}
	body, _ := json.Marshal(map[string]any{"packageId": pkgID,
		"placement": Placement{Host: map[string]int64{}, Mode: map[string]string{}, Vars: map[string]string{"E2E_VAR": "hi"}},
		"dag":       dag, "mode": "fresh"})
	resp2 := uiDo(t, "POST", ts.URL+"/api/sessions", login.Token, body)
	var sr struct {
		ID    int64  `json:"id"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&sr)
	resp2.Body.Close()
	if sr.ID == 0 {
		t.Fatalf("会话未创建: %s", sr.Error)
	}

	get := func(url string) int {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("X-Agent-Token", h.AgentToken)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	if c := get(fmt.Sprintf("%s/api/file/%d/%s", ts.URL, sr.ID, tarRel)); c != 200 {
		t.Errorf("agent 下载（含 sessionId）应 200，实际 %d", c)
	}
	if c := get(ts.URL + "/api/file/" + tarRel); c != 400 {
		t.Errorf("缺 sessionId 的旧拼法应 400（锁死契约），实际 %d", c)
	}

	// 执行中进度回显：agent running 上报携带日志 → 节点 message 同步最后一行（UI 消息列实时反馈）
	repBody, _ := json.Marshal(map[string]any{"sessionId": fmt.Sprint(sr.ID), "nodeId": "cmd-1",
		"status": "running", "logs": []string{"开始下载 images/e2e_app_1.0.0.tar（1KB）", "下载 images/e2e_app_1.0.0.tar 50%（512B/1KB，1KB/s）"}})
	req3, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", strings.NewReader(string(repBody)))
	req3.Header.Set("X-Agent-Token", h.AgentToken)
	if r3, err := http.DefaultClient.Do(req3); err != nil || r3.StatusCode != 200 {
		t.Fatalf("进度上报失败: %v %v", err, r3)
	} else {
		r3.Body.Close()
	}
	waitNodeStatus(t, srv, sr.ID, "cmd-1", "running", 5*time.Second)
	nodes, _ := srv.store.SessionNodes(sr.ID)
	for _, n := range nodes {
		if n.Key == "cmd-1" && !strings.Contains(n.Message, "下载 images/e2e_app_1.0.0.tar 50%") {
			t.Errorf("节点 message 应回显进度尾行，实际 %q", n.Message)
		}
	}
}

// ---- 自动安装 Docker + 编排导出端到端：无 docker 主机 + dockerInstall → 安装节点派发契约、
// 安装源设置/离线介质端点鉴权、主机池一键装合成会话、会话导出（槽位化 + 敏感置空）----

func TestE2EDockerInstallAndExport(t *testing.T) {
	srv, ts := newTestServer(t)

	// 登录
	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// 安装源端点鉴权：匿名 401；保存自定义脚本地址
	if r := uiDo(t, "PUT", ts.URL+"/api/docker-source", "", []byte(`{}`)); r.StatusCode != 401 {
		t.Fatalf("匿名应 401，实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := uiDo(t, "PUT", ts.URL+"/api/docker-source", login.Token,
		[]byte(`{"scriptUrl":"https://example.com/get-docker.sh","mirrors":"https://m.example.com"}`)); r.StatusCode != 200 {
		t.Fatalf("保存安装源失败: %v", r.Status)
	} else {
		r.Body.Close()
	}

	// 离线介质上传（multipart；file 在前 version 在后——浏览器实序，顺序不敏感契约）与
	// agent 下载端点（无令牌 401 / 有令牌 200）
	mediaTar := append([]byte{0x1f, 0x8b}, []byte("fake-docker-media-tgz")...)
	var mbuf bytes.Buffer
	mbuf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"docker-27.tgz\"\r\n\r\n")
	mbuf.Write(mediaTar)
	mbuf.WriteString("\r\n--B\r\nContent-Disposition: form-data; name=\"version\"\r\n\r\nv27.5.1\r\n--B--\r\n")
	req, _ := http.NewRequest("POST", ts.URL+"/api/docker-source/media/linux-amd64", &mbuf)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	req.Header.Set("X-Console-Token", login.Token)
	if mresp, err := http.DefaultClient.Do(req); err != nil || mresp.StatusCode != 200 {
		t.Fatalf("介质上传失败: %v %v", err, mresp)
	} else {
		mresp.Body.Close()
	}
	// 缺 version 字段应 400（版本化介质库必填）
	var nbuf bytes.Buffer
	nbuf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.tgz\"\r\n\r\nxx\r\n--B--\r\n")
	reqN, _ := http.NewRequest("POST", ts.URL+"/api/docker-source/media/linux-arm64", &nbuf)
	reqN.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	reqN.Header.Set("X-Console-Token", login.Token)
	if r, err := http.DefaultClient.Do(reqN); err != nil || r.StatusCode != 400 {
		t.Fatalf("缺 version 应 400: %v %v", err, r)
	} else {
		r.Body.Close()
	}
	if r, _ := http.Get(ts.URL + "/api/agent/media/docker/linux/amd64"); r.StatusCode != 401 {
		t.Fatalf("无令牌介质下载应 401，实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 注册无 docker 的 Linux agent 主机 + 一次轮询（TouchHost 判在线）
	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-bare","os":"linux","arch":"amd64","docker":false,"ips":["10.9.9.8"]}}`, enroll)
	resp2, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody))
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("注册失败: %v %v", err, resp2)
	}
	var reg RegisterResp
	_ = json.NewDecoder(resp2.Body).Decode(&reg)
	resp2.Body.Close()

	poll := func(token string) *Task {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
		req.Header.Set("X-Agent-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pr PollResp
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Task
	}
	if tk := poll(reg.AgentToken); tk != nil {
		t.Fatalf("不应有任务，实际 %+v", tk)
	}

	// 介质下载（agent 令牌）：旧端点别名（服务最新版本）与新端点 ?v= 定版，内容一致
	req2, _ := http.NewRequest("GET", ts.URL+"/api/agent/docker-media/linux/amd64", nil)
	req2.Header.Set("X-Agent-Token", reg.AgentToken)
	if dresp, err := http.DefaultClient.Do(req2); err != nil || dresp.StatusCode != 200 {
		t.Fatalf("介质下载失败: %v %v", err, dresp)
	} else {
		data, _ := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		if !bytes.Equal(data, mediaTar) {
			t.Error("介质内容不一致")
		}
	}
	for _, u := range []string{
		ts.URL + "/api/agent/media/docker/linux/amd64?v=v27.5.1",
		ts.URL + "/api/agent/media/docker/linux/amd64", // v 缺省=最新
	} {
		req3, _ := http.NewRequest("GET", u, nil)
		req3.Header.Set("X-Agent-Token", reg.AgentToken)
		if dresp, err := http.DefaultClient.Do(req3); err != nil || dresp.StatusCode != 200 {
			t.Fatalf("统一介质端点下载失败: %v %v", err, dresp)
		} else {
			data, _ := io.ReadAll(dresp.Body)
			dresp.Body.Close()
			if !bytes.Equal(data, mediaTar) {
				t.Error("统一介质端点内容不一致")
			}
		}
	}
	req4, _ := http.NewRequest("GET", ts.URL+"/api/agent/media/docker/linux/amd64?v=9.9.9", nil)
	req4.Header.Set("X-Agent-Token", reg.AgentToken)
	if r, err := http.DefaultClient.Do(req4); err != nil || r.StatusCode != 404 {
		t.Fatalf("未上传版本应 404: %v %v", err, r)
	} else {
		r.Body.Close()
	}

	// 主机池一键装：合成单节点会话 → agent 领到 docker 任务 → 上报 ok → 会话 done
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/hosts/%d/install-docker", ts.URL, reg.HostID), login.Token, nil); r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		t.Fatalf("一键装失败: %s %s", r.Status, body)
	} else {
		r.Body.Close()
	}
	task := poll(reg.AgentToken)
	if task == nil || task.Kind != "docker" || !strings.HasPrefix(task.NodeID, "docker-install-") {
		t.Fatalf("一键装应派发 docker 任务，实际 %+v", task)
	}
	reportOK := func(sid string, node string) {
		body, _ := json.Marshal(Report{SessionID: sid, NodeID: node, Status: "ok"})
		req, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
			t.Fatalf("上报失败: %v %v", err, resp)
		} else {
			resp.Body.Close()
		}
	}
	reportOK(task.SessionID, task.NodeID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ss, _ := srv.store.SessionByID(mustAtoi(task.SessionID)); ss != nil && ss.Status == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ss, _ := srv.store.SessionByID(mustAtoi(task.SessionID)); ss == nil || ss.Status != "done" {
		t.Fatalf("一键装会话未完成: %+v", ss)
	}

	// 编排会话：app 落到无 docker 主机 + dockerInstall → 安装节点随行，agent 按序领取
	pkgID := uploadPkg(t, ts, login.Token)
	pl := Placement{Host: map[string]int64{"app": reg.HostID}, Mode: map[string]string{},
		Vars: map[string]string{"E2E_VAR": "hi"}, DockerInstall: true}
	sessBody, _ := json.Marshal(map[string]any{"packageId": pkgID, "placement": pl, "mode": "fresh"})
	resp3 := uiDo(t, "POST", ts.URL+"/api/sessions", login.Token, sessBody)
	var sessRes struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&sessRes)
	resp3.Body.Close()
	if sessRes.ID == 0 {
		t.Fatal("编排会话创建失败")
	}
	// 介质优先：首个 docker 任务载荷应带 media*（linux-amd64 已上传）与自定义脚本地址 + 介质版本
	dt := poll(reg.AgentToken)
	if dt == nil || dt.Kind != "docker" {
		t.Fatalf("应先派发安装任务，实际 %+v", dt)
	}
	var dp DockerPayload
	if err := json.Unmarshal(dt.Payload, &dp); err != nil {
		t.Fatal(err)
	}
	if dp.MediaName != "docker-27.tgz" || dp.MediaSHA256 == "" || dp.ScriptURL != "https://example.com/get-docker.sh" {
		t.Fatalf("安装载荷介质未优先: %+v", dp)
	}
	if dp.Version != "v27.5.1" {
		t.Fatalf("安装载荷应带介质版本 v27.5.1: %+v", dp)
	}
	if len(dp.Mirrors) != 1 {
		t.Fatalf("mirrors 未透传: %+v", dp)
	}
	reportOK(dt.SessionID, dt.NodeID)
	// 后续节点（load/compose/wait-entry）依次派发并推到终态
	for i := 0; i < 5; i++ {
		tk := poll(reg.AgentToken)
		if tk == nil {
			break
		}
		reportOK(tk.SessionID, tk.NodeID)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ss, _ := srv.store.SessionByID(sessRes.ID); ss != nil && ss.Status == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 会话导出：槽位化主机 + 变量保留 + 随机凭据置空登记
	eresp := uiDo(t, "GET", fmt.Sprintf("%s/api/sessions/%d/export", ts.URL, sessRes.ID), login.Token, nil)
	if eresp.StatusCode != 200 {
		t.Fatalf("导出失败: %v", eresp.Status)
	}
	ebody, _ := io.ReadAll(eresp.Body)
	eresp.Body.Close()
	var orch orchFile
	if err := json.Unmarshal(ebody, &orch); err != nil {
		t.Fatal(err)
	}
	if orch.Kind != orchKind || orch.Format != orchFormat || orch.Package.App != "e2e-app" {
		t.Fatalf("导出头异常: %+v", orch)
	}
	if cd := eresp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("缺 attachment 头: %q", cd)
	}
	slot := "h" + fmt.Sprint(reg.HostID)
	if orch.Placement.Host["app"] != slot || len(orch.Slots) == 0 || orch.Slots[0].ID != slot {
		t.Fatalf("槽位化异常: %+v / %v", orch.Placement.Host, orch.Slots)
	}
	if orch.Placement.Vars["E2E_VAR"] != "hi" {
		t.Errorf("普通变量应保留: %v", orch.Placement.Vars)
	}
	if orch.Placement.Vars["DB_PASSWORD"] != "" {
		t.Errorf("随机凭据应置空: %v", orch.Placement.Vars["DB_PASSWORD"])
	}
	if len(orch.Redacted) != 1 || orch.Redacted[0] != "DB_PASSWORD" {
		t.Errorf("redacted 登记: %v", orch.Redacted)
	}
	if !orch.DockerInstall {
		t.Error("dockerInstall 未随导出")
	}
	for _, n := range orch.Dag {
		if strings.HasPrefix(n.ID, "load-h") || strings.HasPrefix(n.ID, "compose-h") {
			if n.Host != slot {
				t.Errorf("dag 节点 %s 主机槽位异常: %q", n.ID, n.Host)
			}
		}
	}
}

func mustAtoi(s string) int64 {
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}

// ---- docker compose 维护端到端：compose 安装源保存/介质上传与 agent 下载、主机池一键装
// （版本指定/守卫矩阵：Windows 拒绝、未装 docker 拒绝、已是目标版本拒绝、指定 docker 版本无介质拒绝）----

func TestE2EComposeSourceAndInstall(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// compose 安装源：保存模板与缺省版本；GET 回显
	if r := uiDo(t, "PUT", ts.URL+"/api/compose-source", login.Token,
		[]byte(`{"template":"","version":"v2.30.3"}`)); r.StatusCode != 200 {
		t.Fatalf("保存 compose 源失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	if r := uiDo(t, "GET", ts.URL+"/api/compose-source", login.Token, nil); r.StatusCode != 200 {
		t.Fatal("读取 compose 源失败")
	} else {
		var src struct {
			Template string `json:"template"`
			Default  string `json:"default"`
			Version  string `json:"version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&src)
		r.Body.Close()
		if src.Template != "" || src.Default == "" || src.Version != "v2.30.3" {
			t.Fatalf("compose 源回显异常: %+v", src)
		}
	}

	// compose 介质上传（version 在前 file 在后——另一序，验证顺序不敏感）与 agent 下载
	composeBin := []byte("fake-compose-plugin-binary")
	var cbuf bytes.Buffer
	cbuf.WriteString("--B\r\nContent-Disposition: form-data; name=\"version\"\r\n\r\nv2.30.3\r\n")
	cbuf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"docker-compose\"\r\n\r\n")
	cbuf.Write(composeBin)
	cbuf.WriteString("\r\n--B--\r\n")
	creq, _ := http.NewRequest("POST", ts.URL+"/api/compose-source/media/linux-amd64", &cbuf)
	creq.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	creq.Header.Set("X-Console-Token", login.Token)
	if r, err := http.DefaultClient.Do(creq); err != nil || r.StatusCode != 200 {
		t.Fatalf("compose 介质上传失败: %v %v", err, r)
	} else {
		r.Body.Close()
	}
	mreq, _ := http.NewRequest("GET", ts.URL+"/api/agent/media/compose/linux/amd64?v=v2.30.3", nil)
	mreq.Header.Set("X-Agent-Token", "no-such-token")
	if r, err := http.DefaultClient.Do(mreq); err != nil || r.StatusCode != 401 {
		t.Fatalf("compose 介质无令牌应 401: %v %v", err, r)
	} else {
		r.Body.Close()
	}

	// 注册四台主机：linux+docker 缺 compose、linux+docker+compose 就绪、linux 裸机、windows
	enroll := srv.store.GetSetting(enrollKey, "")
	regHost := func(body string) RegisterResp {
		r2, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(body))
		if err != nil || r2.StatusCode != 200 {
			t.Fatalf("注册失败: %v %v", err, r2)
		}
		var rr RegisterResp
		_ = json.NewDecoder(r2.Body).Decode(&rr)
		r2.Body.Close()
		return rr
	}
	lin := regHost(fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-comp","os":"linux","arch":"amd64","docker":true,"dockerVersion":"27.1.0","compose":false,"ips":["10.9.9.7"]}}`, enroll))
	ready := regHost(fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-ready","os":"linux","arch":"amd64","docker":true,"dockerVersion":"27.1.0","compose":true,"composeVersion":"2.30.3","ips":["10.9.9.4"]}}`, enroll))
	bare := regHost(fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-bare2","os":"linux","arch":"amd64","docker":false,"ips":["10.9.9.5"]}}`, enroll))
	win := regHost(fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-win","os":"windows","arch":"amd64","docker":false,"ips":["10.9.9.6"]}}`, enroll))

	poll := func(token string) *Task {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
		req.Header.Set("X-Agent-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pr PollResp
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Task
	}
	if tk := poll(lin.AgentToken); tk != nil {
		t.Fatalf("lin 不应有任务，实际 %+v", tk)
	}

	expectErr := func(url string, wantSub string) {
		t.Helper()
		r := uiDo(t, "POST", url, login.Token, []byte(`{}`))
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode == 200 || !strings.Contains(string(body), wantSub) {
			t.Fatalf("%s 应拒绝（%s）: %d %s", url, wantSub, r.StatusCode, body)
		}
	}
	// Windows 主机：docker 与 compose 一键装均拒绝并明示预装指引；裸机装 compose 拒（先装 docker）
	expectErr(fmt.Sprintf("%s/api/hosts/%d/install-docker", ts.URL, win.HostID), "预装 Docker Desktop")
	expectErr(fmt.Sprintf("%s/api/hosts/%d/install-compose", ts.URL, win.HostID), "预装 Docker Desktop")
	expectErr(fmt.Sprintf("%s/api/hosts/%d/install-compose", ts.URL, bare.HostID), "先安装 Docker")

	// compose 介质下载（agent 令牌）内容一致
	dreq, _ := http.NewRequest("GET", ts.URL+"/api/agent/media/compose/linux/amd64?v=v2.30.3", nil)
	dreq.Header.Set("X-Agent-Token", lin.AgentToken)
	if r, err := http.DefaultClient.Do(dreq); err != nil || r.StatusCode != 200 {
		t.Fatalf("compose 介质下载失败: %v %v", err, r)
	} else {
		data, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !bytes.Equal(data, composeBin) {
			t.Error("compose 介质内容不一致")
		}
	}

	// 一键装 compose：缺省版本（最新介质 v2.30.3）→ agent 领到 toolkit 任务（载荷带介质版本）
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/hosts/%d/install-compose", ts.URL, lin.HostID), login.Token, nil); r.StatusCode != 200 {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		t.Fatalf("一键装 compose 失败: %s %s", r.Status, body)
	} else {
		r.Body.Close()
	}
	task := poll(lin.AgentToken)
	if task == nil || task.Kind != "toolkit" || !strings.HasPrefix(task.NodeID, "compose-install-") {
		t.Fatalf("一键装应派发 toolkit 任务，实际 %+v", task)
	}
	var tp ToolkitPayload
	if err := json.Unmarshal(task.Payload, &tp); err != nil {
		t.Fatal(err)
	}
	if tp.Tool != "compose" || tp.MediaVer != "v2.30.3" || tp.MediaSHA256 == "" {
		t.Fatalf("toolkit 载荷 = %+v", tp)
	}
	reportBody, _ := json.Marshal(Report{SessionID: task.SessionID, NodeID: task.NodeID, Status: "ok"})
	rreq, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(reportBody))
	rreq.Header.Set("Content-Type", "application/json")
	rreq.Header.Set("X-Agent-Token", lin.AgentToken)
	if r, err := http.DefaultClient.Do(rreq); err != nil || r.StatusCode != 200 {
		t.Fatalf("上报失败: %v %v", err, r)
	} else {
		r.Body.Close()
	}

	// 守卫矩阵：docker 已装不指版本拒重装 / docker 指定版本无介质拒 / compose 已是目标版本拒
	expectVerErr := func(url, ver string, wantSub string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"version": ver})
		r := uiDo(t, "POST", url, login.Token, body)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode == 200 || !strings.Contains(string(b), wantSub) {
			t.Fatalf("%s（%s）应拒绝（%s）: %d %s", url, ver, wantSub, r.StatusCode, b)
		}
	}
	expectErr(fmt.Sprintf("%s/api/hosts/%d/install-docker", ts.URL, lin.HostID), "已安装 docker")
	expectVerErr(fmt.Sprintf("%s/api/hosts/%d/install-docker", ts.URL, lin.HostID), "26.1.0", "需先上传对应离线介质")
	expectVerErr(fmt.Sprintf("%s/api/hosts/%d/install-compose", ts.URL, ready.HostID), "v2.30.3", "已是")
}

// TestE2EHostExecAndPresets 主机运维链路：远程执行（本机节点合成 exec 会话全链路）+
// 批量部分失败（离线远端报明确错误）+ 主机执行历史 + 快捷脚本库 CRUD。
func TestE2EHostExecAndPresets(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// 匿名执行应 401
	if r := uiDo(t, "POST", ts.URL+"/api/exec", "", []byte(`{"hostIds":[1],"script":"x"}`)); r.StatusCode != 401 {
		t.Fatalf("匿名 /api/exec 应 401，实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 快捷脚本库：内置清单 → 保存自定义 → 删自定义（内置不可删）
	if r := uiDo(t, "GET", ts.URL+"/api/exec-presets", login.Token, nil); r.StatusCode != 200 {
		t.Fatalf("presets 读取失败: %v", r.Status)
	} else {
		var pl struct {
			Builtin []execPreset `json:"builtin"`
		}
		_ = json.NewDecoder(r.Body).Decode(&pl)
		r.Body.Close()
		if len(pl.Builtin) == 0 {
			t.Fatal("内置快捷脚本不应为空")
		}
	}
	if r := uiDo(t, "POST", ts.URL+"/api/exec-presets", login.Token,
		[]byte(`{"name":"巡检","os":"linux","script":"df -h"}`)); r.StatusCode != 200 {
		t.Fatalf("保存自定义快捷脚本失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	var pl2 struct {
		Custom []execPreset `json:"custom"`
	}
	if r := uiDo(t, "GET", ts.URL+"/api/exec-presets", login.Token, nil); r.StatusCode != 200 {
		t.Fatal("presets 二次读取失败")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&pl2)
		r.Body.Close()
	}
	if len(pl2.Custom) != 1 || pl2.Custom[0].Name != "巡检" || !strings.HasPrefix(pl2.Custom[0].ID, "c") {
		t.Fatalf("自定义快捷脚本应已保存，实际 %+v", pl2.Custom)
	}
	if r := uiDo(t, "DELETE", ts.URL+"/api/exec-presets/b-docker-ps", login.Token, nil); r.StatusCode != 404 {
		t.Fatalf("内置快捷脚本不可删，应 404，实际 %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := uiDo(t, "DELETE", ts.URL+"/api/exec-presets/"+pl2.Custom[0].ID, login.Token, nil); r.StatusCode != 200 {
		t.Fatalf("删除自定义快捷脚本失败: %v", r.Status)
	} else {
		r.Body.Close()
	}

	// 本机主机 + 远端离线 agent 主机（注册后不轮询 → 判离线）
	srv.ensureLocalHost()
	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-offline","os":"linux","arch":"amd64","docker":false,"ips":["10.9.9.9"]}}`, enroll)
	if r, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody)); err != nil || r.StatusCode != 200 {
		t.Fatalf("注册失败: %v %v", err, r)
	} else {
		r.Body.Close()
	}
	var localID, remoteID int64
	hosts, _ := srv.store.ListHosts()
	for _, h := range hosts {
		if h.Local {
			localID = h.ID
		} else if h.Name == "e2e-offline" {
			remoteID = h.ID
		}
	}
	if localID == 0 || remoteID == 0 {
		t.Fatalf("主机准备失败 local=%d remote=%d", localID, remoteID)
	}

	// 空脚本 / 超时越界应 400
	for _, body := range []string{
		fmt.Sprintf(`{"hostIds":[%d],"script":"  "}`, localID),
		fmt.Sprintf(`{"hostIds":[%d],"script":"echo x","timeoutSec":5}`, localID),
	} {
		if r := uiDo(t, "POST", ts.URL+"/api/exec", login.Token, []byte(body)); r.StatusCode != 400 {
			t.Fatalf("非法请求应 400: %s → %d", body, r.StatusCode)
		} else {
			r.Body.Close()
		}
	}

	// 批量执行：本机成功 + 离线远端明确报错
	body := fmt.Sprintf(`{"hostIds":[%d,%d],"script":"echo exec-e2e-ok","timeoutSec":60,"title":"冒烟"}`, localID, remoteID)
	resp = uiDo(t, "POST", ts.URL+"/api/exec", login.Token, []byte(body))
	var ex struct {
		Items []struct {
			HostID    int64  `json:"hostId"`
			HostName  string `json:"hostName"`
			SessionID int64  `json:"sessionId"`
			Error     string `json:"error"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ex)
	resp.Body.Close()
	if len(ex.Items) != 2 {
		t.Fatalf("应返回 2 台结果，实际 %+v", ex.Items)
	}
	var sid int64
	for _, it := range ex.Items {
		if it.HostID == localID {
			if it.Error != "" || it.SessionID == 0 {
				t.Fatalf("本机执行应成功，实际 %+v", it)
			}
			sid = it.SessionID
		} else if it.Error == "" || !strings.Contains(it.Error, "离线") {
			t.Fatalf("离线主机应报错，实际 %+v", it)
		}
	}

	// 会话终态 done + 节点日志（node 过滤命中 / 未知名空）
	done := false
	for i := 0; i < 80; i++ {
		var d struct {
			Session struct {
				Status string `json:"status"`
			} `json:"session"`
		}
		r := uiDo(t, "GET", fmt.Sprintf("%s/api/sessions/%d", ts.URL, sid), login.Token, nil)
		_ = json.NewDecoder(r.Body).Decode(&d)
		r.Body.Close()
		if d.Session.Status == "done" || d.Session.Status == "failed" {
			if d.Session.Status != "done" {
				t.Fatalf("本机 exec 会话应 done，实际 %s", d.Session.Status)
			}
			done = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatal("exec 会话超时未终态")
	}
	var lg struct {
		Lines []logLine `json:"lines"`
	}
	if r := uiDo(t, "GET", fmt.Sprintf("%s/api/sessions/%d/logs?node=exec", ts.URL, sid), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("日志查询失败")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&lg)
		r.Body.Close()
	}
	joined := ""
	for _, l := range lg.Lines {
		joined += l.Line + "\n"
	}
	if !strings.Contains(joined, "exec-e2e-ok") {
		t.Fatalf("日志应含执行输出，实际:\n%s", joined)
	}
	if r := uiDo(t, "GET", fmt.Sprintf("%s/api/sessions/%d/logs?node=nope", ts.URL, sid), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("node 过滤查询失败")
	} else {
		var lg2 struct {
			Lines []logLine `json:"lines"`
		}
		_ = json.NewDecoder(r.Body).Decode(&lg2)
		r.Body.Close()
		if len(lg2.Lines) != 0 {
			t.Fatalf("未知名过滤应为空，实际 %d 行", len(lg2.Lines))
		}
	}

	// 主机执行历史：本机主机应含刚跑的 exec 会话（nodes 带本机节点行）
	var hs struct {
		Sessions []*hostSessionEntry `json:"sessions"`
	}
	if r := uiDo(t, "GET", fmt.Sprintf("%s/api/hosts/%d/sessions", ts.URL, localID), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("主机执行历史查询失败")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&hs)
		r.Body.Close()
	}
	found := false
	for _, e := range hs.Sessions {
		if e.ID == sid {
			found = true
			if len(e.Nodes) != 1 || e.Nodes[0].Key != "exec" {
				t.Fatalf("exec 会话应带 1 个本机节点，实际 %+v", e.Nodes)
			}
		}
	}
	if !found {
		t.Fatal("主机执行历史应含 exec 会话")
	}
}

// TestE2ETerminalFlow 主机终端链路：一个终端窗口=一条会话（App=terminal），逐命令追加节点；
// 中断指令经 report 响应 {"cancel":true} 下发；close 收尾（未终态落 failed、会话终态）。
func TestE2ETerminalFlow(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// 注册远端 agent 主机并 poll 一次（判在线）
	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-term","os":"linux","arch":"amd64","docker":false,"ips":["10.9.9.7"]}}`, enroll)
	if r, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody)); err != nil || r.StatusCode != 200 {
		t.Fatalf("注册失败: %v %v", err, r)
	} else {
		r.Body.Close()
	}
	var reg RegisterResp
	rr, _ := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody))
	_ = json.NewDecoder(rr.Body).Decode(&reg)
	rr.Body.Close()

	poll := func() *Task {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pr PollResp
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Task
	}
	if tk := poll(); tk != nil {
		t.Fatalf("不应有任务，实际 %+v", tk)
	}

	// 开终端（一个窗口=一条会话）→ 跑第一条命令（模拟 agent 领任务 + 上报，响应应无 cancel）
	var term struct {
		ID int64 `json:"id"`
	}
	r := uiDo(t, "POST", ts.URL+"/api/terminal/open", login.Token, []byte(fmt.Sprintf(`{"hostId":%d}`, reg.HostID)))
	if r.StatusCode != 200 {
		t.Fatalf("开终端失败: %v", r.Status)
	}
	_ = json.NewDecoder(r.Body).Decode(&term)
	r.Body.Close()
	if term.ID == 0 {
		t.Fatal("未返回终端会话 id")
	}
	sid := term.ID

	report := func(node, status, msg string, logs []string) bool {
		body, _ := json.Marshal(Report{SessionID: fmt.Sprint(sid), NodeID: node, Status: status, Message: msg, Logs: logs})
		req, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(body))
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Cancel bool `json:"cancel"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ack)
		resp.Body.Close()
		return ack.Cancel
	}

	// 第一条命令：正常完成
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, sid), login.Token,
		[]byte(`{"script":"echo term-e2e-ok"}`)); r.StatusCode != 200 {
		t.Fatalf("run 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	tk := poll()
	if tk == nil || tk.Kind != "cmd" || tk.NodeID != "cmd-1" || tk.SessionID != fmt.Sprint(sid) {
		t.Fatalf("应领到 cmd-1 任务，实际 %+v", tk)
	}
	if report("cmd-1", "running", "", []string{"$ echo term-e2e-ok"}) {
		t.Fatal("未请求中断时响应不应携带 cancel")
	}
	if report("cmd-1", "ok", "", []string{"term-e2e-ok"}) {
		t.Fatal("ok 响应不应携带 cancel")
	}

	// 第二条命令（模拟 docker logs -f 长命令）：stop 后 report 响应必须带回 cancel
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, sid), login.Token,
		[]byte(`{"script":"docker logs -f c1"}`)); r.StatusCode != 200 {
		t.Fatalf("run2 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	if tk := poll(); tk == nil || tk.NodeID != "cmd-2" {
		t.Fatalf("应领到 cmd-2 任务，实际 %+v", tk)
	}
	if report("cmd-2", "running", "", nil) {
		t.Fatal("stop 前响应不应携带 cancel")
	}
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/stop", ts.URL, sid), login.Token,
		[]byte(`{"node":"cmd-2"}`)); r.StatusCode != 200 {
		t.Fatalf("stop 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	if !report("cmd-2", "running", "", nil) {
		t.Fatal("stop 后 report 响应应携带 cancel:true（中断指令通道）")
	}
	report("cmd-2", "failed", "已人工中断", nil)

	// state：两节点终态、closed=false；close：会话收尾（有失败节点 → failed）
	var st struct {
		Session struct {
			Status string `json:"status"`
		} `json:"session"`
		Nodes  []nodeRow `json:"nodes"`
		Closed bool      `json:"closed"`
	}
	if r := uiDo(t, "GET", fmt.Sprintf("%s/api/terminal/%d/state", ts.URL, sid), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("state 查询失败")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
	}
	if st.Closed || st.Session.Status != "running" || len(st.Nodes) != 2 ||
		st.Nodes[0].Status != "ok" || st.Nodes[1].Status != "failed" {
		t.Fatalf("state 异常: closed=%v session=%s nodes=%+v", st.Closed, st.Session.Status, st.Nodes)
	}
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/close", ts.URL, sid), login.Token, nil); r.StatusCode != 200 {
		t.Fatalf("close 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	// close 幂等 + 收尾后再 report 只留日志不翻状态
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/close", ts.URL, sid), login.Token, nil); r.StatusCode != 200 {
		t.Fatalf("close 应幂等: %v", r.Status)
	} else {
		r.Body.Close()
	}
	report("cmd-2", "ok", "迟到结果", nil) // 不应把 failed 翻成 ok
	if ss, err := srv.store.SessionByID(sid); err != nil || ss.Status != "failed" {
		t.Fatalf("close 后会话应 failed，实际 %+v %v", ss, err)
	}
	if n, _ := srv.store.NodeStatus(sid, "cmd-2"); n != "failed" {
		t.Fatalf("迟到 ok 不应翻转节点状态，实际 %s", n)
	}
	// 日志断言（node 过滤）
	lines, _ := srv.store.LogsAfter(sid, 0, 100, "cmd-1")
	joined := ""
	for _, l := range lines {
		joined += l.Line + "\n"
	}
	if !strings.Contains(joined, "term-e2e-ok") {
		t.Fatalf("cmd-1 日志应含输出，实际:\n%s", joined)
	}

	// 本机终端：open → run（进程内 localCmd）→ 状态终态 → close
	srv.ensureLocalHost()
	hosts, _ := srv.store.ListHosts()
	var localID int64
	for _, h := range hosts {
		if h.Local {
			localID = h.ID
		}
	}
	r = uiDo(t, "POST", ts.URL+"/api/terminal/open", login.Token, []byte(fmt.Sprintf(`{"hostId":%d}`, localID)))
	_ = json.NewDecoder(r.Body).Decode(&term)
	r.Body.Close()
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, term.ID), login.Token,
		[]byte(`{"script":"echo term-local-ok"}`)); r.StatusCode != 200 {
		t.Fatalf("本机 run 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	done := false
	for i := 0; i < 80; i++ {
		if n, _ := srv.store.NodeStatus(term.ID, "cmd-1"); n == "ok" || n == "failed" {
			if n != "ok" {
				t.Fatal("本机终端命令应 ok")
			}
			done = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatal("本机终端命令超时未终态")
	}
	lines, _ = srv.store.LogsAfter(term.ID, 0, 100, "cmd-1")
	joined = ""
	for _, l := range lines {
		joined += l.Line + "\n"
	}
	if !strings.Contains(joined, "term-local-ok") {
		t.Fatalf("本机命令日志应含输出，实际:\n%s", joined)
	}
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/close", ts.URL, term.ID), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("本机 close 失败")
	} else {
		r.Body.Close()
	}
	if ss, err := srv.store.SessionByID(term.ID); err != nil || ss.Status != "done" {
		t.Fatalf("本机关闭后会话应 done，实际 %+v %v", ss, err)
	}
}

// TestE2ETerminalStopStaleAgent 旧版 agent 的停止口径：stop 响应附 stale+advice（终端弹明确指引）、
// cancel 标记照置（agent 若已换代即生效）、命令超时压到 300s
func TestE2ETerminalStopStaleAgent(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// 注入内嵌版本戳，构造「目标机 agent 落后」
	oldStamp := agentStamp
	agentStamp = "209912312359"
	defer func() { agentStamp = oldStamp }()

	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"e2e-stale","os":"linux","arch":"amd64","agentVersion":"202601010000","ips":["10.9.9.6"]}}`, enroll)
	var reg RegisterResp
	if r, err := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody)); err != nil || r.StatusCode != 200 {
		t.Fatalf("注册失败: %v %v", err, r)
	} else {
		_ = json.NewDecoder(r.Body).Decode(&reg)
		r.Body.Close()
	}
	if req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil); true {
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		if pr, err := http.DefaultClient.Do(req); err != nil {
			t.Fatal(err)
		} else {
			pr.Body.Close()
		}
	}

	// 开终端 → 跑命令：领到的任务超时应为 300（旧 agent 压短防 -f 长挂）
	var term struct {
		ID int64 `json:"id"`
	}
	r := uiDo(t, "POST", ts.URL+"/api/terminal/open", login.Token, []byte(fmt.Sprintf(`{"hostId":%d}`, reg.HostID)))
	if r.StatusCode != 200 {
		t.Fatalf("开终端失败: %v", r.Status)
	}
	_ = json.NewDecoder(r.Body).Decode(&term)
	r.Body.Close()

	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, term.ID), login.Token,
		[]byte(`{"script":"echo stale"}`)); r.StatusCode != 200 {
		t.Fatalf("run 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
	req.Header.Set("X-Agent-Token", reg.AgentToken)
	presp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var pr PollResp
	_ = json.NewDecoder(presp.Body).Decode(&pr)
	presp.Body.Close()
	if pr.Task == nil || pr.Task.Kind != "cmd" {
		t.Fatalf("应领到 cmd 任务，实际 %+v", pr.Task)
	}
	var cp CmdPayload
	if err := json.Unmarshal(pr.Task.Payload, &cp); err != nil || cp.TimeoutSec != termStaleAgentTimeoutSec {
		t.Fatalf("旧 agent 任务超时应压到 %d，实际 payload=%s err=%v", termStaleAgentTimeoutSec, pr.Task.Payload, err)
	}

	// report running（无 cancel）→ stop 应答 stale+advice → cancel 标记照置（再 report 带 cancel:true）
	report := func(status string) bool {
		body, _ := json.Marshal(Report{SessionID: fmt.Sprint(term.ID), NodeID: pr.Task.NodeID, Status: status})
		req, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(body))
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Cancel bool `json:"cancel"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ack)
		resp.Body.Close()
		return ack.Cancel
	}
	if report("running") {
		t.Fatal("stop 前响应不应携带 cancel")
	}
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/stop", ts.URL, term.ID), login.Token,
		[]byte(fmt.Sprintf(`{"node":%q}`, pr.Task.NodeID))); r.StatusCode != 200 {
		t.Fatalf("stop 失败: %v", r.Status)
	} else {
		var res struct {
			Stale  bool   `json:"stale"`
			Advice string `json:"advice"`
		}
		_ = json.NewDecoder(r.Body).Decode(&res)
		r.Body.Close()
		if !res.Stale || res.Advice == "" {
			t.Fatalf("旧 agent 的 stop 应带 stale+advice，实际 %+v", res)
		}
	}
	if !report("running") {
		t.Fatal("stop 后 cancel 标记应照置（report 响应携带 cancel:true）")
	}
	report("failed")
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/close", ts.URL, term.ID), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("close 失败")
	} else {
		r.Body.Close()
	}
}

// TestE2ETerminalCwdMemory 终端会话工作目录记忆：cd 后哨兵回传（日志不可见）、state.cwd 更新、
// 后续命令在记忆目录执行（cd 跨命令生效）
func TestE2ETerminalCwdMemory(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	srv.ensureLocalHost()
	hosts, _ := srv.store.ListHosts()
	var localID int64
	for _, h := range hosts {
		if h.Local {
			localID = h.ID
		}
	}
	var term struct {
		ID int64 `json:"id"`
	}
	r := uiDo(t, "POST", ts.URL+"/api/terminal/open", login.Token, []byte(fmt.Sprintf(`{"hostId":%d}`, localID)))
	if r.StatusCode != 200 {
		t.Fatalf("开终端失败: %v", r.Status)
	}
	_ = json.NewDecoder(r.Body).Decode(&term)
	r.Body.Close()

	dir := t.TempDir()
	cdScript := "cd " + strconv.Quote(dir)
	pwdScript := "pwd"
	if runtime.GOOS == "windows" {
		pwdScript = "Write-Output $PWD.ProviderPath"
	}
	waitNode := func(key string) string {
		for i := 0; i < 80; i++ {
			if st, _ := srv.store.NodeStatus(term.ID, key); st == "ok" || st == "failed" {
				return st
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("节点 %s 超时未终态", key)
		return ""
	}
	// ① cd <dir>：完成且日志不含哨兵、state.cwd 已记忆
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, term.ID), login.Token,
		[]byte(fmt.Sprintf(`{"script":%q}`, cdScript))); r.StatusCode != 200 {
		t.Fatalf("run cd 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	if st := waitNode("cmd-1"); st != "ok" {
		t.Fatal("cd 命令应 ok")
	}
	lines, _ := srv.store.LogsAfter(term.ID, 0, 100, "cmd-1")
	for _, l := range lines {
		if strings.Contains(l.Line, "CWD:") || strings.ContainsRune(l.Line, 1) {
			t.Fatalf("哨兵行不应落库，实际: %q", l.Line)
		}
	}
	var st struct {
		Cwd string `json:"cwd"`
	}
	if r := uiDo(t, "GET", fmt.Sprintf("%s/api/terminal/%d/state", ts.URL, term.ID), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("state 查询失败")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
	}
	if !strings.EqualFold(st.Cwd, dir) {
		t.Fatalf("state.cwd 应记忆 %s，实际 %q", dir, st.Cwd)
	}
	// ② 后续命令在记忆目录执行
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/run", ts.URL, term.ID), login.Token,
		[]byte(fmt.Sprintf(`{"script":%q}`, pwdScript))); r.StatusCode != 200 {
		t.Fatalf("run pwd 失败: %v", r.Status)
	} else {
		r.Body.Close()
	}
	if st2 := waitNode("cmd-2"); st2 != "ok" {
		t.Fatal("pwd 命令应 ok")
	}
	lines, _ = srv.store.LogsAfter(term.ID, 0, 100, "cmd-2")
	joined := ""
	for _, l := range lines {
		joined += l.Line + "\n"
	}
	if !strings.Contains(strings.ToLower(joined), strings.ToLower(dir)) {
		t.Fatalf("pwd 应在记忆目录 %s 下执行，实际:\n%s", dir, joined)
	}
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/terminal/%d/close", ts.URL, term.ID), login.Token, nil); r.StatusCode != 200 {
		t.Fatal("close 失败")
	} else {
		r.Body.Close()
	}
}

func TestE2ESessionAbort(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := uiDo(t, "POST", ts.URL+"/api/login", "", []byte(`{"password":"e2e-pass"}`))
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()

	// agent 主机注册（档案 docker=true 无容器清单——端口预检降级放行，聚焦中止链路）
	enroll := srv.store.GetSetting(enrollKey, "")
	regBody := fmt.Sprintf(`{"enrollToken":%q,"host":{"hostname":"abort-node","os":"linux","docker":true,"ips":["10.9.9.9"]}}`, enroll)
	resp2, _ := http.Post(ts.URL+"/api/agent/register", "application/json", strings.NewReader(regBody))
	var reg RegisterResp
	_ = json.NewDecoder(resp2.Body).Decode(&reg)
	resp2.Body.Close()
	if reg.AgentToken == "" || reg.HostID == 0 {
		t.Fatal("agent 注册失败")
	}

	pkgID := uploadPkg(t, ts, login.Token)
	pl := Placement{Host: map[string]int64{}, Mode: map[string]string{}, Vars: map[string]string{"E2E_VAR": "hi"}}
	dag := []DagNode{
		{ID: "cmd-a", Kind: "cmd", Host: reg.HostID, Script: "sleep 60"},
		{ID: "cmd-b", Kind: "cmd", Host: reg.HostID, Script: "echo b"},
	}
	sessBody, _ := json.Marshal(map[string]any{"packageId": pkgID, "placement": pl, "dag": dag, "mode": "fresh"})
	resp4 := uiDo(t, "POST", ts.URL+"/api/sessions", login.Token, sessBody)
	var sessRes struct {
		ID    int64  `json:"id"`
		Error string `json:"error"`
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	_ = json.Unmarshal(body4, &sessRes)
	if sessRes.ID == 0 {
		t.Fatalf("会话创建失败: %s", string(body4))
	}

	poll := func() *Task {
		req, _ := http.NewRequest("GET", ts.URL+"/api/agent/poll?wait=1", nil)
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pr PollResp
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Task
	}
	reportAck := func(node, status string) bool {
		body, _ := json.Marshal(Report{SessionID: fmt.Sprint(sessRes.ID), NodeID: node, Status: status})
		req, _ := http.NewRequest("POST", ts.URL+"/api/agent/report", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("上报失败 %s: %v %v", node, err, resp)
		}
		var ack struct {
			Cancel bool `json:"cancel"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ack)
		resp.Body.Close()
		return ack.Cancel
	}

	// ① agent 领走 cmd-a（cmd-b 滞留队列）；上报 running
	task := poll()
	if task == nil || task.NodeID != "cmd-a" {
		t.Fatalf("应领到 cmd-a，实际 %+v", task)
	}
	if reportAck("cmd-a", "running") {
		t.Fatal("未中止时上报不应收到 cancel")
	}
	// ② 用户中止：在途与滞留节点全部收尾，会话转 aborted
	rAbort := uiDo(t, "POST", fmt.Sprintf("%s/api/sessions/%d/abort", ts.URL, sessRes.ID), login.Token, nil)
	abortBody, _ := io.ReadAll(rAbort.Body)
	rAbort.Body.Close()
	if rAbort.StatusCode != 200 {
		t.Fatalf("中止失败: %v %s", rAbort.Status, string(abortBody))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ss, _ := srv.store.SessionByID(sessRes.ID); ss != nil && ss.Status == "aborted" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ss, _ := srv.store.SessionByID(sessRes.ID); ss == nil || ss.Status != "aborted" {
		t.Fatalf("会话应转 aborted: %+v", ss)
	}
	nodes, _ := srv.store.SessionNodes(sessRes.ID)
	for _, n := range nodes {
		if n.Status != "failed" || !strings.Contains(n.Message, "已中止") {
			t.Errorf("节点 %s 应为已中止收尾，实际 %s/%s", n.Key, n.Status, n.Message)
		}
	}
	// ③ 队列滞留任务丢弃：cmd-b 不再派发
	if t2 := poll(); t2 != nil {
		t.Fatalf("中止后滞留任务应被丢弃，实际 %+v", t2)
	}
	// ④ 在途任务上报收到 cancel（agent 杀进程组的指令通道）
	if !reportAck("cmd-a", "running") {
		t.Fatal("中止后在途任务上报应收到 cancel")
	}
	// ⑤ 已终结会话再中止 → 409
	if r := uiDo(t, "POST", fmt.Sprintf("%s/api/sessions/%d/abort", ts.URL, sessRes.ID), login.Token, nil); r.StatusCode != 409 {
		t.Fatalf("已终结会话中止应 409，实际 %s", r.Status)
	} else {
		r.Body.Close()
	}
}
