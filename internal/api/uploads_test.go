package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：断点续传。分段传、对不上起点时告诉客户端从哪接着传、服务重启后从写好的分块接着传、
// 刷新页面后重新选同一个文件接着传、传到一半断开、新请求接手挂住的旧请求、按内容去重、过期清理。
func TestResumableUpload(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "it-resume-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	newServer := func() (*service.Service, *httptest.Server) {
		svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
		return svc, httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	}
	svc, ts := newServer()
	defer func() { ts.Close() }()

	do := func(method, path string, body io.Reader) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, body)
		req.Header.Set("X-Xiaoju-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		return do(method, path, strings.NewReader(body))
	}
	start := func(name string, size int, probe string, dedupe bool) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"name": name, "parent_id": nil, "size": size, "probe": probe, "dedupe": dedupe})
		return call("POST", "/api/uploads", string(b))
	}
	put := func(id string, off int, data []byte) (int, map[string]any) {
		t.Helper()
		return do("PUT", fmt.Sprintf("/api/uploads/%s?offset=%d", id, off), bytes.NewReader(data))
	}
	offset := func(out map[string]any) int {
		v, _ := out["offset"].(float64)
		return int(v)
	}
	download := func(id string) []byte {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/api/files/"+id+"/content", nil)
		req.Header.Set("X-Xiaoju-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	parts := func(id string) int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM file_parts WHERE file_id = $1`, id).Scan(&n)
		return n
	}
	// 攒满的分块在后台写：等它们都记进数据库
	waitParts := func(id string, want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); parts(id) != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("upload %s: %d parts stored, want %d", id, parts(id), want)
			}
		}
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		rand.Read(b)
		return b
	}

	video := random(3500)
	code, out := start("v.mp4", len(video), "p1", false)
	if code != 201 || offset(out) != 0 || out["chunk"] == nil {
		t.Fatalf("start: %d %v", code, out)
	}
	id := out["id"].(string)

	// 第一段：还没攒满一个分块，存在临时文件里
	if code, out := put(id, 0, video[:700]); code != 200 || offset(out) != 700 {
		t.Fatalf("put 1: %d %v", code, out)
	}
	if code, out := call("GET", "/api/uploads/"+id, ""); code != 200 || offset(out) != 700 {
		t.Fatalf("status: %d %v", code, out)
	}
	// 重发第一段（上次的回复丢了）：告诉客户端从 700 接着传
	if code, out := put(id, 0, video[:700]); code != 409 || offset(out) != 700 {
		t.Fatalf("resend: %d %v", code, out)
	}
	// 第二段跨过两个分块边界：写好两个分块（第二块在后台写，不等它），剩 300 字节攒着
	if code, out := put(id, 700, video[700:2300]); code != 200 || offset(out) != 2300 {
		t.Fatalf("put 2: %d %v", code, out)
	}
	waitParts(id, 2)

	// 服务重启：攒着的 300 字节没了，从写好的两个分块（2000）接着传
	ts.Close()
	svc, ts = newServer()
	if code, out := call("GET", "/api/uploads/"+id, ""); code != 200 || offset(out) != 2000 {
		t.Fatalf("status after restart: %d %v", code, out)
	}
	if code, out := put(id, 2300, video[2300:]); code != 409 || offset(out) != 2000 {
		t.Fatalf("stale offset after restart: %d %v", code, out)
	}
	code, out = put(id, 2000, video[2000:])
	if code != 201 || offset(out) != len(video) || out["file"] == nil {
		t.Fatalf("last put: %d %v", code, out)
	}
	file := out["file"].(map[string]any)
	if file["id"] != id || file["size"] != float64(len(video)) || file["mime"] != "video/mp4" {
		t.Fatalf("file: %v", file)
	}
	if got := download(id); !bytes.Equal(got, video) {
		t.Fatalf("downloaded %d bytes, content differs", len(got))
	}
	// 整个文件的 SHA-256 跨过了重启也是对的（恢复了中间状态）
	sum := sha256.Sum256(video)
	if code, out := call("POST", "/api/files/known", `{"sha256":["`+hex.EncodeToString(sum[:])+`"]}`); code != 200 || len(out["known"].(map[string]any)) != 1 {
		t.Fatalf("whole-file sha256 after restart: %d %v", code, out)
	}
	if code, _ := call("GET", "/api/uploads/"+id, ""); code != 404 {
		t.Fatalf("finished upload status: %d", code)
	}
	if code, _ := start("v.mp4", len(video), "p1", false); code != 409 {
		t.Fatalf("the name is taken now: %d", code)
	}

	// 刷新页面后重新选同一个文件：接着传；同名同大小但内容不一样：旧的作废，重新开始
	doc := random(2500)
	_, out = start("w.bin", len(doc), "a", false)
	wid := out["id"].(string)
	put(wid, 0, doc[:1200])
	if code, out := start("w.bin", len(doc), "a", false); code != 200 || out["id"] != wid || offset(out) != 1200 {
		t.Fatalf("resume after reload: %d %v", code, out)
	}
	code, out = start("w.bin", len(doc), "b", false)
	if code != 201 || out["id"] == wid || offset(out) != 0 {
		t.Fatalf("different content: %d %v", code, out)
	}
	if parts(wid) != 0 {
		t.Fatal("the replaced upload's parts must be deleted")
	}
	wid = out["id"].(string)

	// 传到一半断开：收到的都算数，从实际收到的地方接着传
	pr, pw := io.Pipe()
	go func() {
		pw.Write(doc[:1500])
		time.Sleep(100 * time.Millisecond)
		pw.CloseWithError(fmt.Errorf("network gone"))
	}()
	do("PUT", "/api/uploads/"+wid+"?offset=0", pr)
	var got int
	for range 50 { // 服务端处理完断开的请求需要一点时间
		_, out = call("GET", "/api/uploads/"+wid, "")
		if got = offset(out); got == 1500 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != 1500 {
		t.Fatalf("after a dropped request: offset %d, want 1500", got)
	}

	// 旧请求挂住了（手机断网，服务端还在等数据）：新请求接手，不用等
	pr, pw = io.Pipe()
	hung := make(chan int, 1)
	go func() {
		code, _ := do("PUT", "/api/uploads/"+wid+"?offset=1500", pr)
		hung <- code
	}()
	pw.Write(doc[1500:1600])
	for range 50 {
		if _, out = call("GET", "/api/uploads/"+wid, ""); offset(out) == 1600 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if offset(out) != 1600 {
		t.Fatalf("hung request: offset %v", out)
	}
	began := time.Now()
	code, out = put(wid, 1600, doc[1600:])
	if code != 201 || time.Since(began) > 5*time.Second {
		t.Fatalf("takeover: %d %v after %s", code, out, time.Since(began))
	}
	pw.Close()
	select {
	case <-hung:
	case <-time.After(5 * time.Second):
		t.Fatal("the old request never ended")
	}
	if got := download(wid); !bytes.Equal(got, doc) {
		t.Fatal("content after drop and takeover differs")
	}

	// 按内容去重：传完时发现网盘里有一样的，不存，分块删掉
	_, out = start("copy.mp4", len(video), "c", true)
	cid := out["id"].(string)
	if code, out := put(cid, 0, video); code != 200 || out["same_content"] != true || out["file"].(map[string]any)["id"] != id {
		t.Fatalf("dedupe: %d %v", code, out)
	}
	if parts(cid) != 0 {
		t.Fatal("duplicate parts must be deleted")
	}
	if code, _ := call("GET", "/api/uploads/"+cid, ""); code != 404 {
		t.Fatal("duplicate upload must be gone")
	}

	// 请求不对：大小为 0、内容比文件长
	if code, _ := start("empty.bin", 0, "", false); code != 400 {
		t.Fatalf("size 0: %d", code)
	}
	_, out = start("long.bin", 10, "", false)
	if code, _ := put(out["id"].(string), 0, random(20)); code != 400 {
		t.Fatalf("body too long: %d", code)
	}

	// 24 小时没动静的清理掉（分块一起删）
	_, out = start("old.bin", 1500, "o", false)
	oid := out["id"].(string)
	put(oid, 0, random(1200))
	waitParts(oid, 1)
	pool.Exec(ctx, `UPDATE upload_sessions SET updated_at = now() - interval '2 days' WHERE file_id = $1`, oid)
	if _, err := svc.CleanupStaleUploads(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("GET", "/api/uploads/"+oid, ""); code != 404 || parts(oid) != 0 {
		t.Fatalf("stale upload: %d parts=%d", code, parts(oid))
	}
	// 还在传的不清理
	_, out = start("fresh.bin", 1500, "f", false)
	fid := out["id"].(string)
	put(fid, 0, random(1200))
	svc.CleanupStaleUploads(ctx)
	if code, out := call("GET", "/api/uploads/"+fid, ""); code != 200 || offset(out) != 1200 {
		t.Fatalf("fresh upload must survive cleanup: %d %v", code, out)
	}
}
