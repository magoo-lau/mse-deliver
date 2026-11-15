// 主机终端会话（一个终端窗口 = 一条执行记录，App=terminal）：轻量直发通道——不进 sessionRun
// 调度器，命令节点 cmd-N 直接 enqueue（本机主机走 localCmd 进程内执行、带停止通道），report 由
// handleAgentReport 的「无内存执行器仅落库」路径承接；中断指令经 report 响应 {"cancel":true}
// 回传给新 agent（杀进程组并报 failed「已人工中断」，旧 agent 不识别、跑到超时自然结束）。
//
//	POST /api/terminal/open         {hostId} → {id}     开终端=建会话（无节点，不 StartSession）
//	POST /api/terminal/{sid}/run    {script} → {node}   执行一条命令（追加 cmd-N 节点+派发）
//	POST /api/terminal/{sid}/stop   {node}              中断在跑命令（report 响应传达/本机关停止通道）
//	GET  /api/terminal/{sid}/state                      会话+节点状态（兼终端心跳，30s 不跳=僵尸）
//	POST /api/terminal/{sid}/close                      关窗：中断在跑+未终态节点落 failed+会话终态
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	termStaleSec = 90 // 终端心跳超时（秒）：浏览器对后台标签/被遮挡窗口的定时器节流可达 1 次/分钟，
	//                     // 阈值须容忍节流（30s 会误杀活跃终端），超过才视为窗口已关自动收尾
	termTimeoutSec = 1800 // 终端命令超时（docker logs -f 等长观命令宽限 30 分钟，可随时停止）

	termStaleAgentTimeoutSec = 300 // 旧版 agent 无法远程中断：压短超时防 -f 类命令长挂
)

// staleAgentHost 主机 agent 版本落后于内嵌版本——不解析 report 响应 cancel 字段、无周期心跳，
// 终端能跑但停止指令不生效（停止响应附 stale 指引；命令超时压短）
func staleAgentHost(h *hostRow) bool {
	return !h.Local && agentStamp != "" && h.Info.AgentVersion != agentStamp
}

// terminalRun 终端会话内存态（同 queues/pendingAgentUpgrade：console 重启丢失，由
// FinalizeStaleTerminals 启动兜底收尾）
type terminalRun struct {
	hostID    int64
	seq       int
	lastSeen  int64
	cwd       string                   // 会话记忆的工作目录（cd 跨命令生效；哨兵行回传更新）
	cancel    map[string]bool          // nodeKey → 待中断（随 report 响应 {"cancel":true} 下发）
	localStop map[string]chan struct{} // 本机命令的停止通道（close 即杀进程组）
}

func newTerminalRun(hostID int64) *terminalRun {
	return &terminalRun{hostID: hostID, lastSeen: time.Now().Unix(),
		cancel: map[string]bool{}, localStop: map[string]chan struct{}{}}
}

func (s *Server) terminalOf(sid int64) *terminalRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminals[sid]
}

func (s *Server) touchTerminal(sid int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr := s.terminals[sid]; tr != nil {
		tr.lastSeen = time.Now().Unix()
	}
}

