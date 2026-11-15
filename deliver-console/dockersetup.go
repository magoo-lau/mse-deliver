// Docker / docker-compose 安装源设置与主机池一键安装（编排自动补装与一键装共用）：
//
//	GET/PUT  /api/docker-source                     docker 在线脚本 + registry mirrors + 版本化介质清单
//	POST     /api/docker-source/media/{槽位}         上传介质（multipart file+version，顺序不敏感）
//	DELETE   /api/docker-source/media/{槽位}/{版本}  删除介质版本
//	GET/PUT  /api/compose-source                    compose 在线直链模板 + 缺省版本 + 版本化介质清单
//	POST     /api/compose-source/media/{槽位}        上传 compose 插件二进制（multipart file+version）
//	DELETE   /api/compose-source/media/{槽位}/{版本} 删除
//	POST     /api/hosts/{id}/install-docker         一键装/换版 Docker（body {version?, force?}）
//	POST     /api/hosts/{id}/install-compose        一键装/换版 docker compose（body {version?, force?}）
//
// 介质存储与 agent 下载端点见 medialib.go。Docker 指定版本仅离线介质可承载（在线脚本装发行版
// 最新）；compose 在线直链由 console 按所选版本渲染模板，可在线定版。
package main

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	dockerScriptKey  = "docker_script_url"
	dockerMirrorsKey = "docker_mirrors"
	composeURLKey    = "compose_url_template"
	composeVerKey    = "compose_version"
)

// dockerOpts 从 settings 物化基础工具安装源（docker 引擎 + compose 插件，引擎与一键装共用）
func (s *Server) dockerOpts() *DockerOpts {
	opts := &DockerOpts{ScriptURL: s.store.GetSetting(dockerScriptKey, ""),
		Media: s.mediaLib(mediaToolDocker),
		Compose: &ComposeOpts{
			Template: s.store.GetSetting(composeURLKey, ""),
			Version:  s.store.GetSetting(composeVerKey, ""),
			Media:    s.mediaLib(mediaToolCompose),
		}}
	for _, m := range strings.Fields(s.store.GetSetting(dockerMirrorsKey, "")) {
		m = strings.TrimSuffix(m, ",")
		if m != "" {
			opts.Mirrors = append(opts.Mirrors, m)
		}
	}
	return opts
}

// handleDockerSource GET=查看 / PUT=保存（scriptUrl 留空=回到官方 get.docker.com 兜底）
func (s *Server) handleDockerSource(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"scriptUrl": s.store.GetSetting(dockerScriptKey, ""),
			"mirrors":   strings.Fields(s.store.GetSetting(dockerMirrorsKey, "")),
			"default":   defaultDockerScript,
			"media":     s.mediaLib(mediaToolDocker),
		})
	case http.MethodPut:
		var req struct {
			ScriptURL string `json:"scriptUrl"`
			Mirrors   string `json:"mirrors"` // 逗号/空白分隔
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.ScriptURL != "" && !strings.HasPrefix(req.ScriptURL, "http") {
			writeErr(w, http.StatusBadRequest, "在线脚本地址须为 http(s) URL")
			return
		}
		_ = s.store.PutSetting(dockerScriptKey, req.ScriptURL)
		_ = s.store.PutSetting(dockerMirrorsKey, strings.Join(strings.Fields(req.Mirrors), " "))
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/PUT only")
	}
}

