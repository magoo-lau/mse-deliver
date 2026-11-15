// 登录与令牌（console 双令牌校验）：
//
//	① UI 登录：单管理员 admin，settings 表存 admin_pass="salt:hash"（随机 salt + 万轮 sha256 拉伸，
//	  与 deliver-studio 同款实现），首启生成随机密码打印控制台；会话 token 内存表 + 7 天滑动续期。
//	② 引导令牌（enroll）：settings 表 enroll_token，首启生成打印，UI 可轮换（轮换拒绝新注册，
//	  不影响已注册 agent）；仅用于 agent 首次注册换 agent 令牌。
//	③ agent 令牌：注册时签发（crypto/rand 32B hex），hosts.agent_token 落库，poll/report/文件下载
//	  全部经 X-Agent-Token 校验，可按主机吊销（吊销后 401，agent 侧凭引导令牌重注兜底）。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	adminPassKey = "admin_pass"
	enrollKey    = "enroll_token"
	tokenTTL     = 7 * 24 * time.Hour
	passMinLen   = 6
	passRounds   = 10000
)

type Auth struct {
	mu     sync.Mutex
	tokens map[string]time.Time
	store  *Store
}

func NewAuth(store *Store) *Auth {
	return &Auth{tokens: map[string]time.Time{}, store: store}
}

// ---- 密码哈希（salt:hash；sha256 迭代拉伸——本地工具级防护，守住仅 sqlite 依赖口径） ----

func stretch(pass string, salt []byte) string {
	h := sha256.Sum256(append(append([]byte{}, salt...), []byte(pass)...))
	for i := 0; i < passRounds; i++ {
		h = sha256.Sum256(h[:])
	}
	return hex.EncodeToString(h[:])
}

func newPassRecord(pass string) string {
	salt := make([]byte, 8)
	_, _ = rand.Read(salt)
	return hex.EncodeToString(salt) + ":" + stretch(pass, salt)
}

func verifyPassword(record, pass string) bool {
	saltHex, want, ok := strings.Cut(record, ":")
	if !ok {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	return stretch(pass, salt) == want
}

func randomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func randomPassword(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// EnsureAdminPassword 首启初始化：返回生成的随机密码（调用方醒目打印），已有则返回空
func EnsureAdminPassword(store *Store) string {
	if store.GetSetting(adminPassKey, "") != "" {
		return ""
	}
	pass := randomPassword(10)
	_ = store.PutSetting(adminPassKey, newPassRecord(pass))
	return pass
}

func ResetAdminPassword(store *Store) string {
	pass := randomPassword(10)
	_ = store.PutSetting(adminPassKey, newPassRecord(pass))
	return pass
}

// ---- 引导令牌 ----

func EnsureEnrollToken(store *Store) string {
	if v := store.GetSetting(enrollKey, ""); v != "" {
		return ""
	}
	tok := randomPassword(16)
	_ = store.PutSetting(enrollKey, tok)
	return tok
}

func RotateEnrollToken(store *Store) string {
	tok := randomPassword(16)
	_ = store.PutSetting(enrollKey, tok)
	return tok
}

// ---- UI 会话 ----

func (a *Auth) issue() string {
	token := randomToken()
	a.mu.Lock()
	a.tokens[token] = time.Now().Add(tokenTTL)
	a.mu.Unlock()
	return token
}

func (a *Auth) valid(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expire, ok := a.tokens[token]
	if ok && time.Now().Before(expire) {
		a.tokens[token] = time.Now().Add(tokenTTL) // 滑动续期
		return true
	}
	return false
}

func (a *Auth) revoke(token string) {
	a.mu.Lock()
	delete(a.tokens, token)
	a.mu.Unlock()
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Console-Token")
}

// Middleware 登录校验：只拦 /api/* 与 /download/*（agent 二进制下载，登录态）；放行 login/whoami
// 与 agent 专属端点（agent 侧自带 X-Agent-Token 校验）。前端静态资源（/、/assets/*——登录页所在）
// 必须匿名可达，否则登录页本身被拦 401、无从登录。
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/download/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && path == "/api/login" ||
			r.Method == http.MethodGet && path == "/api/whoami" ||
			strings.HasPrefix(path, "/api/agent/") ||
			strings.HasPrefix(path, "/api/file/") ||
			strings.HasPrefix(path, "/api/plugin/") {
			next.ServeHTTP(w, r)
			return
		}
		if !a.valid(bearerToken(r)) {
			writeErr(w, http.StatusUnauthorized, "未登录或会话过期")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agentAuth agent 令牌校验（X-Agent-Token → hosts 表；吊销/未知 → 401）
func (s *Server) agentAuth(r *http.Request) *hostRow {
	h, err := s.store.HostByToken(r.Header.Get("X-Agent-Token"))
	if err != nil || h.Revoked {
		return nil
	}
	return h
}

// ---- handlers ----

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !verifyPassword(a.store.GetSetting(adminPassKey, ""), req.Password) {
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	writeJSON(w, map[string]any{"token": a.issue()})
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.revoke(bearerToken(r))
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Auth) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"authed": a.valid(bearerToken(r))})
}

func (a *Auth) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.New) < passMinLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("新密码至少 %d 位", passMinLen))
		return
	}
	if !verifyPassword(a.store.GetSetting(adminPassKey, ""), req.Old) {
		writeErr(w, http.StatusBadRequest, "旧密码错误")
		return
	}
	_ = a.store.PutSetting(adminPassKey, newPassRecord(req.New))
	writeJSON(w, map[string]any{"ok": true})
}
