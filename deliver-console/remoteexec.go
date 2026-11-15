// 主机运维：远程执行命令 + 主机执行历史 + 快捷脚本库（主机池「执行 / 日志」入口）：
//
//	POST   /api/exec                  {hostIds:[], script, timeoutSec?, title?} → 单台/批量执行
//	GET    /api/hosts/{id}/sessions   主机执行历史（含该主机节点的会话清单）
//	GET    /api/exec-presets          快捷脚本库 {builtin, custom}
//	POST   /api/exec-presets          保存自定义快捷脚本 {name, os?, script}
//	DELETE /api/exec-presets/{id}     删除自定义快捷脚本
//
// 复用既有 cmd 任务类型（agent execCmd / 本机 localCmd：双平台 sh|powershell、可配超时、
// 流式日志回传）——零协议新增、旧 agent 全兼容；照 dockersetup.go 一键装先例合成单节点
// 会话（App=exec），自动获得拓扑调度/看门狗/重试/日志落库，并留档「安装记录」页。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	execPresetsKey = "exec_presets"    // settings 键：自定义快捷脚本库（JSON 数组）
	execScriptMax  = 64 << 10          // 脚本长度上限
	execTitleMax   = 60                // 备注长度上限（字符）
	execNameMax    = 30                // 快捷脚本名长度上限
	execHostMax    = 50                // 单次批量主机数上限
	execTimeoutDef = 600               // 缺省超时（秒）
	execHistoryLim = 50                // 主机执行历史条数上限
)

// execPreset 快捷脚本：OS 空=全平台；内置项 ID 以 b- 前缀（不可删），自定义项 c<毫秒戳>。
type execPreset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os,omitempty"`
	Script string `json:"script"`
}

// builtinExecPresets 内置快捷脚本（随版本发布；常用巡检命令，按目标机 OS 过滤展示）
var builtinExecPresets = []execPreset{
	{ID: "b-docker-ps", Name: "Docker 容器", Script: "docker ps"},
	{ID: "b-docker-df", Name: "Docker 磁盘占用", Script: "docker system df"},
	{ID: "b-linux-df", Name: "磁盘空间", OS: "linux", Script: "df -h"},
	{ID: "b-linux-free", Name: "内存占用", OS: "linux", Script: "free -h"},
	{ID: "b-linux-uptime", Name: "系统负载", OS: "linux", Script: "uptime"},
	{ID: "b-linux-apps", Name: "应用目录清单", OS: "linux", Script: "ls -la /opt/mse/deliver/apps"},
	{ID: "b-linux-hostdate", Name: "主机名与时间", OS: "linux", Script: "hostname; date"},
	{ID: "b-win-volume", Name: "磁盘空间", OS: "windows", Script: "Get-Volume | Format-Table -AutoSize"},
	{ID: "b-win-mem", Name: "内存占用", OS: "windows", Script: `Get-CimInstance Win32_OperatingSystem | ForEach-Object { "总计 {0:N1}GB 可用 {1:N1}GB" -f ($_.TotalVisibleMemorySize/1MB), ($_.FreePhysicalMemory/1MB) }`},
	{ID: "b-win-hostdate", Name: "主机名与时间", OS: "windows", Script: "hostname; Get-Date"},
}

