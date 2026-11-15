// agent 自升级（console 侧）：poll 响应注入升级指令（与任务平行，不经会话队列/上报）、
// 内嵌 agent 二进制 sha256 缓存与 agent 令牌分发端点。触发两路（满足任一即注入）：
// ① 主机池「自动升级旧版 agent」开关（settings 持久化，缺省关）；② 主机池红标行「升级 agent」
// 按钮置的 pending 标记（内存态，注入即消费——console 重启丢失可重点）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// agentAutoUpgradeKey settings 开关键（"1"/"0"，缺省关=仅手动按钮触发）
const agentAutoUpgradeKey = "agent_auto_upgrade"

// agentBinName 目标 OS → agent-bin 内文件名（与 embed.go 命名一致）
func agentBinName(goos string) string {
	if goos == "windows" {
		return "deliver-agent-windows.exe"
	}
	return "deliver-agent-linux"
}

var (
	agentBinSHAMu    sync.Mutex
	agentBinSHACache = map[string]string{}
)

// agentBinSHA 内嵌 agent 二进制 sha256（每平台仅算一次缓存；占位文件 <100KB 返回 ""——
// 测试/dev 构建未跑完整 build 时禁用升级注入，防止 agent 拿占位文件替换自身）
func agentBinSHA(goos string) string {
	agentBinSHAMu.Lock()
	defer agentBinSHAMu.Unlock()
	if v, ok := agentBinSHACache[goos]; ok {
		return v
	}
	data, err := agentBinFS(agentBinName(goos))
	if err != nil || len(data) < 100*1024 {
		agentBinSHACache[goos] = ""
		return ""
	}
	sum := sha256.Sum256(data)
	v := hex.EncodeToString(sum[:])
	agentBinSHACache[goos] = v
	return v
}

// buildUpgradeDirective 判定是否向该主机下发升级指令（nil=不下发）。守卫：内嵌戳缺失
// （测试/dev 构建）/本机节点/已吊销/OS 未知/版本未知或不低于内嵌（只升不降——yyyyMMddHHmm
// 定宽数字串字典序即时间序，agent 新于内嵌=console 回退场景）/二进制占位，均不下发。
func (s *Server) buildUpgradeDirective(h *hostRow, pending bool) *UpgradeDirective {
	if agentStamp == "" || h.Local || h.Revoked {
		return nil
	}
	if h.Info.OS != "linux" && h.Info.OS != "windows" {
		return nil
	}
	if h.Info.AgentVersion == "" || h.Info.AgentVersion >= agentStamp {
		return nil
	}
	if s.store.GetSetting(agentAutoUpgradeKey, "") != "1" && !pending {
		return nil
	}
	sha := agentBinSHA(h.Info.OS)
	if sha == "" {
		return nil
	}
	return &UpgradeDirective{Version: agentStamp, OS: h.Info.OS, SHA256: sha}
}

// ---- 手动升级待办标记（内存态） ----

func (s *Server) setPendingUpgrade(hostID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingAgentUpgrade[hostID] = true
}

func (s *Server) hasPendingUpgrade(hostID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingAgentUpgrade[hostID]
}

func (s *Server) clearPendingUpgrade(hostID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pendingAgentUpgrade, hostID)
}

// handleAgentBinary agent 自身二进制分发：GET /api/agent/binary/{linux|windows}（agent 令牌
// 鉴权；占位文件拒绝——模板同 handlePluginFile）
func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	host := s.agentAuth(r)
	if host == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	osName := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent/binary/"), "/")
	if osName != "linux" && osName != "windows" {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/agent/binary/{linux|windows}")
		return
	}
	file := agentBinName(osName)
	data, err := agentBinFS(file)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if len(data) < 100*1024 {
		writeErr(w, http.StatusNotFound, file+" 是占位文件（<100KB）——请先执行 build.sh 完整构建 console")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	_, _ = w.Write(data)
}
