// agent 专属端点（X-Agent-Token 鉴权，独立于 UI 会话）：
//
//	POST /api/agent/register   注册/刷新（引导令牌换 agent 令牌；agent 令牌可刷新能力档案）
//	GET  /api/agent/poll       长轮询领任务（?wait=25；无任务 hold 后返回 task=null）
//	POST /api/agent/report     节点状态与日志上报
//	POST /api/agent/heartbeat  心跳（在线状态 + 资源余量）
//	GET  /api/file/{sid}/{path} 包内文件分发（任务载荷引用的镜像 tar/制品/交付载荷）
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req RegisterReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Host.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "缺少 host.hostname")
		return
	}
	// 已注册：凭 agent 令牌刷新能力档案（同名主机的令牌须匹配）
	if req.AgentToken != "" {
		h, err := s.store.HostByToken(req.AgentToken)
		if err != nil || h.Revoked || h.Name != req.Host.Hostname {
			writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销——请用引导令牌重新注册")
			return
		}
		oldVer := h.Info.AgentVersion
		h.Info = req.Host
		if h.Addr == "" {
			h.Addr = firstIP(req.Host.IPs)
		}
		if oldVer != "" && req.Host.AgentVersion != "" && req.Host.AgentVersion != oldVer {
			fmt.Printf("[agent] 主机 %s agent 版本更新：%s → %s\n", h.Name, oldVer, req.Host.AgentVersion)
		}
		if err := s.store.UpsertHost(h); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, RegisterResp{AgentToken: h.AgentToken, HostID: h.ID})
		return
	}
	// 新注册：校验引导令牌
	if req.EnrollToken == "" || req.EnrollToken != s.store.GetSetting(enrollKey, "") {
		writeErr(w, http.StatusUnauthorized, "引导令牌无效（在 console 主机池页查看/轮换）")
		return
	}
	h := &hostRow{Name: req.Host.Hostname, AgentToken: randomToken(),
		Addr: firstIP(req.Host.IPs), Info: req.Host}
	if err := s.store.UpsertHost(h); err != nil {
		writeErr(w, http.StatusInternalServerError, "注册失败: "+err.Error())
		return
	}
	fmt.Printf("[agent] 主机注册：%s（%s/%s docker=%v systemd=%v java=%s）\n",
		h.Name, h.Info.OS, h.Info.Arch, h.Info.Docker, h.Info.Systemd, h.Info.JavaVer)
	writeJSON(w, RegisterResp{AgentToken: h.AgentToken, HostID: h.ID})
}