// handleTerminalOpen POST /api/terminal/open：一个终端窗口 = 一条执行记录（会话 App=terminal，
// 命令逐条追加为 cmd-N 节点）；顺带清扫僵尸终端（心跳超时自动收尾）
func (s *Server) handleTerminalOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		HostID int64 `json:"hostId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	h, err := s.store.HostByID(req.HostID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	if h.Revoked {
		writeErr(w, http.StatusBadRequest, "主机已吊销")
		return
	}
	if !h.Local && !h.OnlineCheck() {
		writeErr(w, http.StatusBadRequest, "主机离线——agent 上线后再开终端（或检查 agent 服务）")
		return
	}
	s.sweepStaleTerminals()
	snap := &planSnapshot{Mode: "fresh"}
	ss := &sessionRow{App: "terminal", Version: h.Name, PackageID: 0, Mode: "fresh",
		Plan: string(mustJSON(snap)), Placement: "{}"}
	if err := s.store.InsertSession(ss, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "终端会话创建失败: "+err.Error())
		return
	}
	s.mu.Lock()
	s.terminals[ss.ID] = newTerminalRun(h.ID)
	s.mu.Unlock()
	fmt.Printf("[terminal] #%d 开启（%s）\n", ss.ID, h.Name)
	writeJSON(w, map[string]any{"id": ss.ID})
}

// handleTerminalRun POST /api/terminal/{sid}/run：追加 cmd-N 节点并派发（远端 enqueue / 本机 localCmd）
func (s *Server) handleTerminalRun(w http.ResponseWriter, r *http.Request, sid int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Script string `json:"script"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Script = strings.TrimSpace(req.Script)
	if req.Script == "" {
		writeErr(w, http.StatusBadRequest, "命令不能为空")
		return
	}
	if len(req.Script) > execScriptMax {
		writeErr(w, http.StatusBadRequest, "命令过长（上限 64KB）")
		return
	}
	tr := s.terminalOf(sid)
	if tr == nil {
		writeErr(w, http.StatusNotFound, "终端会话不存在或已关闭")
		return
	}
	s.touchTerminal(sid) // 先刷新心跳再清扫：本终端必不在清扫名单（否则节流间隙内自伤）
	s.sweepStaleTerminals()
	h, err := s.store.HostByID(tr.hostID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "主机记录已删除")
		return
	}
	if h.Revoked {
		writeErr(w, http.StatusBadRequest, "主机已吊销")
		return
	}
	if !h.Local && !h.OnlineCheck() {
		writeErr(w, http.StatusBadRequest, "主机离线——agent 上线后再试")
		return
	}
	s.mu.Lock()
	tr.seq++
	key := fmt.Sprintf("cmd-%d", tr.seq)
	s.mu.Unlock()
	title := execNodeTitle(req.Script)
	if err := s.store.AddSessionNode(sid, key, h.ID, "cmd", title, "dispatched"); err != nil {
		writeErr(w, http.StatusInternalServerError, "命令节点登记失败: "+err.Error())
		return
	}
	timeout := termTimeoutSec
	if staleAgentHost(h) {
		timeout = termStaleAgentTimeoutSec
	}
	// 工作目录记忆：CmdPayload.Cwd=会话记忆目录直接落到进程工作目录；TrackCwd 让 agent/本机
	// 执行体在脚本末尾追加 PWD 哨兵行回传，report 落库前剥离并更新会话目录——cd 跨命令生效
	s.mu.Lock()
	cwd := tr.cwd
	s.mu.Unlock()
	p := CmdPayload{Script: req.Script, TimeoutSec: timeout, Cwd: cwd, TrackCwd: true}
	if h.Local {
		stop := make(chan struct{})
		s.mu.Lock()
		tr.localStop[key] = stop
		s.mu.Unlock()
		go s.terminalLocalRun(sid, key, p, stop)
	} else {
		task := &Task{SessionID: fmt.Sprint(sid), NodeID: key, Kind: "cmd", Payload: mustJSON(p)}
		if err := s.enqueue(h.ID, task); err != nil {
			_ = s.store.UpdateNodeStatus(sid, key, "failed", err.Error())
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"node": key})
}

// terminalLocalRun 本机终端命令：localCmd 进程内执行（带停止通道），上报直写库（会话关闭后仅留日志）
func (s *Server) terminalLocalRun(sid int64, key string, p CmdPayload, stop chan struct{}) {
	s.mu.Lock()
	tr := s.terminals[sid]
	s.mu.Unlock()
	if tr != nil {
		defer func() {
			s.mu.Lock()
			delete(tr.localStop, key)
			s.mu.Unlock()
		}()
	}
	report := func(status, msg string, logs []string) {
		if len(logs) > 0 {
			clean, cwd := stripCwdSentinel(logs)
			if cwd != "" && tr != nil {
				s.mu.Lock()
				tr.cwd = cwd
				s.mu.Unlock()
			}
			if len(clean) > 0 {
				_ = s.store.AppendLogs(sid, key, clean)
			}
		}
		if status == "" {
			return
		}
		if ss, err := s.store.SessionByID(sid); err == nil && ss.Status == "running" {
			_ = s.store.UpdateNodeStatus(sid, key, status, msg)
		}
	}
	report("running", "", []string{"[local] " + execNodeTitle(p.Script)})
	if err := s.localCmd(p, stop, report); err != nil {
		report("failed", err.Error(), nil)
	} else {
		report("ok", "", nil)
	}
}

// handleTerminalStop POST /api/terminal/{sid}/stop {node}：置中断标记（随 report 响应传达新 agent；
// 本机关停止通道；agent 离线未领取的直接落 failed 不再等）
func (s *Server) handleTerminalStop(w http.ResponseWriter, r *http.Request, sid int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Node string `json:"node"`
	}
	if !readJSON(w, r, &req) || req.Node == "" {
		writeErr(w, http.StatusBadRequest, "缺少 node")
		return
	}
	s.touchTerminal(sid)
	s.mu.Lock()
	tr := s.terminals[sid]
	if tr != nil && !tr.cancel[req.Node] {
		tr.cancel[req.Node] = true
		if ch, ok := tr.localStop[req.Node]; ok {
			close(ch)
			delete(tr.localStop, req.Node)
		}
	}
	s.mu.Unlock()
	if tr == nil {
		writeErr(w, http.StatusNotFound, "终端会话不存在或已关闭")
		return
	}
	h, err := s.store.HostByID(tr.hostID)
	if err != nil {
		h = &hostRow{}
	}
	// agent 离线且任务未领取：没有 report 往返可承载中断，直接落终态
	if st, err := s.store.NodeStatus(sid, req.Node); err == nil && st == "dispatched" && !h.Local && !h.OnlineCheck() {
		_ = s.store.UpdateNodeStatus(sid, req.Node, "failed", "已停止（主机离线，任务未领取）")
	}
	resp := map[string]any{"ok": true}
	if staleAgentHost(h) {
		// 中断标记照置（agent 若已换代即生效），但明确告知当前执行体收不到
		resp["stale"] = true
		resp["advice"] = "该命令由旧版 agent 执行，无法远程中断——请先在主机池「升级」agent（升级在该命令结束后生效）；急停可到目标机执行 systemctl restart deliver-agent 立即终止"
	}
	writeJSON(w, resp)
}

