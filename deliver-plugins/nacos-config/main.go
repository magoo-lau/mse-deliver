// nacos-config：配置中心插件（ConfigCenter Provider 首个实现，协议见 docs/tech/deliver-design.md ADR-4）。
//
// 形态：独立可执行文件，单次调用单次进程——stdin 读一段 JSON 请求 {op,args}，stdout 写一行 JSON
// 响应 {ok,data|error}（进度走 stderr，由执行器作为日志收集）。Nacos v3 admin API
// （/nacos/v3/admin/cs/config + serverIdentity 头），与 mse-framework/docker/nacos/init-configs.sh 同口径：
// apply 幂等模式只写缺失 Data ID（保护线上手改），force 模式按基线覆盖。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ---- 协议 ----

type request struct {
	Op   string `json:"op"` // health|read|plan|apply
	Args args   `json:"args"`
}

type args struct {
	ServerAddr    string `json:"serverAddr"` // 如 http://nacos:8848
	Namespace     string `json:"namespace,omitempty"`
	Group         string `json:"group,omitempty"` // 缺省 DEFAULT_GROUP
	IdentityKey   string `json:"identityKey,omitempty"`
	IdentityValue string `json:"identityValue,omitempty"`
	DataID        string `json:"dataId,omitempty"` // read：单条
	Mode          string `json:"mode,omitempty"`   // apply：idempotent（缺省）|force
	Items         []item `json:"items,omitempty"`  // plan/apply：基线清单
}

type item struct {
	DataID  string `json:"dataId"`
	Group   string `json:"group,omitempty"`
	Content string `json:"content"`
}

