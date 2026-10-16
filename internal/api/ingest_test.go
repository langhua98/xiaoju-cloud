package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

func TestIngestName(t *testing.T) {
	req := func(q, header string) *http.Request {
		r := httptest.NewRequest("POST", "/api/ingest"+q, nil)
		if header != "" {
			r.Header.Set("X-File-Name", header)
		}
		return r
	}
	for _, c := range []struct {
		r    *http.Request
		ext  string
		want string
	}{
		{req("", "IMG_0001"), ".heic", "IMG_0001.heic"},      // 快捷指令给的名字没有扩展名
		{req("", "IMG_0001.HEIC"), ".heic", "IMG_0001.HEIC"}, // 有扩展名就不动
		{req("?name=a%2Fb.jpg", ""), ".jpg", "a_b.jpg"},      // 不能带路径
		{req("", "%E5%9B%BE.png"), ".png", "图.png"},          // URL 编码的中文名
	} {
		if got := ingestName(c.r, c.ext); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
	if got := ingestName(req("", ""), ".jpg"); !strings.HasPrefix(got, "照片 ") || !strings.HasSuffix(got, ".jpg") {
		t.Errorf("default name: %q", got)
	}
	if got := ingestName(req("", strings.Repeat("长", 120)+".jpg"), ""); len(got) > 200 || !strings.HasSuffix(got, ".jpg") {
		t.Errorf("long name not truncated safely: %d bytes", len(got))
	}
	if stripCopySuffix("IMG_1 (2).jpg") != "IMG_1.jpg" || withCopySuffix("IMG_1.jpg", 3) != "IMG_1 (3).jpg" {
		t.Error("copy suffix")
	}
}

// 集成测试：不需要认证就能往“相册”里传文件；已传过的跳过；同名不同内容改名；但不能用来看文件。
func TestIngest(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "it-ingest-"+uuid.NewString()[:8]) // 每次用新用户，相册从空开始
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	srv := New(svc, Options{UserID: userID, Token: "tok", Log: log})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	call := func(method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	tokenH := []string{"X-Xiaoju-Token", "tok"}

	jpeg := "\xFF\xD8\xFF\xE0" + strings.Repeat("x", 2500) // 跨多个分块
	ingest := func(name, body string) (int, map[string]any) {
		return call("POST", "/api/ingest", body, "X-File-Name", name)
	}
	if code, out := ingest("IMG_0001", jpeg); code != 201 || out["name"] != "IMG_0001.jpg" || out["result"] != "uploaded" {
		t.Fatalf("ingest: %d %v", code, out)
	}
	if code, out := ingest("IMG_0001", jpeg); code != 200 || out["result"] != "skipped" {
		t.Fatalf("same photo again must be skipped: %d %v", code, out)
	}
	if code, out := ingest("img_0001.JPG", jpeg); code != 200 || out["result"] != "skipped" {
		t.Fatalf("case-insensitive duplicate: %d %v", code, out)
	}
	if code, out := ingest("IMG_0001", jpeg+"y"); code != 201 || out["name"] != "IMG_0001 (2).jpg" {
		t.Fatalf("same name, different content: %d %v", code, out)
	}
	// 只进不出：不带认证不能列目录、下载
	if code, _ := call("GET", "/api/folders/root/children", ""); code != 401 {
		t.Fatalf("anonymous must not list files: %d", code)
	}

	// 文件都在“相册”里，内容能正确解密
	_, root := call("GET", "/api/folders/root/children", "", tokenH...)
	var albumID string
	for _, it := range root["items"].([]any) {
		if m := it.(map[string]any); m["name"] == AlbumFolder {
			albumID = m["id"].(string)
		}
	}
	_, album := call("GET", "/api/folders/"+albumID+"/children", "", tokenH...)
	if n := len(album["items"].([]any)); n != 2 {
		t.Fatalf("album has %d files, want 2", n)
	}
	first := album["items"].([]any)[1].(map[string]any)
	if first["mime"] != "image/jpeg" {
		t.Fatalf("mime from content: %v", first)
	}
	if code, _ := call("GET", "/api/files/"+first["id"].(string)+"/content", ""); code != 401 {
		t.Fatalf("anonymous must not download: %d", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/files/"+first["id"].(string)+"/content", nil)
	req.Header.Set("X-Xiaoju-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != jpeg {
		t.Fatalf("content mismatch: %d bytes", len(got))
	}

	// 大文件写存储端很慢：先回 200，期间发空格保持连接，最后才是 JSON
	ingestHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { ingestHeartbeat = 10 * time.Second })
	slow := service.New(store, slowDriver{driver, 120 * time.Millisecond}, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	ts2 := httptest.NewServer(New(slow, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts2.Close()
	video := "\x00\x00\x00\x14ftypqt  " + strings.Repeat("v", 2990) // 3 个分块，写入约 360ms
	req, _ = http.NewRequest("POST", ts2.URL+"/api/ingest", strings.NewReader(video))
	req.Header.Set("X-File-Name", "VID_0001")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response with heartbeats must still be valid JSON: %v %q", err, raw)
	}
	spaces := len(raw) - len(strings.TrimLeft(string(raw), " "))
	if resp.StatusCode != 200 || spaces < 3 || out["result"] != "uploaded" || out["name"] != "VID_0001.mov" {
		t.Fatalf("slow ingest: %d, %d heartbeat spaces, %v", resp.StatusCode, spaces, out)
	}
	if code, out := ingest("VID_0001", video); code != 200 || out["result"] != "skipped" {
		t.Fatalf("video re-run must be skipped: %d %v", code, out)
	}

	// 隐藏相册：X-Xiaoju-Album: hidden 放进根目录的“隐藏相册”，和“相册”分开查重；不认识的值拒绝（不能随意建文件夹）
	if code, out := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0001", "X-Xiaoju-Album", "hidden"); code != 201 || out["name"] != "IMG_0001.jpg" {
		t.Fatalf("hidden album ingest: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0001", "X-Xiaoju-Album", "Hidden"); code != 200 || out["result"] != "skipped" {
		t.Fatalf("hidden album re-run must be skipped: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0002", "X-Xiaoju-Album", "../etc"); code != 400 {
		t.Fatalf("unknown album must be rejected: %d", code)
	}
	_, root = call("GET", "/api/folders/root/children", "", tokenH...)
	var hiddenID string
	for _, it := range root["items"].([]any) {
		if m := it.(map[string]any); m["name"] == HiddenAlbumFolder {
			hiddenID = m["id"].(string)
		}
	}
	_, hidden := call("GET", "/api/folders/"+hiddenID+"/children", "", tokenH...)
	if hiddenID == "" || len(hidden["items"].([]any)) != 1 {
		t.Fatalf("hidden album folder: %q %v", hiddenID, hidden)
	}

	// 先问再传：同名同大小才算传过；名字没有扩展名时只比前面的部分；大小看不懂就当没传过
	size := strconv.Itoa(len(jpeg))
	for _, c := range []struct {
		name, size, album, want string
	}{
		{"IMG_0001.JPG", size, "hidden", "exists"},
		{"IMG_0001.jpg", size, "", "exists"},
		{"IMG_0001", size, "", "exists"},
		{"IMG_0001 (3).jpg", size, "", "exists"},
		{"IMG_0001.jpg", "1," + size[1:], "", "missing"}, // 大小不一样
		{"IMG_0001.jpg", "2.5 KB", "", "missing"},
		{"IMG_0001.jpg", "", "", "missing"},
		{"IMG_0009.jpg", size, "", "missing"},
	} {
		code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", c.name, "X-File-Size", c.size, "X-Xiaoju-Album", c.album)
		if code != 200 || out["result"] != c.want {
			t.Errorf("exists %q size %q album %q: %d %v, want %s", c.name, c.size, c.album, code, out, c.want)
		}
	}
	if len(jpeg) >= 1000 {
		if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_0001.jpg", "X-File-Size", size[:1]+","+size[1:]); code != 200 || out["result"] != "exists" {
			t.Errorf("size with a thousands separator: %d %v", code, out)
		}
	}
	if code, _ := call("GET", "/api/ingest/exists", "", "X-File-Name", "a.jpg", "X-File-Size", "1", "X-Xiaoju-Album", "x"); code != 400 {
		t.Errorf("unknown album must be rejected: %d", code)
	}
}

// slowDriver 模拟写 Telegram 很慢（大视频）
type slowDriver struct {
	storage.Driver
	delay time.Duration
}

func (d slowDriver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	time.Sleep(d.delay)
	return d.Driver.Put(ctx, req)
}