// handleTerminalState GET /api/terminal/{sid}/state：会话+节点状态；兼终端心跳；顺带清扫僵尸终端。
// closed=true 表示会话已收尾（前端停轮询）。
func (s *Server) handleTerminalState(w http.ResponseWriter, r *http.Request, sid int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	s.touchTerminal(sid) // 先刷新心跳再清扫：本终端必不在清扫名单（否则节流间隙内自伤）
	s.sweepStaleTerminals()
	ss, err := s.store.SessionByID(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "终端会话不存在")
		return
	}
	nodes, err := s.store.SessionNodes(sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	cwd, alive := "", s.terminals[sid] != nil
	if tr := s.terminals[sid]; tr != nil {
		cwd = tr.cwd
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"session": ss, "nodes": nodes, "cwd": cwd,
		"closed": !alive || ss.Status != "running"})
}

// handleTerminalClose POST /api/terminal/{sid}/close：关窗收尾（幂等）——中断在跑命令、未终态节点
// 落 failed、会话终态（有失败节点=failed）并从内存表移除；此后 report 只追加日志不翻状态
func (s *Server) handleTerminalClose(w http.ResponseWriter, r *http.Request, sid int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.closeTerminal(sid, "终端已关闭")
	writeJSON(w, map[string]any{"ok": true})
}

// closeTerminal 终端收尾（关窗/僵尸清扫共用）：幂等
func (s *Server) closeTerminal(sid int64, reason string) {
	s.mu.Lock()
	tr, ok := s.terminals[sid]
	if ok {
		delete(s.terminals, sid)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	nodes, err := s.store.SessionNodes(sid)
	if err != nil {
		return
	}
	failed := 0
	for _, n := range nodes {
		if n.Status == "failed" {
			failed++
		}
		if n.Status == "pending" || n.Status == "dispatched" || n.Status == "running" {
			s.mu.Lock()
			tr.cancel[n.Key] = true
			if ch, ok := tr.localStop[n.Key]; ok {
				close(ch)
				delete(tr.localStop, n.Key)
			}
			s.mu.Unlock()
			_ = s.store.UpdateNodeStatus(sid, n.Key, "failed", reason)
			failed++
		}
	}
	status := "done"
	if failed > 0 {
		status = "failed"
	}
	_ = s.store.UpdateSessionStatus(sid, status, fmt.Sprintf("%d 条命令（%d 失败）· %s", len(nodes), failed, reason))
	fmt.Printf("[terminal] #%d 关闭：%s\n", sid, reason)
}

// sweepStaleTerminals 僵尸终端清扫：心跳超过 termStaleSec 未跳（浏览器崩溃/断网关不掉窗口）自动收尾
func (s *Server) sweepStaleTerminals() {
	now := time.Now().Unix()
	s.mu.Lock()
	var stale []int64
	for sid, tr := range s.terminals {
		if now-tr.lastSeen > termStaleSec {
			stale = append(stale, sid)
			delete(s.terminals, sid)
		}
	}
	s.mu.Unlock()
	for _, sid := range stale {
		s.closeTerminal(sid, "连接断开，自动结束")
	}
}

// terminalCancel 该终端节点是否被要求中断（handleAgentReport 响应捎带）
func (s *Server) terminalCancel(sid int64, node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tr, ok := s.terminals[sid]
	return ok && tr.cancel[node]
}

// stripCwdSentinel 剥离日志中的 PWD 哨兵行，返回净行与解析出的会话工作目录（无哨兵时 cwd 为空；
// 哨兵由 agent/本机执行体在脚本末尾追加，exit 提前退出的命令无哨兵=会话目录不变）
func stripCwdSentinel(lines []string) ([]string, string) {
	cwd := ""
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, "\x01CWD:") && strings.HasSuffix(l, "\x02") {
			cwd = strings.TrimSuffix(strings.TrimPrefix(l, "\x01CWD:"), "\x02")
			continue
		}
		out = append(out, l)
	}
	return out, cwd
}
