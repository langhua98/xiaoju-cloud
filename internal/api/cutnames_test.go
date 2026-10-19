package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
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

func TestCutName(t *testing.T) {
	for in, want := range map[string]string{
		"video_啦啦啦#扭一扭_top_0_3": "video_",
		"@奶茶文":                  "@",
		"IMG_0001":              "IMG_0001",
		"微信图片_1":                "",
	} {
		if got := cutName(in); got != want {
			t.Errorf("cutName(%q) = %q, want %q", in, got, want)
		}
	}
	if !wasCut("video_#微胖_0", "video_#") || wasCut("img_1", "img_1") || wasCut("微信", "") {
		t.Error("wasCut")
	}
}

// 集成测试：以前被 iOS 截断名字存进来的文件（“video_.mp4”“@ (2).mp4”），上传计划和上传时认出来、改回真名；
// URL 编码的文件名能正确解码。
func TestRestoreCutNames(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "it-cut-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
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
	mp4 := func(n int, fill string) string { return "\x00\x00\x00\x18ftypmp42" + strings.Repeat(fill, n) }
	a, b, c := mp4(30000, "a"), mp4(20000, "b"), mp4(15000, "c")
	// 以前的快捷指令：名字到服务端只剩第一个中文字前面的部分
	for _, f := range []struct{ name, body string }{{"video_", a}, {"@", b}, {"@", c}} {
		if code, out := call("POST", "/api/ingest", f.body, "X-File-Name", f.name); code != 201 {
			t.Fatalf("old upload: %d %v", code, out)
		}
	}
	names := func() string {
		t.Helper()
		_, root := call("GET", "/api/folders/root/children", "", "X-Xiaoju-Token", "tok")
		var albumID string
		for _, it := range root["items"].([]any) {
			if m := it.(map[string]any); m["name"] == AlbumFolder {
				albumID = m["id"].(string)
			}
		}
		_, out := call("GET", "/api/folders/"+albumID+"/children", "", "X-Xiaoju-Token", "tok")
		var n []string
		for _, it := range out["items"].([]any) {
			n = append(n, it.(map[string]any)["name"].(string))
		}
		sort.Strings(n)
		return strings.Join(n, ",")
	}
	if got := names(); got != "@ (2).mp4,@.mp4,video_.mp4" {
		t.Fatalf("stored names: %s", got)
	}

	// 上传计划带着完整的名字和大小：认出两个、改回真名；第三个名字截断后也是“@”但大小对不上，照常上传
	plan, _ := json.Marshal(map[string]string{
		"album": "",
		"names": "video_啦啦啦#扭一扭_top_0_3\n@奶茶文\n@别的视频",
		"sizes": "30 KB\n20 KB\n99 KB",
	})
	code, out := call("POST", "/api/ingest/plan", string(plan))
	if code != 200 || out["todo"] != "3" {
		t.Fatalf("plan: %d %v", code, out)
	}
	if got := names(); got != "@ (2).mp4,@奶茶文.mp4,video_啦啦啦#扭一扭_top_0_3.mp4" {
		t.Fatalf("after plan: %s", got)
	}

	// 新版快捷指令：名字做了 URL 编码。上传时按完全一样的大小认出剩下那个截断的，改回真名
	encoded := url.PathEscape("@奶茶文二.mp4")
	if code, out := call("POST", "/api/ingest", c, "X-File-Name", encoded); code != 200 || out["result"] != "skipped" {
		t.Fatalf("upload with the real name: %d %v", code, out)
	}
	if got := names(); got != "@奶茶文.mp4,@奶茶文二.mp4,video_啦啦啦#扭一扭_top_0_3.mp4" {
		t.Fatalf("after upload: %s", got)
	}
	// 新文件：URL 编码的中文名正确解码
	if code, out := call("POST", "/api/ingest", mp4(100, "n"), "X-File-Name", url.PathEscape("新视频 #1.mp4")); code != 201 || out["name"] != "新视频 #1.mp4" {
		t.Fatalf("encoded name: %d %v", code, out)
	}
}
