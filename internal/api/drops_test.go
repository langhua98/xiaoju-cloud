package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// 集成测试：上传链接。不用登录能用 PUT、multipart、原始请求体往链接的文件夹里传；同名改名不覆盖；
// 页面按 Accept 给 HTML / JSON / 文字；撤销、过期、文件夹进回收站后传不了；没登录建不了链接。
func TestDropLinks(t *testing.T) {
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
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "it-drop-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	do := func(auth bool, method, path string, body io.Reader, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, body)
		if auth {
			req.Header.Set("X-Xiaoju-Token", "tok")
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
		return resp.StatusCode, resp.Header, b
	}
	call := func(auth bool, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		code, _, b := do(auth, method, path, strings.NewReader(body), hdr...)
		var out map[string]any
		json.Unmarshal(b, &out)
		return code, out
	}
	names := func(out map[string]any) []string {
		var n []string
		for _, f := range out["files"].([]any) {
			n = append(n, f.(map[string]any)["name"].(string))
		}
		return n
	}

	// 没登录建不了
	if code, _ := call(false, "POST", "/api/drops", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("create without auth: %d", code)
	}
	// 不指定文件夹：放进根目录的“AI 上传”
	code, d := call(true, "POST", "/api/drops", `{"name":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, d)
	}
	path := d["path"].(string)
	folder := d["folder"].(map[string]any)
	if folder["name"] != DropFolder || d["name"] != "claude" || len(d["token"].(string)) < 24 {
		t.Fatalf("drop: %v", d)
	}

	// PUT 原始内容
	code, out := call(false, "PUT", path+"/report.txt", "hello")
	if code != http.StatusCreated || names(out)[0] != "report.txt" {
		t.Fatalf("put: %d %v", code, out)
	}
	if f := out["files"].([]any)[0].(map[string]any); f["size"].(float64) != 5 || f["mime"] != "text/plain; charset=utf-8" ||
		f["sha256"] != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("put result: %v", f)
	}
	// 同名不覆盖，改名“ (2)”
	if code, out = call(false, "PUT", path+"/report.txt", "world"); code != http.StatusCreated || names(out)[0] != "report (2).txt" {
		t.Fatalf("put same name: %d %v", code, out)
	}
	// 名字里带路径分隔符、中文（URL 编码）
	if code, out = call(false, "PUT", path+"/a%2Fb%20%E6%8A%A5%E5%91%8A.md", "x"); code != http.StatusCreated || names(out)[0] != "a_b 报告.md" {
		t.Fatalf("put encoded name: %d %v", code, out)
	}
	// multipart，一次多个（普通字段忽略）
	var mb bytes.Buffer
	mw := multipart.NewWriter(&mb)
	mw.WriteField("note", "ignored")
	fw, _ := mw.CreateFormFile("file", "a.csv")
	fw.Write([]byte("1,2\n"))
	fw, _ = mw.CreateFormFile("file", "report.txt")
	fw.Write([]byte("third"))
	mw.Close()
	code, _, b := do(false, "POST", path, &mb, "Content-Type", mw.FormDataContentType())
	json.Unmarshal(b, &out)
	if code != http.StatusCreated || strings.Join(names(out), "|") != "a.csv|report (3).txt" {
		t.Fatalf("multipart: %d %s", code, b)
	}
	// 原始请求体 + ?name=；没名字时按内容补扩展名
	if code, out = call(false, "POST", path+"?name=notes.md", "# hi"); code != http.StatusCreated || names(out)[0] != "notes.md" {
		t.Fatalf("raw post: %d %v", code, out)
	}
	if code, out = call(false, "POST", path, "\x89PNG\r\n\x1a\nxxxxxxxx"); code != http.StatusCreated || !strings.HasSuffix(names(out)[0], ".png") {
		t.Fatalf("raw post without name: %d %v", code, out)
	}
	// 空文件也行；multipart 里没有文件不行
	if code, out = call(false, "PUT", path+"/empty.txt", ""); code != http.StatusCreated {
		t.Fatalf("empty: %d %v", code, out)
	}
	mb.Reset()
	mw = multipart.NewWriter(&mb)
	mw.WriteField("note", "no file")
	mw.Close()
	code, _, b = do(false, "POST", path, &mb, "Content-Type", mw.FormDataContentType())
	if code != http.StatusBadRequest || !strings.Contains(string(b), "no_file") {
		t.Fatalf("no file: %d %s", code, b)
	}

	// 文件都在“AI 上传”里
	fid, _ := uuid.Parse(folder["id"].(string))
	kids, err := store.ListChildren(ctx, userID, &fid)
	if err != nil || len(kids) != 8 {
		t.Fatalf("folder has %d files, err %v", len(kids), err)
	}

	// 页面：浏览器 HTML、JSON、curl 文字；地址里不会跑出别的
	code, hdr, b := do(false, "GET", path, nil, "Accept", "text/html")
	if code != 200 || !strings.Contains(string(b), "上传到小桔云盘") || !strings.Contains(string(b), "curl -T report.pdf "+ts.URL+path+"/") ||
		hdr.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("html page: %d %s", code, b)
	}
	if code, out = call(false, "GET", path+"?format=json", ""); code != 200 || out["folder"] != DropFolder {
		t.Fatalf("json: %d %v", code, out)
	}
	if code, _, b = do(false, "GET", path, nil, "Accept", "*/*"); code != 200 || !strings.HasPrefix(string(b), "xiaoju-cloud upload link") {
		t.Fatalf("text: %d %s", code, b)
	}
	// 跨域预检
	if code, hdr, _ = do(false, "OPTIONS", path+"/x.txt", nil, "Origin", "https://example.com", "Access-Control-Request-Method", "PUT"); code != http.StatusNoContent ||
		hdr.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", code, hdr)
	}

	// 列表里有统计
	code, list := call(true, "GET", "/api/drops", "")
	item := list["items"].([]any)[0].(map[string]any)
	if code != 200 || item["uploads"].(float64) != 8 {
		t.Fatalf("list: %d %v", code, list)
	}

	// 不存在的链接
	if code, out = call(false, "PUT", "/u/nope/x.txt", "x"); code != http.StatusNotFound || out["code"] != "link_missing" {
		t.Fatalf("missing: %d %v", code, out)
	}
	// 指定文件夹；文件夹进回收站后传不了
	_, sub := call(true, "POST", "/api/folders", `{"name":"agent-out"}`)
	_, d2 := call(true, "POST", "/api/drops", `{"folder_id":"`+sub["id"].(string)+`","expires_days":1}`)
	if d2["folder"].(map[string]any)["name"] != "agent-out" || d2["expires_at"] == nil {
		t.Fatalf("drop into folder: %v", d2)
	}
	if code, _ = call(false, "PUT", d2["path"].(string)+"/x.txt", "x"); code != http.StatusCreated {
		t.Fatalf("put into folder: %d", code)
	}
	call(true, "DELETE", "/api/files/"+sub["id"].(string), "")
	if code, _ = call(false, "PUT", d2["path"].(string)+"/y.txt", "y"); code != http.StatusNotFound {
		t.Fatalf("trashed folder: %d", code)
	}
	// 过期
	if _, err := pool.Exec(ctx, `UPDATE agent_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, d2["id"]); err != nil {
		t.Fatal(err)
	}
	if code, out = call(false, "GET", d2["path"].(string)+"?format=json", ""); code != http.StatusGone || out["code"] != "link_expired" {
		t.Fatalf("expired: %d %v", code, out)
	}
	// 文件不能当目标
	kid := kids[0].ID.String()
	if code, _ = call(true, "POST", "/api/drops", `{"folder_id":"`+kid+`"}`); code != http.StatusBadRequest {
		t.Fatalf("drop into a file: %d", code)
	}
	// 撤销
	if code, _ = call(true, "DELETE", "/api/drops/"+d["id"].(string), ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ = call(false, "PUT", path+"/z.txt", "z"); code != http.StatusNotFound {
		t.Fatalf("revoked: %d", code)
	}
}
