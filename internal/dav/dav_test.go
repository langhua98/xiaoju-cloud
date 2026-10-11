package dav

import (
	"bytes"
	"context"
	"crypto/rand"
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

// 集成测试：需要一个 PostgreSQL 数据库，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestWebDAVIntegration(t *testing.T) {
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
	driver, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "it-"+t.Name()+"-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(Handler(svc, userID, "tok", log))
	defer ts.Close()

	escaped := func(p string) string { return (&url.URL{Path: p}).EscapedPath() }
	do := func(method, p string, body []byte, hdr ...string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+escaped(p), bytes.NewReader(body))
		req.SetBasicAuth("anyone", "tok")
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
	expect := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: status %d, want %d", what, got, want)
		}
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		rand.Read(b)
		return b
	}

	// 认证
	resp, err := http.Get(ts.URL + "/dav/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no auth: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	code, _ := do("MKCOL", "/dav/相册", nil)
	expect("MKCOL", code, http.StatusCreated)
	code, _ = do("MKCOL", "/dav/相册", nil)
	expect("MKCOL existing", code, http.StatusMethodNotAllowed)

	a := random(3500) // 4 个分块
	code, _ = do("PUT", "/dav/相册/IMG_0001.jpg", a)
	expect("PUT", code, http.StatusCreated)
	code, _ = do("PUT", "/dav/不存在/x.jpg", a)
	expect("PUT into missing folder", code, http.StatusConflict)

	code, body := do("PROPFIND", "/dav/相册", nil, "Depth", "1")
	expect("PROPFIND", code, http.StatusMultiStatus)
	if !bytes.Contains(body, []byte("IMG_0001.jpg")) || !bytes.Contains(body, []byte("<D:getcontentlength>3500</D:getcontentlength>")) ||
		!bytes.Contains(body, []byte("image/jpeg")) {
		t.Fatalf("PROPFIND body missing file props:\n%s", body)
	}
	code, body = do("PROPFIND", "/dav/", nil, "Depth", "1")
	expect("PROPFIND root", code, http.StatusMultiStatus)
	if !bytes.Contains(body, []byte(escaped("/dav/相册/"))) && !bytes.Contains(body, []byte("相册")) {
		t.Fatalf("root listing missing folder:\n%s", body)
	}

	code, got := do("GET", "/dav/相册/IMG_0001.jpg", nil)
	expect("GET", code, http.StatusOK)
	if !bytes.Equal(got, a) {
		t.Fatal("GET content mismatch")
	}
	code, got = do("GET", "/dav/相册/IMG_0001.jpg", nil, "Range", "bytes=995-2004")
	expect("GET range", code, http.StatusPartialContent)
	if !bytes.Equal(got, a[995:2005]) {
		t.Fatal("range content mismatch")
	}

	// 覆盖：内容替换，目录里只剩一个文件，没有临时文件残留
	b := random(1200)
	code, _ = do("PUT", "/dav/相册/IMG_0001.jpg", b)
	expect("PUT overwrite", code, http.StatusCreated) // x/net/webdav 对成功的 PUT 一律返回 201
	_, got = do("GET", "/dav/相册/IMG_0001.jpg", nil)
	if !bytes.Equal(got, b) {
		t.Fatal("overwrite content mismatch")
	}
	album, err := store.FindFolder(ctx, userID, nil, "相册")
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := store.ListChildren(ctx, userID, &album.ID); len(items) != 1 || items[0].Name != "IMG_0001.jpg" {
		t.Fatalf("after overwrite, album has %d items: %+v", len(items), items)
	}

	// 重命名 / 移动
	dest := ts.URL + escaped("/dav/相册/IMG_0002.jpg")
	code, _ = do("MOVE", "/dav/相册/IMG_0001.jpg", nil, "Destination", dest)
	expect("MOVE", code, http.StatusCreated)
	code, _ = do("GET", "/dav/相册/IMG_0001.jpg", nil)
	expect("GET old name", code, http.StatusNotFound)
	_, got = do("GET", "/dav/相册/IMG_0002.jpg", nil)
	if !bytes.Equal(got, b) {
		t.Fatal("moved content mismatch")
	}
	code, _ = do("MKCOL", "/dav/相册/子目录", nil)
	expect("MKCOL sub", code, http.StatusCreated)
	code, _ = do("MOVE", "/dav/相册", nil, "Destination", ts.URL+escaped("/dav/相册/子目录/相册"))
	expect("MOVE folder into itself", code, http.StatusForbidden)

	// 删除进回收站
	code, _ = do("DELETE", "/dav/相册/IMG_0002.jpg", nil)
	expect("DELETE", code, http.StatusNoContent)
	code, _ = do("GET", "/dav/相册/IMG_0002.jpg", nil)
	expect("GET deleted", code, http.StatusNotFound)
	code, _ = do("DELETE", "/dav/", nil)
	if code < 400 {
		t.Fatalf("DELETE root allowed: %d", code)
	}
}
