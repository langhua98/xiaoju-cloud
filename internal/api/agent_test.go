package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：AI agent 接口。网页（管理员令牌）发令牌；agent 按权限、文件夹范围、私密、额度办事；
// 改名、移动、回收站、批量预演、撤销；分享；危险操作申请和批准；撤销令牌、换管理员令牌后失效；MCP。
func TestAgentAPI(t *testing.T) {
	dbURL := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	agentRate, agentBurst = 1000, 1000
	defer func() { agentRate, agentBurst = 10, 30 }()
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "it-agent-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	raw := func(token, method, path string, body io.Reader, hdr ...string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, body)
		switch {
		case token == "admin":
			req.Header.Set("X-Xiaoju-Token", "tok")
		case token != "":
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	call := func(token, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		code, b := raw(token, method, path, strings.NewReader(body), hdr...)
		var out map[string]any
		json.Unmarshal(b, &out)
		return code, out
	}
	mustCall := func(want int, token, method, path, body string, hdr ...string) map[string]any {
		t.Helper()
		code, out := call(token, method, path, body, hdr...)
		if code != want {
			t.Fatalf("%s %s: %d (want %d) %v", method, path, code, want, out)
		}
		return out
	}
	id := func(m map[string]any) string {
		if it, ok := m["item"].(map[string]any); ok {
			return it["id"].(string)
		}
		return m["id"].(string)
	}
	newAgent := func(body map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(body)
		out := mustCall(201, "admin", "POST", "/api/agents", string(b))
		tok := out["token"].(string)
		if !strings.HasPrefix(tok, "xa_") || !strings.Contains(out["instructions"].(string), tok) {
			t.Fatalf("token %q", tok)
		}
		return tok
	}

	// 网盘：/工作区（里面 a.txt、sub/b.txt）、/私密（私密文件夹）、/其他/c.txt
	work := id(mustCall(201, "admin", "POST", "/api/folders", `{"name":"工作区"}`))
	sub := id(mustCall(201, "admin", "POST", "/api/folders", `{"name":"sub","parent_id":"`+work+`"}`))
	secret := id(mustCall(201, "admin", "POST", "/api/folders", `{"name":"私密","parent_id":"`+work+`"}`))
	mustCall(200, "admin", "PATCH", "/api/files/"+secret, `{"private":true}`)
	other := id(mustCall(201, "admin", "POST", "/api/folders", `{"name":"其他"}`))
	a := id(mustCall(201, "admin", "POST", "/api/files?name=a.txt&parent_id="+work, "hello agent"))
	mustCall(201, "admin", "POST", "/api/files?name=b.txt&parent_id="+sub, "bbb")
	hidden := id(mustCall(201, "admin", "POST", "/api/files?name=h.txt&parent_id="+secret, "hidden"))
	c := id(mustCall(201, "admin", "POST", "/api/files?name=c.txt&parent_id="+other, "ccc"))

	// 没登录建不了令牌；坏的参数
	if code, _ := call("", "POST", "/api/agents", `{"name":"x","preset":"admin"}`); code != 401 {
		t.Fatalf("anonymous create: %d", code)
	}
	mustCall(400, "admin", "POST", "/api/agents", `{"name":"x","scopes":["files:everything"]}`)
	mustCall(400, "admin", "POST", "/api/agents", `{"name":"x","preset":"admin","expires_days":365}`)
	mustCall(400, "admin", "POST", "/api/agents", `{"name":"x","preset":"admin","root_folder_id":"`+secret+`"}`)

	// 整理档，限定在 /工作区
	org := newAgent(map[string]any{"name": "整理照片", "preset": "organize", "root_folder_id": work})
	if code, out := call("", "GET", "/api/agent/v1/me", ""); code != 401 || out["hint"] == nil {
		t.Fatalf("no token: %d %v", code, out)
	}
	if code, _ := call("xa_wrong", "GET", "/api/agent/v1/me", ""); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	me := mustCall(200, org, "GET", "/api/agent/v1/me", "")
	if me["root"].(map[string]any)["path_on_drive"] != "/工作区" || len(me["scopes"].([]any)) != 4 {
		t.Fatalf("me: %v", me)
	}
	// 列目录：根是 /工作区，私密文件夹看不到；X-Xiaoju-Agent 头也行
	code, b := raw("", "GET", "/api/agent/v1/items?path=/", nil, "X-Xiaoju-Agent", org)
	var root struct {
		Item     agentView   `json:"item"`
		Children []agentView `json:"children"`
	}
	json.Unmarshal(b, &root)
	if code != 200 || root.Item.Path != "/" || len(root.Children) != 2 {
		t.Fatalf("list root: %d %s", code, b)
	}
	for _, ch := range root.Children {
		if ch.Name == "私密" {
			t.Fatal("private folder visible")
		}
	}
	out := mustCall(200, org, "GET", "/api/agent/v1/items?path=/sub", "")
	if ch := out["children"].([]any); len(ch) != 1 || ch[0].(map[string]any)["path"] != "/sub/b.txt" {
		t.Fatalf("list sub: %v", out)
	}
	// 范围外、私密的一律 404
	mustCall(404, org, "GET", "/api/agent/v1/items/"+c, "")
	mustCall(404, org, "GET", "/api/agent/v1/items/"+hidden, "")
	mustCall(404, org, "GET", "/api/agent/v1/items?path=/私密", "")
	mustCall(404, org, "GET", "/api/agent/v1/items/"+other, "")
	// 搜索、统计不含私密的
	out = mustCall(200, org, "GET", "/api/agent/v1/search?q=.TXT", "")
	if n := len(out["items"].([]any)); n != 2 {
		t.Fatalf("search: %v", out)
	}
	out = mustCall(200, org, "GET", "/api/agent/v1/search?type=folder&in=/sub", "")
	if n := len(out["items"].([]any)); n != 0 {
		t.Fatalf("search folders in sub: %v", out)
	}
	out = mustCall(200, org, "GET", "/api/agent/v1/stats", "")
	if st := out["stats"].(map[string]any); st["files"].(float64) != 2 {
		t.Fatalf("stats: %v", out)
	}
	// 下载、Range
	if code, b := raw(org, "GET", "/api/agent/v1/items/"+a+"/content", nil); code != 200 || string(b) != "hello agent" {
		t.Fatalf("download: %d %q", code, b)
	}
	if code, b := raw(org, "GET", "/api/agent/v1/items/"+a+"/content", nil, "Range", "bytes=6-10"); code != 206 || string(b) != "agent" {
		t.Fatalf("range: %d %q", code, b)
	}
	if out := mustCall(200, org, "POST", "/api/agent/v1/items/"+a+"/link", ""); !strings.Contains(out["url"].(string), "/dl/"+a) {
		t.Fatalf("link: %v", out)
	}
	// 上传：上级文件夹自动建；同名改名；Idempotency-Key 重试不传两份
	out = mustCall(201, org, "PUT", "/api/agent/v1/files?path="+url.QueryEscape("/报告/2026/r.md"), "# r")
	if out["item"].(map[string]any)["path"] != "/报告/2026/r.md" {
		t.Fatalf("upload: %v", out)
	}
	out = mustCall(201, org, "PUT", "/api/agent/v1/files?path="+url.QueryEscape("/报告/2026/r.md"), "# r2", "Idempotency-Key", "k1")
	if out["item"].(map[string]any)["name"] != "r (2).md" || out["renamed"] != true {
		t.Fatalf("upload rename: %v", out)
	}
	if out := mustCall(200, org, "PUT", "/api/agent/v1/files?path="+url.QueryEscape("/报告/2026/r.md"), "# r2", "Idempotency-Key", "k1"); out["replayed"] != true {
		t.Fatalf("idempotent: %v", out)
	}
	mustCall(400, org, "PUT", "/api/agent/v1/files?path=/../x", "x")
	mustCall(200, org, "POST", "/api/agent/v1/folders", `{"path":"/报告"}`)
	mustCall(201, org, "POST", "/api/agent/v1/folders", `{"path":"/归档/2025"}`)

	// 没有的权限 403
	if code, out := call(org, "POST", "/api/agent/v1/shares", `{"id":"`+a+`"}`); code != 403 || out["code"] != "missing_scope" {
		t.Fatalf("missing scope: %d %v", code, out)
	}

	// 改名（etag 过期 412）、移动、回收站、恢复
	info := mustCall(200, org, "GET", "/api/agent/v1/items/"+a, "")
	etag := info["item"].(map[string]any)["etag"].(string)
	mustCall(412, org, "PATCH", "/api/agent/v1/items/"+a, `{"name":"x.txt"}`, "If-Match", "2001-01-01T00:00:00Z")
	out = mustCall(200, org, "PATCH", "/api/agent/v1/items/"+a, `{"name":"renamed.txt"}`, "If-Match", etag)
	renameAudit := int64(out["audit_id"].(float64))
	out = mustCall(200, org, "PATCH", "/api/agent/v1/items/"+a, `{"to":"/归档/2025"}`)
	if out["item"].(map[string]any)["path"] != "/归档/2025/renamed.txt" {
		t.Fatalf("move: %v", out)
	}
	moveAudit := int64(out["audit_id"].(float64))
	mustCall(404, org, "PATCH", "/api/agent/v1/items/"+a, `{"parent_id":"`+other+`"}`) // 目标在范围外
	mustCall(403, org, "DELETE", "/api/agent/v1/items/"+work, "")                      // 根文件夹不能删
	out = mustCall(200, org, "DELETE", "/api/agent/v1/items/"+a, "")
	trashAudit := int64(out["audit_id"].(float64))
	mustCall(404, org, "GET", "/api/agent/v1/items/"+a, "")
	if out := mustCall(200, org, "GET", "/api/agent/v1/trash", ""); len(out["items"].([]any)) != 1 {
		t.Fatalf("trash: %v", out)
	}
	mustCall(200, org, "POST", "/api/agent/v1/items/"+a+"/restore", "")
	trashAudit = int64(mustCall(200, org, "DELETE", "/api/agent/v1/items/"+a, "")["audit_id"].(float64))

	// 网页上的操作记录：quiet=1 不列成功的只读请求
	quiet := mustCall(200, "admin", "GET", "/api/agents/audit?quiet=1&limit=500", "")["items"].([]any)
	seen := map[string]bool{}
	for _, it := range quiet {
		e := it.(map[string]any)
		if e["result"] == "ok" && (e["action"] == "read" || e["action"] == "search") {
			t.Fatalf("quiet audit lists read-only entry: %v", e)
		}
		seen[e["action"].(string)] = true
	}
	if !seen["rename"] || !seen["move"] || !seen["trash"] {
		t.Fatalf("quiet audit misses changes: %v", seen)
	}

	// 网页上撤销：回收站的放回去，移动的放回原处，改名的改回去；之后又被改过名字、位置的不动
	mustCall(200, "admin", "POST", fmt.Sprintf("/api/agents/audit/%d/undo", trashAudit), "") // 最后那次移入回收站
	mustCall(409, "admin", "POST", fmt.Sprintf("/api/agents/audit/%d/undo", trashAudit), "") // 只能撤一次
	mustCall(200, "admin", "PATCH", "/api/files/"+a, `{"name":"manual.txt"}`)
	mustCall(409, "admin", "POST", fmt.Sprintf("/api/agents/audit/%d/undo", moveAudit), "") // 人又改了名字
	mustCall(200, "admin", "PATCH", "/api/files/"+a, `{"name":"renamed.txt"}`)
	mustCall(200, "admin", "POST", fmt.Sprintf("/api/agents/audit/%d/undo", moveAudit), "")
	mustCall(200, "admin", "POST", fmt.Sprintf("/api/agents/audit/%d/undo", renameAudit), "")
	if f, _ := store.Get(ctx, userID, uuid.MustParse(a)); f.Name != "a.txt" || f.ParentID.String() != work || f.TrashedAt != nil {
		t.Fatalf("after undo: %+v", f)
	}

	// 批量：超过 50 个文件要先预演
	many := id(mustCall(201, "admin", "POST", "/api/folders", `{"name":"many","parent_id":"`+work+`"}`))
	for i := 0; i < agentPlanFiles+1; i++ {
		mustCall(201, "admin", "POST", fmt.Sprintf("/api/files?name=%d.txt&parent_id=%s", i, many), "x")
	}
	if code, out := call(org, "DELETE", "/api/agent/v1/items/"+many, ""); code != 409 || out["code"] != "plan_required" {
		t.Fatalf("plan required: %d %v", code, out)
	}
	ops := `"ops":[{"op":"move","path":"/many","to":"/归档"},{"op":"rename","path":"/sub/b.txt","name":"b2.txt"},{"op":"trash","id":"` + c + `"}]`
	if code, out := call(org, "POST", "/api/agent/v1/batch", "{"+ops+"}"); code != 409 || out["code"] != "plan_required" {
		t.Fatalf("batch without plan: %d %v", code, out)
	}
	plan := mustCall(200, org, "POST", "/api/agent/v1/batch", "{"+ops+`,"dry_run":true}`)
	res := plan["results"].([]any)
	if plan["total_files"].(float64) != float64(agentPlanFiles+1) || res[2].(map[string]any)["code"] != "not_found" {
		t.Fatalf("plan: %v", plan)
	}
	if f, _ := store.Get(ctx, userID, uuid.MustParse(many)); f.ParentID.String() != work {
		t.Fatal("dry run changed something")
	}
	mustCall(409, org, "POST", "/api/agent/v1/batch", "{"+ops+`,"plan_id":"nope"}`)
	ran := mustCall(200, org, "POST", "/api/agent/v1/batch", "{"+ops+`,"plan_id":"`+plan["plan_id"].(string)+`"}`)
	if ran["done"].(float64) != 2 || ran["failed"].(float64) != 1 {
		t.Fatalf("batch: %v", ran)
	}
	mustCall(200, org, "GET", "/api/agent/v1/items?path="+url.QueryEscape("/归档/many"), "")
	undo := mustCall(200, "admin", "POST", "/api/agents/batches/"+ran["batch_id"].(string)+"/undo", "")
	if undo["undone"].(float64) != 2 {
		t.Fatalf("batch undo: %v", undo)
	}
	mustCall(200, org, "GET", "/api/agent/v1/items?path=/sub/b.txt", "")
	mustCall(200, org, "GET", "/api/agent/v1/items?path=/many", "")

	// 管理员档（整个网盘）：分享（私密的不行、最长 7 天）、危险操作申请
	adm := newAgent(map[string]any{"name": "管家", "preset": "admin", "limits": map[string]int64{"download_bytes": 15}})
	mustCall(400, adm, "POST", "/api/agent/v1/shares", `{"id":"`+c+`","expires_days":30}`)
	sh := mustCall(201, adm, "POST", "/api/agent/v1/shares", `{"id":"`+c+`","expires_days":3}`)
	if out := mustCall(200, adm, "GET", "/api/agent/v1/shares", ""); len(out["items"].([]any)) != 1 {
		t.Fatalf("shares: %v", out)
	}
	mustCall(404, adm, "POST", "/api/agent/v1/shares", `{"id":"`+hidden+`"}`) // 看不到私密的
	mustCall(204, adm, "DELETE", "/api/agent/v1/shares/"+sh["id"].(string), "")
	// 额度：下载 15 字节，第三次超了
	mustCall(200, adm, "GET", "/api/agent/v1/items/"+c+"/content", "")
	mustCall(200, adm, "GET", "/api/agent/v1/items/"+c+"/content", "")
	if code, out := call(adm, "GET", "/api/agent/v1/items/"+a+"/content", ""); code != 429 || out["code"] != "quota_exceeded" {
		t.Fatalf("quota: %d %v", code, out)
	}
	// 永久删除：要先进回收站，要理由，要人批准
	mustCall(400, adm, "POST", "/api/agent/v1/requests", `{"action":"purge","ids":["`+c+`"],"reason":"重复"}`)
	mustCall(200, adm, "DELETE", "/api/agent/v1/items/"+c, "")
	mustCall(400, adm, "POST", "/api/agent/v1/requests", `{"action":"purge","ids":["`+c+`"]}`)
	q := mustCall(202, adm, "POST", "/api/agent/v1/requests", `{"action":"purge","ids":["`+c+`"],"reason":"重复的文件"}`)["request"].(map[string]any)
	if _, err := store.Get(ctx, userID, uuid.MustParse(c)); err != nil {
		t.Fatal("purged before approval")
	}
	if out := mustCall(200, "admin", "GET", "/api/agents/requests?status=pending", ""); len(out["items"].([]any)) != 1 {
		t.Fatalf("pending: %v", out)
	}
	ap := mustCall(200, "admin", "POST", "/api/agents/requests/"+q["id"].(string)+"/approve", `{}`)
	if ap["request"].(map[string]any)["status"] != "approved" {
		t.Fatalf("approve: %v", ap)
	}
	if _, err := store.Get(ctx, userID, uuid.MustParse(c)); err == nil {
		t.Fatal("not purged after approval")
	}
	mustCall(409, "admin", "POST", "/api/agents/requests/"+q["id"].(string)+"/approve", `{}`)
	if st := mustCall(200, adm, "GET", "/api/agent/v1/requests/"+q["id"].(string), "")["request"].(map[string]any); st["status"] != "approved" {
		t.Fatalf("agent sees: %v", st)
	}
	q2 := mustCall(202, adm, "POST", "/api/agent/v1/requests", `{"action":"empty_trash","reason":"清理"}`)["request"].(map[string]any)
	mustCall(200, "admin", "POST", "/api/agents/requests/"+q2["id"].(string)+"/reject", "")
	mustCall(404, org, "GET", "/api/agent/v1/requests/"+q2["id"].(string), "") // 别的令牌的看不到

	// 只读档：MCP
	ro := newAgent(map[string]any{"name": "只读", "preset": "readonly"})
	rpc := func(token string, body string) map[string]any {
		t.Helper()
		out := mustCall(200, token, "POST", "/mcp", body, "Content-Type", "application/json", "Accept", "application/json, text/event-stream")
		if out["error"] != nil {
			t.Fatalf("rpc %s: %v", body, out)
		}
		return out["result"].(map[string]any)
	}
	init := rpc(ro, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("init: %v", init)
	}
	if code, _ := call(ro, "POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	tools := rpc(ro, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if j := strings.Join(names, ","); !strings.Contains(j, "list_folder") || strings.Contains(j, "move_to_trash") || strings.Contains(j, "read_file") {
		t.Fatalf("readonly tools: %s", j)
	}
	r := rpc(ro, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_folder","arguments":{"path":"/工作区"}}}`)
	if text := r["content"].([]any)[0].(map[string]any)["text"].(string); r["isError"] == true || !strings.Contains(text, "a.txt") {
		t.Fatalf("mcp list: %v", r)
	}
	r = rpc(ro, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"move_to_trash","arguments":{"id":"`+a+`"}}}`)
	if r["isError"] != true {
		t.Fatalf("readonly trash allowed: %v", r)
	}
	// 管理员档用 MCP 上传、读文件
	r = rpc(adm, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"upload_file","arguments":{"path":"/工作区/mcp.md","text":"from mcp"}}}`)
	if r["isError"] == true {
		t.Fatalf("mcp upload: %v", r)
	}
	var up struct {
		Item agentView `json:"item"`
	}
	json.Unmarshal([]byte(r["content"].([]any)[0].(map[string]any)["text"].(string)), &up)
	agentID := func(name string) string {
		t.Helper()
		for _, it := range mustCall(200, "admin", "GET", "/api/agents", "")["items"].([]any) {
			if m := it.(map[string]any); m["name"] == name {
				return m["id"].(string)
			}
		}
		t.Fatalf("no agent %s", name)
		return ""
	}
	mustCall(200, "admin", "PATCH", "/api/agents/"+agentID("管家"), `{"limits":{"download_bytes":0}}`)
	r = rpc(adm, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read_file","arguments":{"id":"`+up.Item.ID.String()+`"}}}`)
	if text := r["content"].([]any)[0].(map[string]any)["text"]; text != "from mcp" {
		t.Fatalf("mcp read: %v", r)
	}

	// 审计日志：被拒的也记了
	out = mustCall(200, "admin", "GET", "/api/agents/audit?result=denied", "")
	if len(out["items"].([]any)) == 0 {
		t.Fatal("no denied entries")
	}
	// 连续被拒 50 次冻结
	for i := 0; i < agentFreezeAfter; i++ {
		call(ro, "DELETE", "/api/agent/v1/items/"+a, "")
	}
	if code, out := call(ro, "GET", "/api/agent/v1/me", ""); code != 403 || out["code"] != "token_frozen" {
		t.Fatalf("frozen: %d %v", code, out)
	}
	list := mustCall(200, "admin", "GET", "/api/agents", "")["items"].([]any)
	var roID string
	for _, it := range list {
		if m := it.(map[string]any); m["name"] == "只读" {
			roID = m["id"].(string)
			if m["state"] != "frozen" {
				t.Fatalf("state: %v", m)
			}
		}
	}
	mustCall(200, "admin", "PATCH", "/api/agents/"+roID, `{"unfreeze":true}`)
	mustCall(200, ro, "GET", "/api/agent/v1/me", "")
	// 撤销后立即失效；全部撤销
	mustCall(204, "admin", "DELETE", "/api/agents/"+roID, "")
	if code, out := call(ro, "GET", "/api/agent/v1/me", ""); code != 401 || out["code"] != "token_revoked" {
		t.Fatalf("revoked: %d %v", code, out)
	}
	// 换了管理员令牌，旧的 agent 令牌全部作废
	ts2 := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok2", Log: log}).Handler())
	defer ts2.Close()
	req, _ := http.NewRequest("GET", ts2.URL+"/api/agent/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+org)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("admin token changed: %v %v", err, resp.StatusCode)
	}
	if out := mustCall(200, "admin", "POST", "/api/agents/revoke-all", ""); out["revoked"].(float64) != 2 {
		t.Fatalf("revoke all: %v", out)
	}
	mustCall(401, org, "GET", "/api/agent/v1/me", "")
}