func firstIP(ips []string) string {
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

func (s *Server) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	wait := 25 * time.Second
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v >= 0 && v <= 60 {
		wait = time.Duration(v) * time.Second
	}
	task := s.popTask(host.ID, wait)
	if task != nil {
		fmt.Printf("[agent] %s ← 任务 %s/%s（%s）\n", host.Name, task.SessionID, task.NodeID, task.Kind)
	} else {
		_ = s.store.TouchHost(host.ID, nil) // 无任务轮询也算在线心跳
	}
	resp := PollResp{Task: task}
	// 自升级指令与任务平行下发（agent 先干任务、任务间隙自升级）；手动待办在版本翻绿前
	// 持续随轮询重发（agent 升级失败退避后仍能再次收到），版本达标即清位防泄漏
	if d := s.buildUpgradeDirective(host, s.hasPendingUpgrade(host.ID)); d != nil {
		fmt.Printf("[agent] %s ← 升级指令 %s（当前 %s）\n", host.Name, d.Version, host.Info.AgentVersion)
		resp.Upgrade = d
	} else if agentStamp != "" && host.Info.AgentVersion >= agentStamp {
		s.clearPendingUpgrade(host.ID)
	}
	writeJSON(w, resp)
}

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	var rep Report
	if !readJSON(w, r, &rep) {
		return
	}
	if rep.SessionID == "" || rep.NodeID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 sessionId/nodeId")
		return
	}
	sid, _ := strconv.ParseInt(rep.SessionID, 10, 64)
	if sid == 0 {
		writeErr(w, http.StatusBadRequest, "sessionId 非法")
		return
	}
	// 终端会话：剥离子 PWD 哨兵行并记忆会话工作目录（cd 跨命令生效），净行才落库
	if tr := s.terminalOf(sid); tr != nil && len(rep.Logs) > 0 {
		clean, cwd := stripCwdSentinel(rep.Logs)
		rep.Logs = clean
		if cwd != "" {
			s.mu.Lock()
			tr.cwd = cwd
			s.mu.Unlock()
		}
	}
	if run := s.session(sid); run != nil {
		run.Report(rep.NodeID, rep.Status, rep.Message, rep.Logs)
	} else {
		// 会话执行器不在内存（终端会话恒走此路；console 重启后的旧会话同）——仅落库；
		// 已终态的会话只追加日志不翻状态（终端收尾口径保持稳定，迟到结果仅留痕）
		_ = s.store.AppendLogs(sid, rep.NodeID, rep.Logs)
		if rep.Status != "" {
			if ss, err := s.store.SessionByID(sid); err == nil && ss.Status == "running" {
				_ = s.store.UpdateNodeStatus(sid, rep.NodeID, rep.Status, rep.Message)
			}
		}
	}
	resp := map[string]any{"ok": true}
	if s.terminalCancel(sid, rep.NodeID) || s.sessionAborted(sid) {
		resp["cancel"] = true // 中断指令（终端中止/会话已中止；新 agent 解析并杀进程组，旧 agent 忽略未知字段）
	}
	writeJSON(w, resp)
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	var hb Heartbeat
	if readJSON(w, r, &hb) {
		host.Info.MemMB, host.Info.DiskFreeGB = hb.MemMB, hb.DiskFreeGB
		if hb.Containers != nil { // 容器端口档案随心跳如实刷新（docker 短暂不可用时清空→预检降级）
			host.Info.Containers = hb.Containers
		}
		_ = s.store.TouchHost(host.ID, &host.Info)
	} else {
		_ = s.store.TouchHost(host.ID, nil)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAgentFile 包内文件分发：/api/file/{sessionId}/{相对路径}（zip-slip 防护）
func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/file/")
	sidStr, rel, ok := strings.Cut(rest, "/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/file/{sessionId}/{包内相对路径}")
		return
	}
	sid, err := strconv.ParseInt(sidStr, 10, 64)
	if err != nil {
		// 旧版 agent（e1fb507 前构建）拼 /api/file/{路径}（无 sessionId 段）——醒目日志防再排查难
		fmt.Printf("[file] 拒绝旧格式下载请求（首段 %q 非数字）：发起方 agent 版本过旧，请在主机池重新下载安装 agent\n", sidStr)
		writeErr(w, http.StatusBadRequest, "sessionId 非法")
		return
	}
	ss, err := s.store.SessionByID(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	pkg, err := s.store.PackageByID(ss.PackageID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "部署包不存在")
		return
	}
	clean, err := safeRelPath(rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	full := filepath.Join(pkg.Dir, filepath.FromSlash(clean))
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "文件不存在: "+clean)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, full)
}

// readJSON/writeJSON/writeErr——小工具（UI API 共用）

// handlePluginFile 插件二进制分发：GET /api/plugin/{name}/{os}（agent 令牌鉴权；占位文件拒绝）。
// 插件内嵌 console（版本与工具绑定，不随部署包走——ADR-4）；agent 下载后按 payload.sha256 缓存复用。
func (s *Server) handlePluginFile(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/plugin/"), "/")
	name, osName, ok := strings.Cut(rest, "/")
	if !ok || !pluginNameRe.MatchString(name) || (osName != "linux" && osName != "windows") {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/plugin/{插件名}/{linux|windows}")
		return
	}
	file := pluginFileName(name, osName)
	data, err := pluginBinFS(file)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if len(data) < 100*1024 {
		writeErr(w, http.StatusNotFound, "插件 "+file+" 是占位文件（<100KB）——请先执行 build.sh 完整构建 console")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	_, _ = w.Write(data)
}

var pluginNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
