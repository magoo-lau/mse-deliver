// deliver-console：安装控制台（mse-deliver 交付工具族「安装」段主程序）。
//
// 形态：单文件二进制（go:embed 内嵌 Vue 前端 + 双平台 deliver-agent），SQLite 持久化。
// 职责：部署包（format:3）上传解析 → 主机池（agent 注册/本机节点）→ 图形化编排（分配画布 +
// 自由 DAG 编辑器）→ 会话执行（拓扑调度/增量安装/人工干预）→ 入口可达确认。
//
// 启动：deliver-console [-addr 0.0.0.0:7870] [-root ./console-data] [-reset-pass]
// 首启打印：admin 随机密码 + agent 引导令牌（均可界面修改/轮换）。
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type Server struct {
	store *Store
	auth  *Auth
	root  string

	pluginDirOverride string // 插件目录覆盖（测试/插件开发：非空时优先取该目录下的插件二进制）

	mu     sync.Mutex
	queues map[int64]chan *Task
	sess   map[int64]*sessionRun

	pendingAgentUpgrade map[int64]bool         // 手动「升级 agent」待下发标记（poll 注入即生效，版本翻绿即清位；内存态同 queues，console 重启丢失可重点）
	terminals           map[int64]*terminalRun // 活跃主机终端会话（terminal.go；心跳超时自动收尾，重启由 FinalizeStaleTerminals 兜底）
}

