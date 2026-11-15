// apps.go —— 应用中心（KubeSphere 应用负载对应物）：以 inventory 为状态真源的装后运维入口。
//
//	GET  /api/apps                     应用列表（inventory 聚合：服务/主机/版本/最近会话）
//	GET  /api/apps/{app}               应用详情（服务×主机×镜像明细 + 漂移快照 + 近期会话 + 可升级包）
//	POST /api/apps/{app}/reconcile     对账检查（inventory 期望 × 主机容器档案实况 → 漂移清单）
//	POST /api/apps/{app}/uninstall     卸载（合成清理会话：label 定位容器删除 + 网络清理 + inventory 收敛；
//	                                   ./data 数据目录恒保留）
//	POST /api/apps/{app}/rollback      回退（历史成功会话重放：按其 placement+vars+包走增量安装=介质回退，
//	                                   ADR-6：库结构与数据不回滚，apply-forward）
//	POST /api/sessions/preview         统一安装预览（期望态渲染 + 对 inventory 的逐服务 diff——与执行
//	                                   同一引擎同一口径，取代包详情页的粗粒度 blockHash/image 预览）
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---- 应用列表 / 详情 ----

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	inv, err := s.store.AllInventory()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type appAgg struct {
		Services int
		Hosts    map[int64]bool
	}
	apps := map[string]*appAgg{}
	for _, row := range inv {
		a := apps[row.App]
		if a == nil {
			a = &appAgg{Hosts: map[int64]bool{}}
			apps[row.App] = a
		}
		a.Services++
		a.Hosts[row.HostID] = true
	}
	sessions, err := s.store.ListSessions("")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type appInfo struct {
		App            string `json:"app"`
		Services       int    `json:"services"`
		Hosts          int    `json:"hosts"`
		Version        string `json:"version"`
		LastSessionID  int64  `json:"lastSessionId"`
		LastStatus     string `json:"lastStatus"`
		LastSummary    string `json:"lastSummary"`
		LastFinishedAt string `json:"lastFinishedAt,omitempty"`
		Running        bool   `json:"running"`
	}
	out := map[string]*appInfo{}
	for name := range apps {
		out[name] = &appInfo{App: name, Services: apps[name].Services, Hosts: len(apps[name].Hosts)}
	}
	for _, ss := range sessions { // ListSessions 按新到旧
		info, ok := out[ss.App]
		if !ok {
			continue
		}
		if info.LastSessionID == 0 {
			info.LastSessionID, info.LastStatus, info.LastSummary, info.Version = ss.ID, ss.Status, ss.Summary, ss.Version
			info.Running = ss.Status == "running"
			if ss.Status == "running" {
				info.LastStatus = "running"
			}
		} else if ss.Status == "running" {
			info.Running = true
		}
	}
	var list []*appInfo
	for _, info := range out {
		list = append(list, info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].App < list[j].App })
	writeJSON(w, map[string]any{"apps": list})
}