// handleComposeSource GET=查看 / PUT=保存（template 留空=官方 GitHub releases 兜底模板）
func (s *Server) handleComposeSource(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"template": s.store.GetSetting(composeURLKey, ""),
			"default":  defaultComposeTemplate,
			"version":  s.store.GetSetting(composeVerKey, ""),
			"media":    s.mediaLib(mediaToolCompose),
		})
	case http.MethodPut:
		var req struct {
			Template string `json:"template"`
			Version  string `json:"version"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		req.Template, req.Version = strings.TrimSpace(req.Template), strings.TrimSpace(req.Version)
		if req.Template != "" && !strings.HasPrefix(req.Template, "http") {
			writeErr(w, http.StatusBadRequest, "在线直链模板须为 http(s) URL（支持 {version}/{platform} 占位符）")
			return
		}
		_ = s.store.PutSetting(composeURLKey, req.Template)
		_ = s.store.PutSetting(composeVerKey, req.Version)
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/PUT only")
	}
}

// handleHostInstallDocker POST /api/hosts/{id}/install-docker：主机池一键装/换版——合成单节点
// 小会话（App=docker），复用全套调度/看门狗/日志/重试；安装记录页可见进度。
// version 指定目标版本（须已上传对应槽位介质——在线脚本无法定版）；force=已装同版本仍重装。
func (s *Server) handleHostInstallDocker(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	req := installReq(w, r)
	if req == nil {
		return
	}
	h, err := s.store.HostByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if h.Revoked {
		writeErr(w, http.StatusBadRequest, "主机已吊销")
		return
	}
	if h.Info.OS != "linux" {
		writeErr(w, http.StatusBadRequest, "自动安装 Docker 仅支持 Linux 主机（Windows 请预装 Docker Desktop 并确保守护进程运行）")
		return
	}
	if h.Info.Docker && !req.Force {
		if req.Version == "" {
			writeErr(w, http.StatusBadRequest, "主机已安装 docker "+h.Info.DockerVer+"（守护进程可用）——换版本请指定目标版本")
			return
		}
		if verEqual(h.Info.DockerVer, req.Version) {
			writeErr(w, http.StatusBadRequest, "主机已是 docker "+h.Info.DockerVer+"（如需重装勾选强制）")
			return
		}
	}
	if !h.Local && !h.OnlineCheck() {
		writeErr(w, http.StatusBadRequest, "主机离线——agent 上线后再试（或检查 agent 服务）")
		return
	}
	if req.Version != "" {
		if _, ok := s.mediaLib(mediaToolDocker)[h.Info.OS+"-"+h.Info.Arch][req.Version]; !ok {
			writeErr(w, http.StatusBadRequest, "指定版本 "+req.Version+" 需先上传对应离线介质（docker 在线脚本只能装最新版）——主机池「Docker 安装源」上传 "+h.Info.OS+"-"+h.Info.Arch+" 槽位")
			return
		}
	}
	opts := s.dockerOpts()
	node := buildDockerNode(h, opts, req.Version, req.Force)
	snap := &planSnapshot{Nodes: []planNode{node}, Mode: "fresh"}
	ss := &sessionRow{App: "docker", Version: h.Name, PackageID: 0, Mode: "fresh",
		Plan: string(mustJSON(snap)), Placement: "{}"}
	if err := s.store.InsertSession(ss, snap.Nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话创建失败: "+err.Error())
		return
	}
	s.StartSession(ss.ID, ss.App, "", snap)
	fmt.Printf("[session] #%d 启动：安装 Docker（%s，版本 %s）\n", ss.ID, h.Name, orStr(req.Version, "最新"))
	writeJSON(w, map[string]any{"id": ss.ID, "node": node.Key})
}

// handleHostInstallCompose POST /api/hosts/{id}/install-compose：主机池一键装/换版 docker compose
// 插件（App=compose 单节点会话）。compose 依赖 docker CLI——主机须已装 docker；版本来源：指定版本
// （介质优先，无介质按模板渲染在线直链）或缺省（最新介质 / 在线缺省版本）。
func (s *Server) handleHostInstallCompose(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	req := installReq(w, r)
	if req == nil {
		return
	}
	h, err := s.store.HostByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if h.Revoked {
		writeErr(w, http.StatusBadRequest, "主机已吊销")
		return
	}
	if h.Info.OS != "linux" {
		writeErr(w, http.StatusBadRequest, "自动安装 docker compose 仅支持 Linux 主机（Windows 请预装 Docker Desktop——桌面版自带 compose 插件）")
		return
	}
	if !h.Info.Docker {
		writeErr(w, http.StatusBadRequest, "主机未安装 docker（compose 插件依赖 docker CLI）——先安装 Docker")
		return
	}
	if h.Info.Compose && !req.Force {
		if req.Version == "" {
			writeErr(w, http.StatusBadRequest, "docker compose 已就绪（"+h.Info.ComposeVer+"）——换版本请指定目标版本")
			return
		}
		if verEqual(h.Info.ComposeVer, req.Version) {
			writeErr(w, http.StatusBadRequest, "docker compose 已是 "+h.Info.ComposeVer+"（如需重装勾选强制）")
			return
		}
	}
	if !h.Local && !h.OnlineCheck() {
		writeErr(w, http.StatusBadRequest, "主机离线——agent 上线后再试（或检查 agent 服务）")
		return
	}
	opts := s.dockerOpts()
	p := buildToolkitPayload(h, opts, req.Version, req.Force)
	if p.MediaSHA256 == "" && p.OnlineURL == "" {
		writeErr(w, http.StatusBadRequest, "缺少 compose 安装源——主机池「Compose 安装源」上传介质或设置在线版本/模板")
		return
	}
	node := buildToolkitNode(h, opts, req.Version, req.Force)
	snap := &planSnapshot{Nodes: []planNode{node}, Mode: "fresh"}
	ss := &sessionRow{App: "compose", Version: h.Name, PackageID: 0, Mode: "fresh",
		Plan: string(mustJSON(snap)), Placement: "{}"}
	if err := s.store.InsertSession(ss, snap.Nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话创建失败: "+err.Error())
		return
	}
	s.StartSession(ss.ID, ss.App, "", snap)
	fmt.Printf("[session] #%d 启动：安装 docker-compose（%s，版本 %s）\n", ss.ID, h.Name, orStr(p.MediaVer, "缺省"))
	writeJSON(w, map[string]any{"id": ss.ID, "node": node.Key})
}

// installToolReq 一键装请求体（{version?, force?}；空 body 合法=缺省安装）
type installToolReq struct {
	Version string `json:"version"`
	Force   bool   `json:"force"`
}

func installReq(w http.ResponseWriter, r *http.Request) *installToolReq {
	req := &installToolReq{}
	if r.ContentLength == 0 {
		return req
	}
	if !readJSON(w, r, req) {
		return nil
	}
	req.Version = strings.TrimSpace(req.Version)
	if req.Version != "" && !mediaVerRe.MatchString(req.Version) {
		writeErr(w, http.StatusBadRequest, "版本号格式应为字母数字开头、含 . _ + - 的短串（如 27.5.1 / v2.30.3）")
		return nil
	}
	return req
}

func humanSizeLocal(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
}
