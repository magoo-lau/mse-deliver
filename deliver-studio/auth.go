// 登录与会话（单管理员 admin，账号密码登录）：
//
//	settings 表存 admin_pass = "salt:hash"（随机 salt + 万轮 sha256 拉伸；本地工具级防护，
//	不引 x/crypto——守住工作台「仅 modernc.org/sqlite」的依赖口径）。
//	首次启动无该键时生成随机密码并打印控制台（见 EnsureAdminPassword），登录后可在界面修改。
//	会话 token：crypto/rand 32B hex，内存表 + 7 天滑动续期（重启进程即全部失效）；
//	鉴权中间件包住全部 /api/*，放行清单仅 POST /api/login 与 GET /api/whoami；
//	凭据头 Authorization: Bearer <token> 或 X-Studio-Token（collect-kit 推送用后者等价）。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	adminSettingKey = "admin_pass"
	adminUser       = "admin"
	tokenTTL        = 7 * 24 * time.Hour
	passMinLen      = 6
	passRounds      = 10000
)

type Auth struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token → 过期时刻
	store  *Store
}

func NewAuth(store *Store) *Auth {
	return &Auth{tokens: map[string]time.Time{}, store: store}
}

// ---- 密码哈希（salt:hash；stretch = sha256 迭代拉伸，防直接彩虹表/暴力字典的廉价破解） ----

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

// EnsureAdminPassword 首次启动初始化：无密码记录时生成随机密码落库并返回（调用方负责醒目打印）
func EnsureAdminPassword(store *Store) string {
	if store.GetSetting(adminSettingKey, "") != "" {
		return ""
	}
	pass := randomPassword(10)
	store.PutSetting(adminSettingKey, newPassRecord(pass))
	return pass
}

// ResetAdminPassword 忘密恢复（-reset-pass 启动参数）：强制生成新随机密码并吊销隐含旧凭据
func ResetAdminPassword(store *Store) string {
	pass := randomPassword(10)
	store.PutSetting(adminSettingKey, newPassRecord(pass))
	return pass
}

// ---- token 生命周期 ----

func (a *Auth) issue() (string, time.Time) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	expire := time.Now().Add(tokenTTL)
	a.mu.Lock()
	a.tokens[token] = expire
	a.mu.Unlock()
	return token, expire
}

func (a *Auth) valid(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expire, ok := a.tokens[token]
	return ok && time.Now().Before(expire)
}

func (a *Auth) touch(token string) {
	a.mu.Lock()
	if expire, ok := a.tokens[token]; ok && time.Now().Before(expire) {
		a.tokens[token] = time.Now().Add(tokenTTL)
	}
	a.mu.Unlock()
}

func (a *Auth) revoke(token string) {
	a.mu.Lock()
	delete(a.tokens, token)
	a.mu.Unlock()
}

func (a *Auth) revokeAll() {
	a.mu.Lock()
	a.tokens = map[string]time.Time{}
	a.mu.Unlock()
}

// ---- 鉴权中间件 ----

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("X-Studio-Token"))
}

// pkgDownloadPathRe 带下载票的包下载路由形态（GB 级文件由浏览器原生下载器 GET，带不了
// token 头——形态级放行，票有效性由 handlePackageDownload 校验）
var pkgDownloadPathRe = regexp.MustCompile(`^/api/packages/\d+/download$`)

func isPublicPath(r *http.Request) bool {
	switch {
	case r.URL.Path == "/api/login" && r.Method == http.MethodPost,
		r.URL.Path == "/api/whoami" && r.Method == http.MethodGet,
		r.Method == http.MethodGet && pkgDownloadPathRe.MatchString(r.URL.Path) && r.URL.Query().Get("t") != "":
		return true
	}
	return false
}

// Middleware 包住整个 mux：/api/* 需有效 token（放行清单除外），其余（SPA 静态资源）直通
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		token := tokenFrom(r)
		if token == "" || !a.valid(token) {
			writeErr(w, 401, fmt.Errorf("未登录或会话已过期（请登录；外部工具推送请先 POST /api/login 换 token）"))
			return
		}
		a.touch(token) // 滑动续期
		next.ServeHTTP(w, r)
	})
}

// ---- handlers ----

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	user := strings.TrimSpace(req.User)
	if user != "" && user != adminUser {
		writeErr(w, 401, fmt.Errorf("账号或密码不正确"))
		return
	}
	record := a.store.GetSetting(adminSettingKey, "")
	if record == "" {
		writeErr(w, 500, fmt.Errorf("管理员密码未初始化（请重启工作台自动生成）"))
		return
	}
	if !verifyPassword(record, req.Password) {
		writeErr(w, 401, fmt.Errorf("账号或密码不正确"))
		return
	}
	token, expire := a.issue()
	writeJSON(w, 200, map[string]any{"token": token, "user": adminUser, "expireAt": expire.UnixMilli()})
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := tokenFrom(r); token != "" {
		a.revoke(token)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleWhoami 探活（免鉴权）：凭当前 token 报告登录态，SPA 据此决定登录页/主界面
func (a *Auth) handleWhoami(w http.ResponseWriter, r *http.Request) {
	token := tokenFrom(r)
	writeJSON(w, 200, map[string]any{"loggedIn": token != "" && a.valid(token), "user": adminUser})
}

// handlePasswordChange 修改密码（校验旧密；成功后吊销全部会话，强制重新登录）
func (a *Auth) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	record := a.store.GetSetting(adminSettingKey, "")
	if record == "" || !verifyPassword(record, req.OldPassword) {
		writeErr(w, 400, fmt.Errorf("当前密码不正确"))
		return
	}
	if len(req.NewPassword) < passMinLen {
		writeErr(w, 400, fmt.Errorf("新密码至少 %d 位", passMinLen))
		return
	}
	a.store.PutSetting(adminSettingKey, newPassRecord(req.NewPassword))
	a.revokeAll()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