func (s *Server) handleAppDetail(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	inv, err := s.store.InventoryByApp(app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(inv) == 0 {
		writeErr(w, http.StatusNotFound, "应用 "+app+" 无安装记录（inventory 为空）")
		return
	}
	hosts, _ := s.store.ListHosts()
	hostByID := map[int64]*hostRow{}
	for _, h := range hosts {
		hostByID[h.ID] = h
	}
	type svcRow struct {
		Service string `json:"service"`
		HostID  int64  `json:"hostId"`
		Host    string `json:"host"`
		Kind    string `json:"kind"`
		Image   string `json:"image,omitempty"`
		Detail  string `json:"-"`
	}
	var svcs []svcRow
	for _, row := range inv {
		sr := svcRow{Service: row.Service, HostID: row.HostID, Kind: row.Kind, Detail: row.Detail}
		if h := hostByID[row.HostID]; h != nil {
			sr.Host = h.Name
		} else {
			sr.Host = fmt.Sprintf("h%d", row.HostID)
		}
		var d struct {
			Image string `json:"image"`
		}
		_ = json.Unmarshal([]byte(row.Detail), &d)
		sr.Image = d.Image
		svcs = append(svcs, sr)
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
	sessions, _ := s.store.ListSessions(app)
	pkgs, _ := s.store.ListPackages()
	var upgrades []*packageRow
	for _, p := range pkgs {
		if p.App == app {
			upgrades = append(upgrades, p)
		}
	}
	writeJSON(w, map[string]any{
		"app": app, "services": svcs,
		"drift": s.driftSnapshot(app),
		"sessions": sessions, "packages": upgrades,
	})
}

// ---- 对账（应用时对账 + 手动漂移修复；无后台循环） ----

type driftItem struct {
	HostID       int64  `json:"hostId"`
	Host         string `json:"host"`
	Service      string `json:"service"`
	Status       string `json:"status"` // ok|missing|unexpected|imageMismatch|noArchive
	Expected     string `json:"expected,omitempty"`
	Actual       string `json:"actual,omitempty"`
	Container    string `json:"container,omitempty"`
	ArchiveStale bool   `json:"archiveStale,omitempty"` // 档案时点久远（远端心跳 ≤120s，提示口径）
}

type driftReport struct {
	App       string      `json:"app"`
	CheckedAt string      `json:"checkedAt"`
	Items     []driftItem `json:"items"`
	Drifted   bool        `json:"drifted"`
}

// driftSnapshot 对账核心（复用 agent 心跳容器档案；本机节点即时探测）。归因口径：
//   - missing：inventory 有行、实况无对应容器（被手动删掉/未起来）
//   - unexpected：实况有本项目容器、inventory 无行（手动 docker run / 残留）
//   - imageMismatch：镜像不一致（手动换了镜像/升级中断）
//   - noArchive：主机容器档案缺失（agent 离线/心跳未达/无 docker）——降级不判定
func (s *Server) driftSnapshot(app string) *driftReport {
	rep := &driftReport{App: app, CheckedAt: time.Now().Format(time.RFC3339)}
	inv, _ := s.store.InventoryByApp(app)
	hosts, _ := s.store.ListHosts()
	hostByID := map[int64]*hostRow{}
	for _, h := range hosts {
		hostByID[h.ID] = h
	}
	// 本机主机即时探测刷新档案（毫秒级），远端用最近心跳档案（≤120s 时点）
	for _, h := range hosts {
		if h.Local {
			h.Info.Containers = localContainerPorts()
			break
		}
	}
	byHost := map[int64][]invRow{}
	for _, row := range inv {
		byHost[row.HostID] = append(byHost[row.HostID], row)
	}
	appSeg := sanitizeSeg(app)
	for hostID, rows := range byHost {
		h := hostByID[hostID]
		hostName := fmt.Sprintf("h%d", hostID)
		var containers []ContainerPort
		stale := false
		if h != nil {
			hostName = h.Name
			containers = h.Info.Containers
			if !h.Local && time.Now().Unix()-h.LastSeen > 180 {
				stale = true
			}
		}
		proj := fmt.Sprintf("%s-h%d", appSeg, hostID)
		actual := map[string]ContainerPort{} // service → 容器（本项目 label 归因）
		for _, c := range containers {
			if c.Project != proj {
				continue
			}
			svc := strings.TrimPrefix(c.Name, proj+"-")
			svc = strings.TrimSuffix(svc, "-1")
			svc = strings.TrimSuffix(svc, "_1")
			if svc == "" {
				svc = c.Name
			}
			actual[svc] = c
		}
		emitted := map[string]bool{}
		for _, row := range rows {
			item := driftItem{HostID: hostID, Host: hostName, Service: row.Service}
			var d struct {
				Image string `json:"image"`
			}
			_ = json.Unmarshal([]byte(row.Detail), &d)
			item.Expected = d.Image
			if len(containers) == 0 {
				item.Status = "noArchive"
			} else if c, ok := actual[row.Service]; !ok {
				item.Status = "missing"
			} else if d.Image != "" && c.Image != "" && c.Image != d.Image {
				item.Status, item.Actual, item.Container = "imageMismatch", c.Image, c.Name
			} else {
				item.Status = "ok"
				item.Container = c.Name
			}
			item.ArchiveStale = stale
			emitted[row.Service] = true
			rep.Items = append(rep.Items, item)
		}
		for svc, c := range actual {
			if emitted[svc] {
				continue
			}
			rep.Items = append(rep.Items, driftItem{HostID: hostID, Host: hostName, Service: svc,
				Status: "unexpected", Actual: c.Image, Container: c.Name})
		}
	}
	sort.Slice(rep.Items, func(i, j int) bool {
		if rep.Items[i].HostID != rep.Items[j].HostID {
			return rep.Items[i].HostID < rep.Items[j].HostID
		}
		return rep.Items[i].Service < rep.Items[j].Service
	})
	for _, it := range rep.Items {
		if it.Status != "ok" {
			rep.Drifted = true
			break
		}
	}
	return rep
}

func (s *Server) handleAppReconcile(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if inv, _ := s.store.InventoryByApp(app); len(inv) == 0 {
		writeErr(w, http.StatusNotFound, "应用 "+app+" 无安装记录")
		return
	}
	writeJSON(w, s.driftSnapshot(app))
}

// ---- 卸载（合成清理会话；./data 恒保留） ----

func (s *Server) handleAppUninstall(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Confirm string `json:"confirm"` // 须填应用名（防误触二次确认）
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != app {
		writeErr(w, http.StatusBadRequest, "二次确认失败：confirm 须填写应用名 "+app)
		return
	}
	inv, err := s.store.InventoryByApp(app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(inv) == 0 {
		writeErr(w, http.StatusNotFound, "应用 "+app+" 无安装记录")
		return
	}
	byHost := map[int64][]string{}
	for _, row := range inv {
		byHost[row.HostID] = append(byHost[row.HostID], row.Service)
	}
	hosts, _ := s.store.ListHosts()
	hostByID := map[int64]*hostRow{}
	for _, h := range hosts {
		hostByID[h.ID] = h
	}
	appSeg := sanitizeSeg(app)
	var ids []int64
	for id := range byHost {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var nodes []planNode
	for _, id := range ids {
		h := hostByID[id]
		if h == nil {
			continue
		}
		proj := fmt.Sprintf("%s-h%d", appSeg, id)
		var script strings.Builder
		for _, svc := range byHost[id] {
			fmt.Fprintf(&script, "docker rm -f %s-%s-1\n", proj, svc) // compose v2 命名
			fmt.Fprintf(&script, "docker rm -f %s-%s_1\n", proj, svc) // compose v1 命名
		}
		fmt.Fprintf(&script, "docker network rm %s_default\n", proj) // 同项目网络（缺失时非零=正常）
		script.WriteString("exit 0\n")
		nodes = append(nodes, planNode{Key: fmt.Sprintf("uninstall-h%d", id), Kind: "cmd",
			Title:    fmt.Sprintf("%s：卸载 %s（%s；数据目录 ./data 保留）", h.Name, app, strings.Join(byHost[id], ", ")),
			Host:     id,
			Payload:  mustJSON(CmdPayload{App: app, Script: script.String(), TimeoutSec: 600}),
			TimeoutSec: 600, InvEvictHost: id, InvEvictSvcs: byHost[id]})
	}
	if len(nodes) == 0 {
		writeErr(w, http.StatusConflict, "inventory 记录的主机均不存在（可直接清库：请手工删除安装记录）")
		return
	}
	snap := &planSnapshot{Nodes: nodes, Mode: "uninstall",
		Warnings: []string{"卸载移除容器与 compose 网络；数据目录 ./data 与 named volume 恒保留（需彻底清数据请登主机手工处理）"}}
	ss := &sessionRow{App: app, Version: "uninstall", Mode: "uninstall",
		Plan: string(mustJSON(snap)), Placement: "{}"}
	if err := s.store.InsertSession(ss, snap.Nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话创建失败: "+err.Error())
		return
	}
	s.StartSession(ss.ID, ss.App, "", snap)
	fmt.Printf("[session] #%d 启动：卸载 %s（%d 台主机）\n", ss.ID, app, len(nodes))
	writeJSON(w, map[string]any{"id": ss.ID, "nodes": len(nodes)})
}

// ---- 回退（历史成功会话重放 = 介质回退；ADR-6 apply-forward） ----

func (s *Server) handleAppRollback(w http.ResponseWriter, r *http.Request, app string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		SessionID int64 `json:"sessionId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ss, err := s.store.SessionByID(req.SessionID)
	if err != nil || ss.App != app {
		writeErr(w, http.StatusNotFound, "会话不存在或不属于应用 "+app)
		return
	}
	if ss.Status != "done" {
		writeErr(w, http.StatusBadRequest, "仅成功会话可回退（当前状态 "+ss.Status+"）——失败会话的变量与分配可能是半套")
		return
	}
	if ss.PackageID == 0 {
		writeErr(w, http.StatusBadRequest, "该会话无关联部署包（合成会话不可回退）")
		return
	}
	var pl Placement
	if err := json.Unmarshal([]byte(ss.Placement), &pl); err != nil {
		writeErr(w, http.StatusBadRequest, "会话分配方案解析失败: "+err.Error())
		return
	}
	res, pkg, err := s.buildPlanRow(ss.PackageID, &pl, nil, "incremental")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "回退计划构建失败: "+err.Error())
		return
	}
	varsJSON, _ := json.Marshal(res.Vars)
	snap := &planSnapshot{Nodes: res.Nodes, EntryURL: res.EntryURL, Warnings: res.Warnings, Mode: res.Mode}
	newSS := &sessionRow{App: pkg.App, Version: pkg.Version, PackageID: ss.PackageID, PkgSha: pkg.PkgSha,
		Mode: res.Mode, Vars: string(varsJSON), Plan: string(mustJSON(snap)), Placement: ss.Placement}
	if err := s.store.InsertSession(newSS, res.Nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话创建失败: "+err.Error())
		return
	}
	s.StartSession(newSS.ID, newSS.App, pkg.Dir, snap)
	fmt.Printf("[session] #%d 启动：回退 %s → 会话 #%d 的版本 %s（介质回退，apply-forward）\n", newSS.ID, app, ss.ID, pkg.Version)
	writeJSON(w, map[string]any{"id": newSS.ID, "mode": res.Mode, "warnings": res.Warnings})
}

// ---- 统一安装预览（与执行同引擎同口径） ----

// handleSessionPreview POST /api/sessions/preview {packageId, placement, mode} →
// {mode, entryUrl, warnings, changes[], dag[]}——期望态渲染 + 对 inventory 的逐服务 diff +
// 完整 DAG 预览（不建会话不执行）。UI 升级横幅/变更清单的唯一数据源。
func (s *Server) handleSessionPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		PackageID int64     `json:"packageId"`
		Placement Placement `json:"placement"`
		Mode      string    `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	mode := orStr2(req.Mode, "incremental")
	res, err := s.buildPlan(req.PackageID, &req.Placement, nil, mode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var dag []DagNode
	for _, n := range res.Nodes {
		d := DagNode{ID: n.Key, Kind: n.Kind, Title: n.Title, Host: n.Host, Deps: n.Deps}
		if n.Kind == "cmd" {
			var p CmdPayload
			if json.Unmarshal(n.Payload, &p) == nil {
				d.Script = p.Script
			}
		}
		dag = append(dag, d)
	}
	writeJSON(w, map[string]any{"mode": res.Mode, "entryUrl": res.EntryURL, "warnings": res.Warnings,
		"changes": res.Changes, "dag": dag})
}
