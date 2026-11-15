// UI REST：主机池 / 部署包（上传解析 + 增量 diff 预览）/ 安装方案 / 会话（创建执行、进度日志、
// retry/skip/gate）/ agent 二进制下载。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// ---- 主机池 ----

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 本机主机无 agent 心跳，容器端口档案随本页请求即时探测回填（毫秒级；与远端心跳档案同口径）
	for _, h := range hosts {
		if h.Local {
			h.Info.Containers = localContainerPorts()
			break
		}
	}
	writeJSON(w, map[string]any{
		"hosts":         hosts,
		"enrollToken":   s.store.GetSetting(enrollKey, ""),
		"agentDownload": map[string]string{"linux": "/download/agent/linux", "windows": "/download/agent/windows.exe"},
		"agentStamp":    agentStamp, // 内嵌 agent 版本戳：版本列比对红标「更新了但旧进程仍在跑」
	})
}

func (s *Server) handleHostUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "PUT only")
		return
	}
	var req struct {
		Addr   string `json:"addr"`
		Labels string `json:"labels"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.UpdateHost(id, req.Addr, req.Labels); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleHostRevoke(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.RevokeHost(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleHostDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.DeleteHost(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleHostUpgradeAgent 手动下发 agent 升级指令（主机池红标行按钮）：置 pending 标记，
// agent 下一轮轮询（≤25s）收到指令在任务间隙自升级重启；标记在版本翻绿前持续重发（agent
// 失败退避后仍能再收到），成功即由 poll 侧清位。
func (s *Server) handleHostUpgradeAgent(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	h, err := s.store.HostByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if h.Local {
		writeErr(w, http.StatusBadRequest, "本机节点无需升级 agent")
		return
	}
	if !h.Online {
		writeErr(w, http.StatusBadRequest, "主机离线——升级指令需 agent 在线轮询时接收")
		return
	}
	if agentStamp != "" && h.Info.AgentVersion != "" && h.Info.AgentVersion >= agentStamp {
		writeErr(w, http.StatusBadRequest, "agent 版本已不低于 console 内嵌版本（"+agentStamp+"）")
		return
	}
	s.setPendingUpgrade(id)
	writeJSON(w, map[string]any{"ok": true,
		"message": "升级指令已下发：agent 将在下一轮轮询（≤25 秒）的任务间隙自升级并重启"})
}

// handleAgentUpgradeSetting agent 自动升级开关（GET 读 / POST 写；settings 持久化，
// 缺省关=仅手动按钮触发；开启后所有旧版在线 agent 下一轮轮询自动收到升级指令）
func (s *Server) handleAgentUpgradeSetting(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Auto bool `json:"auto"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		v := "0"
		if req.Auto {
			v = "1"
		}
		if err := s.store.PutSetting(agentAutoUpgradeKey, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"auto": req.Auto})
	default:
		writeJSON(w, map[string]any{"auto": s.store.GetSetting(agentAutoUpgradeKey, "") == "1"})
	}
}

func (s *Server) handleEnrollRotate(w http.ResponseWriter, r *http.Request) {
	tok := RotateEnrollToken(s.store)
	writeJSON(w, map[string]any{"enrollToken": tok})
}

// ---- 部署包 ----

func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pkgs, err := s.store.ListPackages()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"packages": pkgs})
	case http.MethodPost: // multipart 流式收包（直落临时 zip；ParseMultipartForm 会先把大文件
		// 整包落一份临时盘再由 FormFile 拷出，多 GB 部署包等于白读白写一遍）
		mr, err := r.MultipartReader()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
			return
		}
		var file io.ReadCloser
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeErr(w, http.StatusBadRequest, "multipart 读取失败: "+err.Error())
				return
			}
			if p.FormName() == "file" {
				file = p
				break
			}
		}
		if file == nil {
			writeErr(w, http.StatusBadRequest, "缺少 file 字段（部署包 zip）")
			return
		}
		defer file.Close()
		tmp, err := os.CreateTemp("", "deliver-pkg-*.zip")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		_, copyErr := io.Copy(tmp, file)
		tmp.Close()
		if copyErr != nil {
			writeErr(w, http.StatusInternalServerError, "上传落盘失败: "+copyErr.Error())
			return
		}
		pp, err := savePackageZip(s.root, tmpPath)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		_, dupErr := s.store.PackageByAppVersion(pp.Row.App, pp.Row.Version)
		overwritten := dupErr == nil
		if err := s.store.UpsertPackage(pp.Row); err != nil {
			writeErr(w, http.StatusInternalServerError, "登记失败: "+err.Error())
			return
		}
		tag := ""
		if overwritten {
			tag = "（覆盖同版本旧记录）"
		}
		fmt.Printf("[pkg] 部署包上传%s：%s %s（%d 个文件）\n", tag, pp.Row.App, pp.Row.Version, len(pp.Row.Files))
		writeJSON(w, map[string]any{"id": pp.Row.ID, "app": pp.Row.App, "version": pp.Row.Version,
			"prefill": pp.Prefill, "prefillMissing": pp.Prefill == nil, "overwritten": overwritten})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/POST only")
	}
}

