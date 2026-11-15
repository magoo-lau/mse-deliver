// deliver-studio：通用交付工作台（本地单机工具）。
//
// 形态：单文件二进制（go:embed 内嵌 Vue 前端 + 示例预设），SQLite 持久化，
// 大文件（物料/镜像仓/部署包）落本地目录。零仓库知识——应用与组件全部数据化声明。
//
// 启动：deliver-studio [-addr 127.0.0.1:7860] [-root ./deliver-data] [-open]
// 构建：sh build.sh（npm build 前端 → go build 内嵌；安装由 deliver-console + deliver-agent 承担）
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var srv *Server

func main() {
	addr := flag.String("addr", "127.0.0.1:7860", "监听地址")
	root := flag.String("root", "", "数据目录（缺省 ./deliver-data）")
	open := flag.Bool("open", true, "自动打开浏览器")
	resetPass := flag.Bool("reset-pass", false, "重置管理员密码（生成新随机密码打印到控制台后继续启动）")
	flag.Parse()

	dataRoot := *root
	if dataRoot == "" {
		exe, err := os.Executable()
		if err == nil {
			dataRoot = filepath.Join(filepath.Dir(exe), "deliver-data")
		} else {
			dataRoot = "deliver-data"
		}
	}
	store, err := OpenStore(dataRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "存储初始化失败:", err)
		os.Exit(1)
	}
	defer store.Close()
	if n := store.ReconcileRunningPackages(); n > 0 {
		fmt.Printf("启动对账：%d 条遗留「进行中」打包记录改判失败（进程重启中断）\n", n)
	}
	if n, err := store.ReconcileDuplicateMaterials(); err != nil {
		fmt.Fprintln(os.Stderr, "制品存量对账失败:", err)
	} else if n > 0 {
		fmt.Printf("启动对账：%d 条重复制品清除（同类型+文件名+应用+版本只保留最新一条）\n", n)
	}
	auth := NewAuth(store)
	srv = &Server{store: store, engine: NewEngine(dataRoot, store), root: dataRoot}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", auth.handleLogin)
	mux.HandleFunc("/api/logout", auth.handleLogout)
	mux.HandleFunc("/api/whoami", auth.handleWhoami)
	mux.HandleFunc("/api/settings/password", auth.handlePasswordChange)
	mux.HandleFunc("/api/apps", srv.handleApps)
	mux.HandleFunc("/api/apps/", func(w http.ResponseWriter, r *http.Request) {
		if code := strings.TrimPrefix(r.URL.Path, "/api/apps/"); code != "" && r.Method == http.MethodDelete {
			srv.handleAppDelete(w, r, code)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/components", srv.handleComponents)
	mux.HandleFunc("/api/components/import", srv.handleComponentsImport)
	mux.HandleFunc("/api/components/export", srv.handleComponentsExport)
	mux.HandleFunc("/api/components/", func(w http.ResponseWriter, r *http.Request) {
		if code := strings.TrimPrefix(r.URL.Path, "/api/components/"); code != "" && r.Method == http.MethodDelete {
			srv.handleComponentDelete(w, r, code)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/presets", srv.handlePresets)
	mux.HandleFunc("/api/presets/", func(w http.ResponseWriter, r *http.Request) {
		// /api/presets/{name}/apply：一键载入内嵌示例预设
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/presets/"), "/"), "/")
		if len(parts) == 2 && parts[1] == "apply" && parts[0] != "" && r.Method == http.MethodPost {
			srv.handlePresetApply(w, r, parts[0])
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/export", srv.handleExport)
	mux.HandleFunc("/api/import", srv.handleImport)
	mux.HandleFunc("/api/materials", srv.handleMaterials)
	mux.HandleFunc("/api/materials/", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := pathID(r.URL.Path); ok && r.Method == http.MethodDelete {
			srv.handleMaterialDelete(w, r, id)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/images", srv.handleImages)
	mux.HandleFunc("/api/images/", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := pathID(r.URL.Path); ok && r.Method == http.MethodDelete {
			srv.handleImageDelete(w, r, id)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/packages", srv.handlePackages)
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
		} else if strings.HasSuffix(path, "/deliver") {
			if id, ok := pathID(strings.TrimSuffix(path, "/deliver")); ok {
				srv.handlePackageDeliver(w, r, id)
				return
			}
		} else if id, ok := pathID(path); ok {
			srv.handlePackageDetail(w, r, id)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/deliveries", srv.handleDeliveries)
	mux.HandleFunc("/api/deliveries/", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := pathID(r.URL.Path); ok && r.Method == http.MethodDelete {
			srv.handleDeliveryDelete(w, r, id)
			return
		}
		w.WriteHeader(404)
	})
	mux.HandleFunc("/api/settings", srv.handleSettings)

	// 前端（内嵌 dist；SPA 回退到 index.html）
	dist, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		fmt.Fprintln(os.Stderr, "内嵌前端缺失:", err)
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
		// SPA 回退
		index, _ := fs.ReadFile(dist, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})

	url := "http://" + *addr
	fmt.Println("==============================================")
	fmt.Println("  deliver-studio 通用交付工作台")
	fmt.Printf("  访问入口: %s（数据目录: %s）\n", url, dataRoot)
	fmt.Println("  登录账号: admin（界面右上/侧栏底部可修改密码）")
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
		fmt.Println("  登录后请在界面修改；collect-kit 推送经 --studio-pass 传同一密码")
		fmt.Println("**************************************************")
	}
	if *open {
		openBrowser(url)
	}
	if err := http.ListenAndServe(*addr, auth.Middleware(mux)); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
		os.Exit(1)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
