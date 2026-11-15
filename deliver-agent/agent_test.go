package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSafeRelPath(t *testing.T) {
	ok := map[string]string{
		"images/mysql_8.4.tar": "images/mysql_8.4.tar",
		"artifacts/app.jar":    "artifacts/app.jar",
		"delivery/vars.env":    "delivery/vars.env",
		"./conf/app.yaml":      "conf/app.yaml",
	}
	for in, want := range ok {
		got, err := safeRelPath(in)
		if err != nil || got != want {
			t.Errorf("safeRelPath(%q) = %q, %v；want %q", in, got, err, want)
		}
	}
	bad := []string{"../escape.tar", "..", "/etc/passwd", `C:\windows\system32`, "a/../../b", ""}
	for _, in := range bad {
		if _, err := safeRelPath(in); err == nil {
			t.Errorf("safeRelPath(%q) 应拒绝，却通过了", in)
		}
	}
}

func TestAgentStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &agentState{Server: "http://127.0.0.1:7870", AgentToken: "tok", HostID: 3, Hostname: "node-a"}
	if err := saveState(dir, st); err != nil {
		t.Fatal(err)
	}
	// 权限校验（非 Windows）
	if fi, err := os.Stat(filepath.Join(dir, "agent.json")); err == nil && fi.Mode().Perm() != 0600 {
		if filepath.Separator != '\\' {
			t.Errorf("agent.json 权限 = %v，want 0600", fi.Mode().Perm())
		}
	}
	got := loadState(dir)
	if *got != *st {
		t.Errorf("round trip 不一致: %+v", got)
	}
}

// 任务载荷 JSON 契约快照（与 console 侧镜像同构，字段改名会在此处暴露）
func TestTaskPayloadContract(t *testing.T) {
	raw := `{"sessionId":"s1","nodeId":"n1","kind":"compose","payload":{` +
		`"app":"mse","project":"mse-h1","compose":"services:\n  app:\n    image: mse/app:1.0\n",` +
		`"env":"DB_PASSWORD=x\n","files":{"conf/application.yaml":"mse: {}\n"}}}`
	var task Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatal(err)
	}
	if task.Kind != "compose" {
		t.Fatalf("kind = %q", task.Kind)
	}
	var p ComposePayload
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Project != "mse-h1" || p.Compose == "" || p.Env == "" || len(p.Files) != 1 {
		t.Errorf("compose 载荷解析异常: %+v", p)
	}
	sys := `{"app":"mse","unit":"mse-app.service","unitFile":"ExecStart=java -jar {{APP_DIR}}/artifacts/app.jar","configFiles":{"conf/application.yaml":"a: 1\n"}}`
	var sp SystemdPayload
	if err := json.Unmarshal([]byte(sys), &sp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sp.UnitFile, "{{APP_DIR}}") {
		t.Error("unitFile 应支持 {{APP_DIR}} 占位")
	}
}

func TestHTTPWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if ok, _ := httpOK(srv.URL + "/good"); !ok {
		t.Error("200 应判健康")
	}
	if ok, detail := httpOK(srv.URL + "/bad"); ok {
		t.Error("500 应判不健康")
	} else if detail != "HTTP 500" {
		t.Errorf("detail = %q", detail)
	}
	if ok, _ := httpOK(srv.URL[:len(srv.URL)-4] + ":1/no"); ok {
		t.Error("连接失败应判不健康")
	}
}

func TestEnsureDataDirs(t *testing.T) {
	base := t.TempDir()
	ensureDataDirs(base, "services:\n  mysql:\n    volumes:\n      - ./data/mysql:/var/lib/mysql\n      - ./data/redis:/data\n")
	for _, d := range []string{"data/mysql", "data/redis", "data/conf", "data/logs"} {
		if fi, err := os.Stat(filepath.Join(base, filepath.FromSlash(d))); err != nil || !fi.IsDir() {
			t.Errorf("目录 %s 未创建", d)
		}
	}
}

func TestHumanSize(t *testing.T) {
	if humanSize(5) != "5B" || humanSize(2048) != "2.0KB" {
		t.Errorf("humanSize 异常: %s / %s", humanSize(5), humanSize(2048))
	}
}

// 慢终端态保护：reporter 对失败上报的重试不应阻塞测试太久（这里只验证立即失败路径不 panic）
func TestReporterAppend(t *testing.T) {
	a := &agent{server: "http://127.0.0.1:1", state: &agentState{}, api: &http.Client{Timeout: 200 * time.Millisecond}, dl: &http.Client{}}
	r := newReporter(a, &Task{SessionID: "s", NodeID: "n"})
	r.logf("行1")
	r.append("行2", "行3")
	r.mu.Lock()
	n := len(r.lines)
	r.mu.Unlock()
	if n != 3 {
		t.Errorf("行数 = %d，want 3", n)
	}
}