// handlePackageDetail GET /api/packages/{id}：声明 + 服务清单 + 预填 + 对 inventory 的 diff 预览
func (s *Server) handlePackageDetail(w http.ResponseWriter, r *http.Request, id int64) {
	row, err := s.store.PackageByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "部署包不存在")
		return
	}
	pp, err := parsePackageDir(row.Dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "包目录解析失败: "+err.Error())
		return
	}
	// 服务清单（UI 分配画布数据源）
	var services []map[string]any
	for _, name := range pp.Order {
		m := pp.Meta[name]
		services = append(services, map[string]any{
			"name": name, "owner": m.Owner, "kind": m.Kind, "primary": m.Primary,
			"image": resolveEnvRefs(m.Image, map[string]string{}), "ports": m.Ports, "deps": m.Deps,
			"unitPort": unitPortOf(pp, name), "unitHealth": unitHealthOf(pp, name), "artifact": artifactOf(pp, name),
		})
	}
	// console 组态器：内置拓扑形态清单（组件卡「组态」入口数据源，topology.go）
	topoForms := map[string][]map[string]any{}
	for _, c := range pp.Decl.Components {
		if forms := topologyFormsFor(c); len(forms) > 0 {
			topoForms[c.Code] = forms
		}
	}
	// 增量 diff 预览（对 inventory，服务级粗粒度）：按 blockHash/image 两字段子集比对——inventory
	// detail 含会话期生成的 envHash/confHash（依赖变量取值），预览时无从生成，整串比对恒不等；
	// 变量与配置变更在发起安装时由引擎精确比对（预览 UI 注明口径）
	inv, _ := s.store.InventoryByApp(row.App)
	invMap := map[string]invRow{}
	for _, r := range inv {
		invMap[r.Service] = r
	}
	diff := map[string][]string{"added": {}, "changed": {}, "unchanged": {}, "removed": {}}
	for _, name := range pp.Order {
		m := pp.Meta[name]
		row, installed := invMap[name]
		if !installed {
			diff["added"] = append(diff["added"], name)
			continue
		}
		var prev struct {
			BlockHash string `json:"blockHash"`
			Image     string `json:"image"`
		}
		_ = json.Unmarshal([]byte(row.Detail), &prev)
		// inventory 存解析变量后的镜像引用，预览侧同口径解析（无变量值时原样，含变量模板仍可能
		// 显示为变更——精确比对在发起安装时）
		if prev.BlockHash == hashStr(m.Block) && prev.Image == resolveEnvRefs(m.Image, nil) {
			diff["unchanged"] = append(diff["unchanged"], name)
		} else {
			diff["changed"] = append(diff["changed"], name)
		}
	}
	for _, r := range inv {
		if _, ok := pp.Meta[r.Service]; !ok {
			diff["removed"] = append(diff["removed"], r.Service)
		}
	}
	writeJSON(w, map[string]any{
		"id": row.ID, "app": row.App, "version": row.Version, "pkgSha": row.PkgSha, "decl": pp.Decl,
		"services": services, "prefill": pp.Prefill, "prefillMissing": pp.Prefill == nil, "diff": diff,
		"topologyForms": topoForms, "files": len(row.Files),
	})
}

func unitPortOf(pp *ParsedPackage, svc string) string {
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			if s == svc {
				return u.Port
			}
		}
	}
	return ""
}

func unitHealthOf(pp *ParsedPackage, svc string) string {
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			if s == svc {
				return u.HealthPath
			}
		}
	}
	return ""
}

