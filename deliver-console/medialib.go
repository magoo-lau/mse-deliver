// 版本化介质库：docker / docker-compose 安装介质统一托管（console 全局，主机池页上传维护）。
//
// 存储布局：{root}/media/{tool}/{槽位}/{版本} 文件——docker 槽位存官方 static 包合并 tar.gz
// （{版本}.tar.gz），compose 槽位存 docker compose 插件原始二进制（{版本}）。
// meta 落 settings（media_docker / media_compose）：{"linux-amd64": {"27.5.1": {name,sha256,size,uploadedAt}}}。
//
// agent 统一下载端点 GET /api/agent/media/{tool}/{os}/{arch}?v={version}（X-Agent-Token；
// v 缺省=该槽最新真实版本）；旧 /api/agent/docker-media/{os}/{arch} 保留为别名（服务最新版本，
// 旧 agent / 在途会话零影响）。旧单介质数据（docker_media 键 + docker-media/{槽位}.tar.gz 文件）
// 首次访问一次性迁移为 legacy 版本条目（UI 显示「未标注版本」、不参与精确版本匹配，可删后重传带版本的）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	mediaToolDocker  = "docker"
	mediaToolCompose = "compose"
	mediaDockerKey   = "media_docker"
	mediaComposeKey  = "media_compose"
	legacyMediaKey   = "docker_media" // 旧单介质 settings 键（迁移源，迁移后清除）
	mediaLegacyVer   = "legacy"       // 迁移条目版本标（UI 显示「未标注版本」，不参与精确匹配）
)

