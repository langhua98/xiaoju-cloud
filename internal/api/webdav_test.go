package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// 集成测试：需要一个 PostgreSQL 数据库，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestWebDAV(t *testing.T) {
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

	// 没设密码时不开 WebDAV
	off := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	resp, err := http.Get(off.URL + "/dav/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	off.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("webdav without password: status %d, want 404", resp.StatusCode)
	}

	const pass = "dav-password-123"
	srv := New(svc, Options{UserID: userID, Token: "tok", Log: log, WebDAVPassword: pass})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	escaped := func(p string) string { return (&url.URL{Path: p}).EscapedPath() }
	doAs := func(password, method, p string, body []byte, hdr ...string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+escaped(p), bytes.NewReader(body))
		if password != "" {
			req.SetBasicAuth("anyone", password)
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
	do := func(method, p string, body []byte, hdr ...string) (int, []byte) {
		t.Helper()
		return doAs(pass, method, p, body, hdr...)
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

	// 认证：管理员令牌不能当 WebDAV 密码
	st, _ := doAs("", "PROPFIND", "/dav/", nil, "Depth", "0")
	expect("no auth", st, http.StatusUnauthorized)
	st, _ = doAs("tok", "PROPFIND", "/dav/", nil, "Depth", "0")
	expect("admin token as password", st, http.StatusUnauthorized)
	st, _ = do("PROPFIND", "/dav/", nil, "Depth", "0")
	expect("propfind root", st, http.StatusMultiStatus)

	// 建文件夹、上传（3500 字节，分 4 块）
	st, _ = do("MKCOL", "/dav/相册", nil)
	expect("mkcol", st, http.StatusCreated)
	photo := random(3500)
	st, _ = do("PUT", "/dav/相册/IMG_0001.jpg", photo)
	expect("put", st, http.StatusCreated)
	st, got := do("GET", "/dav/相册/IMG_0001.jpg", nil)
	expect("get", st, http.StatusOK)
	if !bytes.Equal(got, photo) {
		t.Fatal("downloaded content differs")
	}
	st, got = do("GET", "/dav/相册/IMG_0001.jpg", nil, "Range", "bytes=990-2010")
	expect("range", st, http.StatusPartialContent)
	if !bytes.Equal(got, photo[990:2011]) {
		t.Fatal("range content differs")
	}
	folder, err := store.FindChild(ctx, userID, nil, "相册")
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.FindChild(ctx, userID, &folder.ID, "IMG_0001.jpg")
	if err != nil || f.Size != 3500 || f.Mime != "image/jpeg" {
		t.Fatalf("stored file = %+v, %v", f, err)
	}

	// 私密文件夹：列不出来，按路径也进不去，也不能往里传
	priv, err := store.CreateFolder(ctx, userID, nil, "隐藏相册")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := store.UpdateFile(ctx, userID, priv.ID, meta.FileUpdate{Private: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateFolder(ctx, userID, &priv.ID, "子文件夹"); err != nil {
		t.Fatal(err)
	}
	st, got = do("PROPFIND", "/dav/", nil, "Depth", "1")
	expect("propfind depth 1", st, http.StatusMultiStatus)
	listing, _ := url.PathUnescape(string(got))
	if !strings.Contains(listing, "相册") || strings.Contains(listing, "隐藏相册") {
		t.Fatalf("root listing should show 相册 but not 隐藏相册:\n%s", got)
	}
	st, _ = do("PROPFIND", "/dav/隐藏相册/子文件夹", nil, "Depth", "0")
	expect("propfind inside private", st, http.StatusNotFound)
	st, _ = do("PUT", "/dav/隐藏相册/x.jpg", []byte("x"))
	expect("put into private", st, http.StatusConflict)

	// 覆盖：新内容生效，旧的进回收站
	photo2 := random(1200)
	st, _ = do("PUT", "/dav/相册/IMG_0001.jpg", photo2)
	expect("overwrite", st, http.StatusCreated)
	_, got = do("GET", "/dav/相册/IMG_0001.jpg", nil)
	if !bytes.Equal(got, photo2) {
		t.Fatal("overwrite did not replace content")
	}
	if old, err := store.Get(ctx, userID, f.ID); err != nil || old.TrashedAt == nil {
		t.Fatalf("overwritten file should be in the trash: %+v, %v", old, err)
	}

	// 移动、改名
	st, _ = do("MKCOL", "/dav/相册/2026", nil)
	expect("mkcol sub", st, http.StatusCreated)
	st, _ = do("MOVE", "/dav/相册/IMG_0001.jpg", nil, "Destination", ts.URL+escaped("/dav/相册/2026/新名字.jpg"))
	expect("move", st, http.StatusCreated)
	st, got = do("GET", "/dav/相册/2026/新名字.jpg", nil)
	expect("get moved", st, http.StatusOK)
	if !bytes.Equal(got, photo2) {
		t.Fatal("moved content differs")
	}
	st, _ = do("MOVE", "/dav/相册", nil, "Destination", ts.URL+escaped("/dav/相册/2026/相册"))
	expect("move into itself", st, http.StatusForbidden)

	// 删除：进回收站
	moved, err := store.FindChild(ctx, userID, nil, "相册")
	if err != nil {
		t.Fatal(err)
	}
	st, _ = do("DELETE", "/dav/相册", nil)
	expect("delete", st, http.StatusNoContent)
	if d, err := store.Get(ctx, userID, moved.ID); err != nil || d.TrashedAt == nil {
		t.Fatalf("deleted folder should be in the trash: %+v, %v", d, err)
	}
	st, _ = do("GET", "/dav/相册/2026/新名字.jpg", nil)
	expect("get after delete", st, http.StatusNotFound)

	// 客户端传到一半断开：不能存下半截文件
	u, _ := url.Parse(ts.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Basic " + basic("anyone", pass)
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Length: 5000\r\n\r\n",
		escaped("/dav/half.bin"), u.Host, auth)
	conn.Write(random(2500))
	deadline := time.Now().Add(5 * time.Second)
	for srv.act.active.Load() == 0 && time.Now().Before(deadline) { // 等服务端开始收
		time.Sleep(5 * time.Millisecond)
	}
	conn.Close()
	for srv.act.active.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := store.FindChild(ctx, userID, nil, "half.bin"); err != meta.ErrNotFound {
		t.Fatalf("half-uploaded file was stored: %v", err)
	}

	// 密码错太多次：先停一会儿，对的密码也不行
	for range 10 {
		doAs("wrong-password", "PROPFIND", "/dav/", nil, "Depth", "0")
	}
	st, _ = do("PROPFIND", "/dav/", nil, "Depth", "0")
	expect("after too many failures", st, http.StatusTooManyRequests)
}

func basic(user, pass string) string {
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth(user, pass)
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
}