func (s *Server) handlePackageDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.DeletePackage(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 安装方案 ----

func (s *Server) handleSchemes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		scs, err := s.store.ListSchemes(r.URL.Query().Get("app"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"schemes": scs})
	case http.MethodPost:
		var req schemeRow
		if !readJSON(w, r, &req) {
			return
		}
		if req.App == "" || req.Name == "" {
			writeErr(w, http.StatusBadRequest, "缺少 app/name")
			return
		}
		if req.Placement == "" {
			req.Placement = "{}"
		}
		if req.Dag == "" {
			req.Dag = "[]"
		}
		if err := s.store.UpsertScheme(&req); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, req)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/POST only")
	}
}

func (s *Server) handleSchemeDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.DeleteScheme(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAutoDag POST /api/schemes/autodag：默认 DAG 预览（UI 编辑起点）
func (s *Server) handleAutoDag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PackageID int64     `json:"packageId"`
		Placement Placement `json:"placement"`
		Mode      string    `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := s.buildPlan(req.PackageID, &req.Placement, nil, orStr2(req.Mode, "fresh"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var dag []DagNode
	for _, n := range res.Nodes {
		d := DagNode{ID: n.Key, Kind: n.Kind, Title: n.Title, Host: n.Host, Deps: n.Deps}
		if n.Kind == "cmd" { // schema 等自动 cmd 的脚本随预览带出（UI 详情可见、往返提交不丢）
			var p CmdPayload
			if json.Unmarshal(n.Payload, &p) == nil {
				d.Script = p.Script
			}
		}
		dag = append(dag, d)
	}
	writeJSON(w, map[string]any{"dag": dag, "warnings": res.Warnings, "entryUrl": res.EntryURL, "mode": res.Mode})
}

// ---- 会话 ----

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sss, err := s.store.ListSessions(r.URL.Query().Get("app"))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"sessions": sss})
	case http.MethodPost:
		var req struct {
			PackageID int64     `json:"packageId"`
			Placement Placement `json:"placement"`
			Dag       []DagNode `json:"dag"`
			Mode      string    `json:"mode"`   // fresh|incremental
			Scheme    string    `json:"scheme"` // 可选：成功后按此名保存方案
		}
		if !readJSON(w, r, &req) {
			return
		}
		res, pkg, err := s.buildPlanRow(req.PackageID, &req.Placement, req.Dag, orStr2(req.Mode, "fresh"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		varsJSON, _ := json.Marshal(res.Vars)
		snap := &planSnapshot{Nodes: res.Nodes, EntryURL: res.EntryURL, Warnings: res.Warnings, Mode: res.Mode}
		snapJSON, _ := json.Marshal(snap)
		plJSON, _ := json.Marshal(req.Placement)
		ss := &sessionRow{App: pkg.App, Version: pkg.Version, PackageID: req.PackageID, PkgSha: pkg.PkgSha,
			Mode: res.Mode, Vars: string(varsJSON), Plan: string(snapJSON), Placement: string(plJSON)}
		if err := s.store.InsertSession(ss, res.Nodes); err != nil {
			writeErr(w, http.StatusInternalServerError, "会话创建失败: "+err.Error())
			return
		}
		if req.Scheme != "" {
			plJSON, _ := json.Marshal(req.Placement)
			dagJSON := "[]"
			if len(req.Dag) > 0 {
				dagJSON = string(mustJSON(req.Dag))
			}
			_ = s.store.UpsertScheme(&schemeRow{App: pkg.App, Name: req.Scheme, Placement: string(plJSON), Dag: dagJSON})
		}
		s.StartSession(ss.ID, ss.App, pkg.Dir, snap)
		fmt.Printf("[session] #%d 启动：%s %s（%s，%d 节点）\n", ss.ID, ss.App, ss.Version, res.Mode, len(res.Nodes))
		writeJSON(w, map[string]any{"id": ss.ID, "mode": res.Mode, "warnings": res.Warnings,
			"entryUrl": res.EntryURL, "nodes": len(res.Nodes)})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/POST only")
	}
}

// buildPlanRow 同 buildPlan 但返回包行（会话创建用）
func (s *Server) buildPlanRow(pkgID int64, pl *Placement, dag []DagNode, mode string) (*buildResult, *packageRow, error) {
	row, err := s.store.PackageByID(pkgID)
	if err != nil {
		return nil, nil, fmt.Errorf("部署包 #%d 不存在", pkgID)
	}
	res, err := s.buildPlanWithRow(row, pl, dag, mode)
	return res, row, err
}

func (s *Server) buildPlan(pkgID int64, pl *Placement, dag []DagNode, mode string) (*buildResult, error) {
	res, _, err := s.buildPlanRow(pkgID, pl, dag, mode)
	return res, err
}

func (s *Server) buildPlanWithRow(row *packageRow, pl *Placement, dag []DagNode, mode string) (*buildResult, error) {
	pp, err := parsePackageDir(row.Dir)
	if err != nil {
		return nil, fmt.Errorf("包目录解析失败: %w", err)
	}
	// 文件清单自 DB 行回填（上传时随解包哈希一次入库；不再对全包做二次磁盘重哈希）
	pp.Row.Files = row.Files
	hosts := map[int64]*hostRow{}
	list, err := s.store.ListHosts()
	if err != nil {
		return nil, err
	}
	for _, h := range list {
		if !h.Revoked {
			hosts[h.ID] = h
		}
	}
	inv, _ := s.store.InventoryByApp(row.App)
	// 增量安装：表单未覆盖的变量沿用上次会话取值（随机凭据稳定，未变更服务不入计划）
	if mode == "incremental" {
		last := s.store.LastSessionVars(row.App)
		if pl.Vars == nil {
			pl.Vars = map[string]string{}
		}
		for k, v := range last {
			if pl.Vars[k] == "" {
				pl.Vars[k] = v
			}
		}
	}
	return BuildSessionPlan(pp, pl, hosts, inv, dag, mode, s.dockerOpts())
}

// handleSessionDetail GET /api/sessions/{id}
func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request, id int64) {
	ss, err := s.store.SessionByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	nodes, err := s.store.SessionNodes(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	snap := LoadPlanSnapshot(ss.Plan)
	writeJSON(w, map[string]any{
		"session": ss, "nodes": nodes, "entryUrl": snap.EntryURL, "warnings": snap.Warnings,
	})
}

// handleSessionLogs GET /api/sessions/{id}/logs?after=<logId>&node=<nodeKey>（node 可选：仅取该节点行）
func (s *Server) handleSessionLogs(w http.ResponseWriter, r *http.Request, id int64) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines, err := s.store.LogsAfter(id, after, 500, r.URL.Query().Get("node"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"lines": lines})
}

// handleSessionAbort POST /api/sessions/{id}/abort——中止安装（运行中会话：剩余节点置已中止、
// 在途任务经 report 响应 cancel 中断杀进程组、队列滞留任务丢弃；终态落库由执行器 finalize 承担）
func (s *Server) handleSessionAbort(w http.ResponseWriter, r *http.Request, id int64) {
	run := s.session(id)
	if run == nil {
		// 不在内存：运行中会话（console 重启后未恢复前）直接落库收尾——节点与状态一次置终态
		ss, err := s.store.SessionByID(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		if ss.Status != "running" {
			writeErr(w, http.StatusConflict, "会话已终结（"+ss.Status+"），无需中止")
			return
		}
		nodes, err := s.store.SessionNodes(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, n := range nodes {
			switch n.Status {
			case "pending", "dispatched", "running":
				_ = s.store.UpdateNodeStatus(id, n.Key, "failed", "已中止：用户发起")
			}
		}
		_ = s.store.UpdateSessionStatus(id, "aborted", "安装已中止（用户发起）")
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if err := run.Abort(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleNodeAction POST /api/sessions/{id}/nodes/{key}/{action}（retry|skip|gate）
func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request, id int64, key, action string) {
	run := s.session(id)
	if run == nil {
		// 不在内存：仅 gate/终态会话的轻量操作直接落库（重试需运行器）
		if action == "gate" {
			if err := s.store.UpdateNodeStatus(id, key, "ok", "人工确认通过"); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		writeErr(w, http.StatusConflict, "会话不在运行中（console 重启后请刷新页面重试）")
		return
	}
	var err error
	switch action {
	case "retry":
		err = run.Retry(key)
	case "skip":
		err = run.Skip(key)
	case "gate":
		err = run.Gate(key)
	default:
		err = fmt.Errorf("未知操作 %q", action)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- agent 二进制下载（登录态；占位文件拒绝） ----

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request, plat string) {
	var name string
	switch plat {
	case "linux":
		name = "deliver-agent-linux"
	case "windows":
		name = "deliver-agent-windows.exe"
	default:
		writeErr(w, http.StatusNotFound, "未知平台")
		return
	}
	data, err := agentBinFS(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent 二进制未内嵌（构建时由 build.sh 交叉编译 deliver-agent 后嵌入）")
		return
	}
	if len(data) < 100*1024 {
		writeErr(w, http.StatusNotFound, "agent 二进制是占位文件（<100KB）——请先执行 build.sh 完整构建")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(data)
}

func orStr2(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