func main() {
	addr := flag.String("addr", "0.0.0.0:7870", "监听地址（agent 需可达，勿用 127.0.0.1）")
	root := flag.String("root", "", "数据目录（缺省 ./console-data）")
	resetPass := flag.Bool("reset-pass", false, "重置管理员密码（生成新随机密码打印后继续启动）")
	flag.Parse()

	dataRoot := *root
	if dataRoot == "" {
		if exe, err := os.Executable(); err == nil {
			dataRoot = filepath.Join(filepath.Dir(exe), "console-data")
		} else {
			dataRoot = "console-data"
		}
	}
	store, err := OpenStore(dataRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "存储初始化失败:", err)
		os.Exit(1)
	}
	defer store.Close()
	auth := NewAuth(store)
	srv := &Server{store: store, auth: auth, root: dataRoot,
		queues: map[int64]chan *Task{}, sess: map[int64]*sessionRun{},
		pendingAgentUpgrade: map[int64]bool{}, terminals: map[int64]*terminalRun{}}

	srv.ensureLocalHost()
	store.FinalizeStaleTerminals()
	srv.resumeSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", auth.handleLogin)
	mux.HandleFunc("/api/logout", auth.handleLogout)
	mux.HandleFunc("/api/whoami", auth.handleWhoami)
	mux.HandleFunc("/api/settings/password", auth.handlePasswordChange)
	mux.HandleFunc("/api/settings/agent-upgrade", srv.handleAgentUpgradeSetting)
	mux.HandleFunc("/api/hosts", srv.handleHosts)
	mux.HandleFunc("/api/hosts/enroll-rotate", srv.handleEnrollRotate)
	mux.HandleFunc("/api/hosts/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/hosts/"), "/")
		id, action, ok := strings.Cut(rest, "/")
		hostID, _ := parseID(id)
		if !ok || hostID == 0 {
			writeErr(w, 404, "not found")
			return
		}
		switch action {
		case "revoke":
			srv.handleHostRevoke(w, r, hostID)
		case "delete":
			srv.handleHostDelete(w, r, hostID)
		case "install-docker":
			srv.handleHostInstallDocker(w, r, hostID)
		case "install-compose":
			srv.handleHostInstallCompose(w, r, hostID)
		case "sessions":
			srv.handleHostSessions(w, r, hostID)
		case "update", "":
			srv.handleHostUpdate(w, r, hostID)
		case "upgrade-agent":
			srv.handleHostUpgradeAgent(w, r, hostID)
		default:
			writeErr(w, 404, "not found")
		}
	})
	mux.HandleFunc("/api/exec", srv.handleExec)
	mux.HandleFunc("/api/exec-presets", srv.handleExecPresets)
	mux.HandleFunc("/api/exec-presets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/exec-presets/"), "/")
		if id != "" && r.Method == http.MethodDelete {
			srv.handleExecPresetDelete(w, r, id)
			return
		}
		writeErr(w, 404, "not found")
	})
	mux.HandleFunc("/api/terminal/open", srv.handleTerminalOpen)
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
			srv.handleTerminalRun(w, r, sid)
		case "stop":
			srv.handleTerminalStop(w, r, sid)
		case "state":
			srv.handleTerminalState(w, r, sid)
		case "close":
			srv.handleTerminalClose(w, r, sid)
		default:
			writeErr(w, 404, "not found")
		}
	})
	mux.HandleFunc("/api/agent/register", srv.handleAgentRegister)
	mux.HandleFunc("/api/agent/poll", srv.handleAgentPoll)
	mux.HandleFunc("/api/agent/report", srv.handleAgentReport)
	mux.HandleFunc("/api/agent/heartbeat", srv.handleAgentHeartbeat)
	mux.HandleFunc("/api/agent/media/", srv.handleAgentMedia)
	mux.HandleFunc("/api/agent/docker-media/", srv.handleAgentDockerMedia) // 旧端点别名（服务最新版本介质，旧 agent 兼容）
	mux.HandleFunc("/api/agent/binary/", srv.handleAgentBinary)
	mux.HandleFunc("/api/file/", srv.handleAgentFile)
	mux.HandleFunc("/api/plugin/", srv.handlePluginFile)
	mux.HandleFunc("/api/docker-source", srv.handleDockerSource)
	mux.HandleFunc("/api/compose-source", srv.handleComposeSource)
	mux.HandleFunc("/api/docker-source/media/", mediaRoute(srv, mediaToolDocker))
	mux.HandleFunc("/api/compose-source/media/", mediaRoute(srv, mediaToolCompose))
	mux.HandleFunc("/api/apps", srv.handleApps)
	mux.HandleFunc("/api/apps/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/apps/"), "/")
		app, action, _ := strings.Cut(rest, "/")
		if app == "" {
			writeErr(w, 404, "not found")
			return
		}
		switch action {
		case "":
			srv.handleAppDetail(w, r, app)
		case "reconcile":
			srv.handleAppReconcile(w, r, app)
		case "uninstall":
			srv.handleAppUninstall(w, r, app)
		case "rollback":
			srv.handleAppRollback(w, r, app)
		default:
			writeErr(w, 404, "not found")
		}
	})
	mux.HandleFunc("/api/packages", srv.handlePackages)
	mux.HandleFunc("/api/packages/", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := parseID(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/packages/"), "/")); ok {
			switch r.Method {
			case http.MethodGet:
				srv.handlePackageDetail(w, r, id)
			case http.MethodDelete:
				srv.handlePackageDelete(w, r, id)
			default:
				writeErr(w, 405, "GET/DELETE only")
			}
			return
		}
		writeErr(w, 404, "not found")
	})
	mux.HandleFunc("/api/schemes", srv.handleSchemes)
	mux.HandleFunc("/api/schemes/autodag", srv.handleAutoDag)
	mux.HandleFunc("/api/schemes/", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := parseID(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/schemes/"), "/")); ok && r.Method == http.MethodDelete {
			srv.handleSchemeDelete(w, r, id)
			return
		}
		writeErr(w, 404, "not found")
	})
	mux.HandleFunc("/api/sessions", srv.handleSessions)
	mux.HandleFunc("/api/sessions/preview", srv.handleSessionPreview) // 先于 /{id} 前缀匹配（统一安装预览）
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
		if rest == "" {
			writeErr(w, 404, "not found")
			return
		}
		// /{id} | /{id}/logs | /{id}/nodes/{key}/{action}
		idStr, tail, _ := strings.Cut(rest, "/")
		id, ok := parseID(idStr)
		if !ok {
			writeErr(w, 404, "not found")
			return
		}
		if tail == "" && r.Method == http.MethodGet {
			srv.handleSessionDetail(w, r, id)
			return
		}
		if tail == "export" && r.Method == http.MethodGet {
			srv.handleSessionExport(w, r, id)
			return
		}
		if tail == "logs" && r.Method == http.MethodGet {
			srv.handleSessionLogs(w, r, id)
			return
		}
		if tail == "abort" && r.Method == http.MethodPost {
			srv.handleSessionAbort(w, r, id)
			return
		}
		if node, action, ok := strings.Cut(strings.TrimPrefix(tail, "nodes/"), "/"); ok && node != "" && action != "" && r.Method == http.MethodPost {
			srv.handleNodeAction(w, r, id, node, action)
			return
		}
		writeErr(w, 404, "not found")
	})
	mux.HandleFunc("/download/agent/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleAgentDownload(w, r, strings.Trim(strings.TrimPrefix(r.URL.Path, "/download/agent/"), "/"))
	})

	// 前端（内嵌 dist；SPA 回退 index.html）
	dist, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		fmt.Fprintln(os.Stderr, "内嵌前端缺失（先执行 npm build 或 build.sh）:", err)
		os.Exit(1)
	}
	fileServer := http.FileServer(http.FS(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(dist, filepath.ToSlash(path)); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		index, _ := fs.ReadFile(dist, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})

	url := "http://" + *addr
	fmt.Println("==============================================")
	fmt.Println("  deliver-console 安装控制台（多机安装分发）")
	fmt.Printf("  访问入口: %s（数据目录: %s）\n", displayAddr(url), dataRoot)
	fmt.Println("  登录账号: admin（界面可修改密码）")
	fmt.Println("==============================================")
	if *resetPass {
		pass := ResetAdminPassword(store)
		fmt.Println()
		fmt.Println("**************************************************")
		fmt.Println("  管理员密码已重置（旧密码与会话全部失效）")
		fmt.Printf("  账号: admin    新密码: %s\n", pass)
		fmt.Println("**************************************************")
	}
	if pass := EnsureAdminPassword(store); pass != "" {
		fmt.Println()
		fmt.Println("**************************************************")
		fmt.Println("  首次启动：已生成管理员密码（仅本次打印，请妥善保存）")
		fmt.Printf("  账号: admin    密码: %s\n", pass)
		fmt.Println("**************************************************")
	}
	if tok := EnsureEnrollToken(store); tok != "" {
		fmt.Println()
		fmt.Println("**************************************************")
		fmt.Println("  agent 引导令牌（仅本次打印；主机池页可轮换）")
		fmt.Printf("  令牌: %s\n", tok)
		fmt.Printf("  安装：deliver-agent install -server %s -token %s\n", displayAddr(url), tok)
		fmt.Println("**************************************************")
	}
	if err := http.ListenAndServe(*addr, auth.Middleware(mux)); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
		os.Exit(1)
	}
}