var (
	mediaSlotRe = regexp.MustCompile(`^linux-(amd64|arm64)$`)
	mediaVerRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,63}$`)
)

// DockerMediaMeta 介质登记项（settings media_* JSON；文件在 console-data/media/{tool}/ 下）
type DockerMediaMeta struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	UploadedAt int64  `json:"uploadedAt"`
}

// mediaLib 版本化介质库：槽位（os-arch）→ 版本 → 登记项
type mediaLib map[string]map[string]DockerMediaMeta

func validMediaTool(tool string) bool { return tool == mediaToolDocker || tool == mediaToolCompose }

func mediaSettingKey(tool string) string {
	if tool == mediaToolCompose {
		return mediaComposeKey
	}
	return mediaDockerKey
}

// mediaLib 读取介质库（docker 工具含旧单介质数据懒迁移）
func (s *Server) mediaLib(tool string) mediaLib {
	lib := mediaLib{}
	if raw := s.store.GetSetting(mediaSettingKey(tool), ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &lib)
	}
	if lib == nil {
		lib = mediaLib{}
	}
	if tool == mediaToolDocker {
		s.migrateLegacyDockerMedia(lib)
	}
	return lib
}

func (s *Server) saveMediaLib(tool string, lib mediaLib) error {
	return s.store.PutSetting(mediaSettingKey(tool), string(mustJSON(lib)))
}

// migrateLegacyDockerMedia 旧单介质数据（每槽一份 docker-media/{槽位}.tar.gz）→ legacy 版本条目
// （文件挪 media/docker/{槽位}/legacy.tar.gz）；幂等，源键空即无事
func (s *Server) migrateLegacyDockerMedia(lib mediaLib) {
	raw := s.store.GetSetting(legacyMediaKey, "")
	if raw == "" {
		return
	}
	old := map[string]*DockerMediaMeta{}
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		return // 旧数据损坏：留键不迁移，人工介入
	}
	moved := 0
	for slot, m := range old {
		if m == nil {
			continue
		}
		if _, exists := lib[slot][mediaLegacyVer]; exists {
			continue
		}
		dst := s.mediaFilePath(mediaToolDocker, slot, mediaLegacyVer)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			continue
		}
		src := filepath.Join(s.root, "docker-media", slot+".tar.gz")
		if err := os.Rename(src, dst); err != nil {
			if _, err2 := os.Stat(dst); err2 != nil {
				continue // 源文件缺失且目标无：丢弃孤儿 meta
			}
		}
		if lib[slot] == nil {
			lib[slot] = map[string]DockerMediaMeta{}
		}
		lib[slot][mediaLegacyVer] = *m
		moved++
	}
	if err := s.saveMediaLib(mediaToolDocker, lib); err != nil {
		return
	}
	_ = s.store.DelSetting(legacyMediaKey)
	if moved > 0 {
		fmt.Printf("[media] 旧 docker 离线介质已迁移为「未标注版本」条目（%d 槽位，可删后重传带版本的）\n", moved)
	}
}

// mediaFilePath 介质文件路径（docker={版本}.tar.gz，compose=原始插件二进制 {版本}）
func (s *Server) mediaFilePath(tool, slot, ver string) string {
	name := ver
	if tool == mediaToolDocker {
		name += ".tar.gz"
	}
	return filepath.Join(s.root, "media", tool, slot, name)
}

// normVer 版本归一（去空白与前导 v/V——docker server 报 27.5.1、compose --short 报 2.30.3、
// 介质版本标签常带 v 前缀）
func normVer(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		v = v[1:]
	}
	return v
}

// verEqual 版本等价判定（归一后比较；空串不等于任何值）
func verEqual(a, b string) bool { return normVer(a) != "" && normVer(a) == normVer(b) }

// verLess 版本先后（数字段数值比较、非数字段字典序；legacy 恒最小）——「最新版本」选取用
func verLess(a, b string) bool {
	if a == mediaLegacyVer {
		return b != mediaLegacyVer
	}
	if b == mediaLegacyVer {
		return false
	}
	split := func(v string) []string {
		return strings.FieldsFunc(normVer(v), func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' })
	}
	as, bs := split(a), split(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			if an != bn {
				return an < bn
			}
			continue
		}
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

// latestMediaVer 槽内最新版本（优先真实版本，仅 legacy 时返回 legacy，空库返回 ""）
func latestMediaVer(vers map[string]DockerMediaMeta) string {
	best := ""
	for v := range vers {
		if v == mediaLegacyVer {
			continue
		}
		if best == "" || verLess(best, v) {
			best = v
		}
	}
	if best == "" {
		if _, ok := vers[mediaLegacyVer]; ok {
			return mediaLegacyVer
		}
	}
	return best
}

// pickMedia 选介质：version 非空精确取该版本，空=最新；返回 (登记项, 真实版本)——legacy 条目
// 版本返回 ""（不参与精确匹配）。未命中返回零值。
func pickMedia(lib mediaLib, slot, version string) (DockerMediaMeta, string) {
	vers := lib[slot]
	if len(vers) == 0 {
		return DockerMediaMeta{}, ""
	}
	if version == "" {
		version = latestMediaVer(vers)
	}
	m, ok := vers[version]
	if !ok {
		return DockerMediaMeta{}, ""
	}
	if version == mediaLegacyVer {
		return m, ""
	}
	return m, version
}

// mediaRoute UI 介质路由：POST /api/{tool}-source/media/{槽位} 上传（版本在 form 字段）；
// DELETE /api/{tool}-source/media/{槽位}/{版本} 删除
func mediaRoute(s *Server, tool string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"+tool+"-source/media/"), "/")
		slot, ver, _ := strings.Cut(rest, "/")
		if slot == "" {
			writeErr(w, 404, "not found")
			return
		}
		switch r.Method {
		case http.MethodPost:
			if ver != "" {
				writeErr(w, http.StatusBadRequest, "上传路径不带版本（版本经 form 字段 version 提供）")
				return
			}
			s.mediaUpload(w, r, tool, slot)
		case http.MethodDelete:
			if ver == "" {
				writeErr(w, http.StatusBadRequest, "删除需指定版本：media/{槽位}/{版本}")
				return
			}
			s.mediaDelete(w, r, tool, slot, ver)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "POST/DELETE only")
		}
	}
}

// handleAgentMedia GET /api/agent/media/{tool}/{os}/{arch}?v={version}（agent 令牌；docker|compose）
func (s *Server) handleAgentMedia(w http.ResponseWriter, r *http.Request) {
	if s.agentAuth(r) == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent/media/"), "/")
	tool, rest2, ok := strings.Cut(rest, "/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/agent/media/{docker|compose}/{os}/{arch}")
		return
	}
	osName, arch, ok := strings.Cut(rest2, "/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/agent/media/{docker|compose}/{os}/{arch}")
		return
	}
	s.serveMedia(w, r, tool, osName, arch, r.URL.Query().Get("v"))
}

// handleAgentDockerMedia 旧端点别名：GET /api/agent/docker-media/{os}/{arch}（无版本参数，
// 恒服务最新版本介质）——旧 agent 与在途会话兼容，新 agent 走 /api/agent/media/
func (s *Server) handleAgentDockerMedia(w http.ResponseWriter, r *http.Request) {
	if s.agentAuth(r) == nil {
		writeErr(w, http.StatusUnauthorized, "agent 令牌无效或已吊销")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent/docker-media/"), "/")
	osName, arch, ok := strings.Cut(rest, "/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "路径应为 /api/agent/docker-media/{os}/{arch}")
		return
	}
	s.serveMedia(w, r, mediaToolDocker, osName, arch, "")
}

// serveMedia 介质文件下发（agent 令牌已校验；ver 空=该槽最新版本）
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, tool, osName, arch, ver string) {
	if !validMediaTool(tool) {
		writeErr(w, http.StatusBadRequest, "介质类型应为 docker 或 compose")
		return
	}
	slot := osName + "-" + arch
	if !mediaSlotRe.MatchString(slot) {
		writeErr(w, http.StatusBadRequest, "介质槽位应为 linux-amd64 或 linux-arm64")
		return
	}
	lib := s.mediaLib(tool)
	vers := lib[slot]
	if len(vers) == 0 {
		hint := "（docker 将回退在线脚本安装）"
		if tool == mediaToolCompose {
			hint = "（compose 将回退在线直链安装）"
		}
		writeErr(w, http.StatusNotFound, "console 未上传 "+tool+" "+slot+" 离线介质"+hint)
		return
	}
	if ver == "" {
		ver = latestMediaVer(vers)
	}
	if _, ok := vers[ver]; !ok {
		writeErr(w, http.StatusNotFound, "介质版本 "+ver+" 未上传（该槽位可用版本："+strings.Join(sortedVerKeys(vers), "、")+"）")
		return
	}
	full := s.mediaFilePath(tool, slot, ver)
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "介质文件缺失: "+tool+"/"+slot+"/"+ver)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, full)
}

func sortedVerKeys(vers map[string]DockerMediaMeta) []string {
	out := make([]string, 0, len(vers))
	for v := range vers {
		out = append(out, v)
	}
	// 简单字典序即可（仅错误提示展示用）
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// mediaUpload 介质上传（docker tar.gz / compose 插件二进制；multipart file+version，part 顺序
// 不敏感——file 边扫边落 .part，全部 part 读完再统一校验，校验不过删 .part 不留垃圾）
func (s *Server) mediaUpload(w http.ResponseWriter, r *http.Request, tool, slot string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !validMediaTool(tool) {
		writeErr(w, http.StatusBadRequest, "介质类型应为 docker 或 compose")
		return
	}
	if !mediaSlotRe.MatchString(slot) {
		writeErr(w, http.StatusBadRequest, "介质槽位应为 linux-amd64 或 linux-arm64")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error())
		return
	}
	dst := s.mediaFilePath(tool, slot, "pending") // 版本校验后重命名；先以固定中转名落盘
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hash := sha256.New()
	var fname, version string
	var size int64
	hasFile := false
	fail := ""
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail = "multipart 读取失败: " + err.Error()
			break
		}
		switch p.FormName() {
		case "file":
			if hasFile {
				p.Close()
				continue // 重复 file part：取首个
			}
			hasFile, fname = true, p.FileName()
			n, err := io.Copy(io.MultiWriter(out, hash), p)
			if err != nil {
				fail = "介质落盘失败: " + err.Error()
			}
			size = n
		case "version":
			data, _ := io.ReadAll(io.LimitReader(p, 64))
			version = strings.TrimSpace(string(data))
		}
	}
	if closeErr := out.Close(); fail == "" && closeErr != nil {
		fail = "介质落盘失败: " + closeErr.Error()
	}
	if fail == "" && !hasFile {
		fail = "缺少 file 字段（介质文件）"
	}
	if fail == "" && version == "" {
		fail = "缺少 version 字段（介质按版本管理，上传须标版本号，如 27.5.1 / v2.30.3）"
	}
	if fail == "" && !mediaVerRe.MatchString(version) {
		fail = "版本号格式应为字母数字开头、含 . _ + - 的短串（如 27.5.1 / v2.30.3）"
	}
	if fail != "" {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, fail)
		return
	}
	final := s.mediaFilePath(tool, slot, version)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lib := s.mediaLib(tool)
	if lib[slot] == nil {
		lib[slot] = map[string]DockerMediaMeta{}
	}
	lib[slot][version] = DockerMediaMeta{Name: orStr(fname, version), SHA256: hex.EncodeToString(hash.Sum(nil)),
		Size: size, UploadedAt: time.Now().Unix()}
	if err := s.saveMediaLib(tool, lib); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Printf("[media] %s 介质上传：%s %s（%s，%s）\n", tool, slot, version, fname, humanSizeLocal(size))
	writeJSON(w, map[string]any{"ok": true, "media": lib})
}

// mediaDelete 删除指定槽位指定版本的介质
func (s *Server) mediaDelete(w http.ResponseWriter, r *http.Request, tool, slot, ver string) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	lib := s.mediaLib(tool)
	if _, ok := lib[slot][ver]; !ok {
		writeErr(w, http.StatusNotFound, "该槽位无此版本介质")
		return
	}
	delete(lib[slot], ver)
	if len(lib[slot]) == 0 {
		delete(lib, slot)
	}
	if err := s.saveMediaLib(tool, lib); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(s.mediaFilePath(tool, slot, ver))
	writeJSON(w, map[string]any{"ok": true, "media": lib})
}