// handleExec POST /api/exec：单台/批量远程执行（每台合成一个单节点 cmd 会话；逐台校验，
// 部分成功——失败台在 items 内带回明确原因，合法台照常执行）
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		HostIDs    []int64 `json:"hostIds"`
		Script     string  `json:"script"`
		TimeoutSec int     `json:"timeoutSec"`
		Title      string  `json:"title"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Script, req.Title = strings.TrimSpace(req.Script), strings.TrimSpace(req.Title)
	if req.Script == "" {
		writeErr(w, http.StatusBadRequest, "脚本内容不能为空")
		return
	}
	if len(req.Script) > execScriptMax {
		writeErr(w, http.StatusBadRequest, "脚本过长（上限 64KB）")
		return
	}
	if len([]rune(req.Title)) > execTitleMax {
		writeErr(w, http.StatusBadRequest, "备注过长（上限 60 字）")
		return
	}
	if len(req.HostIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择主机")
		return
	}
	if len(req.HostIDs) > execHostMax {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("一次最多 %d 台主机", execHostMax))
		return
	}
	timeout := req.TimeoutSec
	if timeout == 0 {
		timeout = execTimeoutDef
	}
	if timeout < 30 || timeout > 3600 {
		writeErr(w, http.StatusBadRequest, "超时须在 30–3600 秒")
		return
	}
	title := orStr(req.Title, execNodeTitle(req.Script))
	items := make([]map[string]any, 0, len(req.HostIDs))
	seen := map[int64]bool{}
	for _, id := range req.HostIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		item := map[string]any{"hostId": id}
		h, err := s.store.HostByID(id)
		if err != nil {
			item["error"] = "主机不存在"
			items = append(items, item)
			continue
		}
		item["hostName"] = h.Name
		switch {
		case h.Revoked:
			item["error"] = "主机已吊销"
		case !h.Local && !h.OnlineCheck():
			item["error"] = "主机离线——agent 上线后再试（或检查 agent 服务）"
		default:
			node := planNode{Key: "exec", Kind: "cmd", Title: h.Name + "：执行 " + title, Host: h.ID,
				Payload: mustJSON(CmdPayload{Script: req.Script, TimeoutSec: timeout}), TimeoutSec: timeout}
			snap := &planSnapshot{Nodes: []planNode{node}, Mode: "fresh"}
			ss := &sessionRow{App: "exec", Version: h.Name, PackageID: 0, Mode: "fresh",
				Plan: string(mustJSON(snap)), Placement: "{}"}
			if err := s.store.InsertSession(ss, snap.Nodes); err != nil {
				item["error"] = "会话创建失败: " + err.Error()
			} else {
				s.StartSession(ss.ID, ss.App, "", snap)
				fmt.Printf("[session] #%d 启动：远程执行（%s，%d 台批量之一）\n", ss.ID, h.Name, len(req.HostIDs))
				item["sessionId"] = ss.ID
			}
		}
		items = append(items, item)
	}
	writeJSON(w, map[string]any{"items": items})
}

// execNodeTitle 无备注时取脚本首行做节点标题（超长截断）
func execNodeTitle(script string) string {
	line := ""
	for _, l := range strings.Split(script, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			line = t
			break
		}
	}
	if rs := []rune(line); len(rs) > 40 {
		return string(rs[:40]) + "…"
	}
	return line
}

// handleHostSessions GET /api/hosts/{id}/sessions：主机执行历史（该主机上有节点的会话，
// 含各会话落在本主机上的节点行——状态/日志数就地可见，日志经 /api/sessions/{id}/logs?node= 细查）
func (s *Server) handleHostSessions(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, err := s.store.HostByID(id); err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	entries, err := s.store.HostSessions(id, execHistoryLim)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": entries})
}

// handleExecPresets GET=清单（内置+自定义） / POST=保存自定义（os 随目标主机平台，空=全平台）
func (s *Server) handleExecPresets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"builtin": builtinExecPresets, "custom": s.customExecPresets()})
	case http.MethodPost:
		var req execPreset
		if !readJSON(w, r, &req) {
			return
		}
		req.Name, req.OS = strings.TrimSpace(req.Name), strings.TrimSpace(req.OS)
		if req.Name == "" || strings.TrimSpace(req.Script) == "" {
			writeErr(w, http.StatusBadRequest, "缺少名称或脚本")
			return
		}
		if len([]rune(req.Name)) > execNameMax {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("名称过长（上限 %d 字）", execNameMax))
			return
		}
		if len(req.Script) > execScriptMax {
			writeErr(w, http.StatusBadRequest, "脚本过长（上限 64KB）")
			return
		}
		if req.OS != "" && req.OS != "linux" && req.OS != "windows" {
			writeErr(w, http.StatusBadRequest, "os 须为 linux|windows 或留空（全平台）")
			return
		}
		list := s.customExecPresets()
		req.ID = fmt.Sprintf("c%d", time.Now().UnixMilli())
		list = append(list, req)
		if err := s.store.PutSetting(execPresetsKey, string(mustJSON(list))); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"preset": req})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "GET/POST only")
	}
}

// handleExecPresetDelete DELETE /api/exec-presets/{id}：仅自定义项可删（内置随版本发布）
func (s *Server) handleExecPresetDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	list := s.customExecPresets()
	kept := list[:0]
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "快捷脚本不存在（内置脚本不可删除）")
		return
	}
	if err := s.store.PutSetting(execPresetsKey, string(mustJSON(kept))); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// customExecPresets 读自定义快捷脚本库（settings JSON；坏数据回空清单，下次保存整体覆写自愈）
func (s *Server) customExecPresets() []execPreset {
	raw := s.store.GetSetting(execPresetsKey, "")
	if raw == "" {
		return nil
	}
	var list []execPreset
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	return list
}