// TestExecCmdInterruptByConsole 终端中断链路：keepalive 周期 running 上报 → console 在响应中
// 下达 {"cancel":true} → execCmd 杀进程并以「已人工中断」失败终态（不等超时）
func TestExecCmdInterruptByConsole(t *testing.T) {
	running := 0
	var finalMsg string
	var finalStatus string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/report" {
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var rep Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		switch rep.Status {
		case "running":
			running++
			if running >= 2 { // 首个 running 之后（keepalive 第二拍）下达中断
				w.Write([]byte(`{"cancel":true}`))
				return
			}
			w.Write([]byte(`{"ok":true}`))
		default:
			finalStatus, finalMsg = rep.Status, rep.Message
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer ts.Close()

	a := &agent{server: ts.URL, dataDir: t.TempDir(), api: &http.Client{Timeout: 5 * time.Second}}
	script := "sleep 30 && echo never"
	if runtime.GOOS == "windows" {
		script = "Start-Sleep -Seconds 30; Write-Output never"
	}
	payload, _ := json.Marshal(CmdPayload{Script: script, TimeoutSec: 120})
	start := time.Now()
	tk := &Task{SessionID: "1", NodeID: "cmd-1", Kind: "cmd", Payload: payload}
	done := make(chan struct{})
	go func() { a.execute(tk); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("中断未生效：命令 20 秒仍未结束")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("应在中断后数秒内结束，实际 %v", el)
	}
	if finalStatus != "failed" || !strings.Contains(finalMsg, "人工中断") {
		t.Fatalf("终态应为 failed/已人工中断，实际 %s/%q", finalStatus, finalMsg)
	}
}

// TestExecCmdTimeoutKillsTree 超时整组终止：短超时 + 带子进程的长命令（子进程握住 stdout 管道），
// 必须按时收尾并报「执行超时」——若只杀主进程不杀树/组，孤儿子进程会令收尾永久挂起
func TestExecCmdTimeoutKillsTree(t *testing.T) {
	var finalStatus, finalMsg string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		if rep.Status != "running" {
			finalStatus, finalMsg = rep.Status, rep.Message
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	a := &agent{server: ts.URL, dataDir: t.TempDir(), api: &http.Client{Timeout: 5 * time.Second}}
	script := "sleep 60 | cat" // POSIX：管道对端子进程握住 stdout
	if runtime.GOOS == "windows" {
		script = "cmd /c ping -n 60 127.0.0.1" // powershell→cmd→ping 三层树，ping 持续写 stdout
	}
	payload, _ := json.Marshal(CmdPayload{Script: script, TimeoutSec: 3})
	tk := &Task{SessionID: "1", NodeID: "cmd-t", Kind: "cmd", Payload: payload}
	start := time.Now()
	done := make(chan struct{})
	go func() { a.execute(tk); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("超时未整树终止：命令 30 秒仍未收尾（孤儿子进程握管道）")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("应在超时（3s）后数秒内收尾，实际 %v", el)
	}
	if finalStatus != "failed" || !strings.Contains(finalMsg, "执行超时") {
		t.Fatalf("终态应为 failed/执行超时，实际 %s/%q", finalStatus, finalMsg)
	}
}

// TestExecCmdCwd 指定工作目录落地：CmdPayload.Cwd 直接作为进程 cwd（终端会话记忆 cd 的落点）
func TestExecCmdCwd(t *testing.T) {
	var out []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		out = append(out, rep.Logs...)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	dir := t.TempDir()
	a := &agent{server: ts.URL, dataDir: t.TempDir(), api: &http.Client{Timeout: 5 * time.Second}}
	script := "pwd"
	if runtime.GOOS == "windows" {
		script = "Write-Output $PWD.ProviderPath"
	}
	payload, _ := json.Marshal(CmdPayload{Script: script, TimeoutSec: 60, Cwd: dir})
	a.execute(&Task{SessionID: "1", NodeID: "cmd-c", Kind: "cmd", Payload: payload})
	joined := strings.Join(out, "\n")
	if !strings.Contains(strings.ToLower(joined), strings.ToLower(dir)) {
		t.Fatalf("应在指定目录执行（%s），实际输出:\n%s", dir, joined)
	}
}