type response struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		emit(response{OK: false, Error: "请求解析失败: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	data, err := dispatch(ctx, req)
	if err != nil {
		emit(response{OK: false, Error: err.Error()})
		return
	}
	emit(response{OK: true, Data: data})
}

func emit(r response) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func dispatch(ctx context.Context, req request) (any, error) {
	if req.Op == "" {
		return nil, fmt.Errorf("缺少 op")
	}
	c, err := newClient(req.Args)
	if err != nil {
		return nil, err
	}
	switch req.Op {
	case "health":
		if err := c.health(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"server": c.base, "namespace": c.ns}, nil
	case "read":
		if req.Args.DataID == "" {
			return nil, fmt.Errorf("read 缺少 dataId")
		}
		content, exists, err := c.get(ctx, req.Args.DataID, c.group)
		if err != nil {
			return nil, err
		}
		return map[string]any{"dataId": req.Args.DataID, "exists": exists, "content": content}, nil
	case "plan":
		return c.plan(ctx, req.Args.Items)
	case "apply":
		return c.apply(ctx, req.Args.Items, req.Args.Mode)
	default:
		return nil, fmt.Errorf("未知 op %q（支持 health|read|plan|apply）", req.Op)
	}
}

// ---- Nacos v3 admin API 客户端 ----

type client struct {
	base  string
	ns    string
	group string
	idKey string
	idVal string
	hc    *http.Client
}

func newClient(a args) (*client, error) {
	addr := strings.TrimRight(strings.TrimSpace(a.ServerAddr), "/")
	if addr == "" {
		return nil, fmt.Errorf("缺少 serverAddr")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	c := &client{base: addr, ns: orStr(a.Namespace, "public"), group: orStr(a.Group, "DEFAULT_GROUP"),
		idKey: orStr(a.IdentityKey, "serverIdentity"), idVal: a.IdentityValue,
		hc: &http.Client{Timeout: 15 * time.Second}}
	if c.idVal == "" {
		c.idKey, c.idVal = "", "" // 未配置身份头：匿名访问（目标 Nacos 未开鉴权）
	}
	return c, nil
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (c *client) newReq(ctx context.Context, method, path string, form url.Values) (*http.Request, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.idKey != "" {
		req.Header.Set(c.idKey, c.idVal)
	}
	return req, nil
}

// get 读单条配置：返回（内容, 是否存在）；404 或业务码 ConfigNotFound 视为不存在
func (c *client) get(ctx context.Context, dataID, group string) (string, bool, error) {
	group = orStr(group, c.group)
	q := url.Values{}
	q.Set("dataId", dataID)
	q.Set("groupName", group)
	q.Set("namespaceId", c.ns)
	req, err := c.newReq(ctx, http.MethodGet, "/nacos/v3/admin/cs/config?"+q.Encode(), nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("连接 %s 失败: %w", c.base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("查询 %s 返回 HTTP %d", dataID, resp.StatusCode)
	}
	var en struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal(body, &en); err != nil {
		return "", false, fmt.Errorf("查询 %s 响应解析失败: %w", dataID, err)
	}
	if en.Code != 0 {
		return "", false, nil // 业务码非 0：配置不存在（v3 admin API 对缺失返回非 0 码）
	}
	return en.Data, true, nil
}

func (c *client) publish(ctx context.Context, dataID, group, content string) error {
	group = orStr(group, c.group)
	form := url.Values{}
	form.Set("dataId", dataID)
	form.Set("groupName", group)
	form.Set("namespaceId", c.ns)
	form.Set("type", "yaml")
	form.Set("content", content)
	req, err := c.newReq(ctx, http.MethodPost, "/nacos/v3/admin/cs/config", form)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("发布 %s 失败: %w", dataID, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("发布 %s 返回 HTTP %d", dataID, resp.StatusCode)
	}
	return nil
}

func (c *client) health(ctx context.Context) error {
	req, err := c.newReq(ctx, http.MethodGet, "/nacos/v3/admin/core/state", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", c.base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("健康检查返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---- plan / apply ----

type change struct {
	DataID        string `json:"dataId"`
	Group         string `json:"group,omitempty"`
	Status        string `json:"status"` // missing|same|differs
	CurrentBytes  int    `json:"currentBytes"`
	BaselineBytes int    `json:"baselineBytes"`
}

func (c *client) plan(ctx context.Context, items []item) (any, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("plan 缺少 items")
	}
	var changes []change
	missing, differs := 0, 0
	for _, it := range items {
		cur, exists, err := c.get(ctx, it.DataID, it.Group)
		if err != nil {
			return nil, err
		}
		ch := change{DataID: it.DataID, Group: orStr(it.Group, c.group), BaselineBytes: len(it.Content), CurrentBytes: len(cur)}
		switch {
		case !exists:
			ch.Status = "missing"
			missing++
		case strings.ReplaceAll(cur, "\r\n", "\n") == strings.ReplaceAll(it.Content, "\r\n", "\n"):
			ch.Status = "same"
		default:
			ch.Status = "differs"
			differs++
		}
		logf("plan %s: %s（现网 %d 字节 / 基线 %d 字节）", ch.DataID, ch.Status, ch.CurrentBytes, ch.BaselineBytes)
		changes = append(changes, ch)
	}
	return map[string]any{"changes": changes, "missing": missing, "differs": differs, "total": len(items)}, nil
}

func (c *client) apply(ctx context.Context, items []item, mode string) (any, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("apply 缺少 items")
	}
	force := mode == "force"
	var applied, skippedSame, skippedExists []string
	for _, it := range items {
		cur, exists, err := c.get(ctx, it.DataID, it.Group)
		if err != nil {
			return nil, err
		}
		switch {
		case !exists:
			if err := c.publish(ctx, it.DataID, it.Group, it.Content); err != nil {
				return nil, err
			}
			applied = append(applied, it.DataID)
			logf("apply %s: 已写入（原缺失）", it.DataID)
		case strings.ReplaceAll(cur, "\r\n", "\n") == strings.ReplaceAll(it.Content, "\r\n", "\n"):
			skippedSame = append(skippedSame, it.DataID)
			logf("apply %s: 跳过（内容一致）", it.DataID)
		case force:
			if err := c.publish(ctx, it.DataID, it.Group, it.Content); err != nil {
				return nil, err
			}
			applied = append(applied, it.DataID)
			logf("apply %s: 已覆盖（force）", it.DataID)
		default:
			skippedExists = append(skippedExists, it.DataID)
			logf("apply %s: 跳过（现网已存在且与基线不同——如确认覆盖请在 DAG 中把 apply 节点改为 force）", it.DataID)
		}
	}
	return map[string]any{"applied": applied, "skippedSame": skippedSame, "skippedExists": skippedExists,
		"mode": orStr(mode, "idempotent"), "total": len(items)}, nil
}