// displayAddr 0.0.0.0 显示为本机 IP 提示（agent 注册地址需要具体 IP）
func displayAddr(url string) string {
	if runtime.GOOS == "windows" {
		return strings.Replace(url, "0.0.0.0:", "127.0.0.1:", 1)
	}
	return url
}

func parseID(s string) (int64, bool) {
	var id int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	if len(s) == 0 {
		return 0, false
	}
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err == nil
}

// ensureLocalHost 本机节点注册（单机场景零 agent；能力档案随启动刷新）
func (s *Server) ensureLocalHost() {
	info := probeLocalHostInfo()
	h := &hostRow{Name: "local-" + info.Hostname, Local: true, AgentToken: randomToken(),
		Addr: "127.0.0.1", Info: info}
	if err := s.store.UpsertHost(h); err != nil {
		fmt.Fprintln(os.Stderr, "本机节点注册失败:", err)
		return
	}
	fmt.Printf("[local] 本机节点就绪：docker=%v systemd=%v java=%s\n", info.Docker, info.Systemd, info.JavaVer)
}

// resumeSessions 恢复未终态会话（console 重启：dispatched/running 重置 pending 重调度）
func (s *Server) resumeSessions() {
	sss, err := s.store.ListSessions("")
	if err != nil {
		return
	}
	for _, ss := range sss {
		if ss.Status != "running" {
			continue
		}
		var pkgDir string
		if ss.PackageID > 0 { // PackageID=0：docker 引导等无包合成会话，快照即全量
			pkg, err := s.store.PackageByID(ss.PackageID)
			if err != nil {
				continue
			}
			pkgDir = pkg.Dir
		}
		nodes, err := s.store.SessionNodes(ss.ID)
		if err != nil {
			continue
		}
		statuses := map[string]string{}
		for _, n := range nodes {
			statuses[n.Key] = n.Status
		}
		s.ResumeSession(ss.ID, ss.App, pkgDir, LoadPlanSnapshot(ss.Plan), statuses)
		fmt.Printf("[session] #%d 恢复调度（%s %s）\n", ss.ID, ss.App, ss.Version)
	}
}
