package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/internal/thumb"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：照片的下载链接带着大图预览地址，预览是缩小过的 JPEG；不是照片的没有预览；链接签名不对 403。
func TestPreviewLinks(t *testing.T) {
	dbURL := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	big := filepath.Join(t.TempDir(), "big.jpg")
	if b, err := exec.Command(ff, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=3200x2400", "-frames:v", "1", "-y", big).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	photo, _ := os.ReadFile(big)
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
	userID, _ := store.EnsureUser(ctx, "it-preview-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 256 << 10, TempDir: t.TempDir(), Log: log, Thumbs: thumb.New(t.TempDir())})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()
	call := func(method, path string, body []byte) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Xiaoju-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	link := func(name string, body []byte) map[string]any {
		t.Helper()
		_, b := call("POST", "/api/files?name="+name, body)
		var f meta.File
		json.Unmarshal(b, &f)
		_, b = call("POST", "/api/files/"+f.ID.String()+"/link", nil)
		var l map[string]any
		json.Unmarshal(b, &l)
		return l
	}
	l := link("big.jpg", photo)
	pv, _ := l["preview"].(string)
	if pv == "" || !strings.HasPrefix(pv, "/pv/") {
		t.Fatalf("link: %v", l)
	}
	for range 2 { // 第二次从内存里取
		resp, b := call("GET", pv, nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("preview: %d %s", resp.StatusCode, b)
		}
		img, _, err := image.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 1600 || img.Bounds().Dy() != 1200 || len(b) >= len(photo) {
			t.Fatalf("preview %v: %v, %d bytes (original %d)", img.Bounds(), err, len(b), len(photo))
		}
	}
	if resp, _ := call("GET", strings.Replace(pv, "sig=", "sig=0", 1), nil); resp.StatusCode != 403 {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	if l := link("a.txt", []byte("hello")); l["preview"] != nil {
		t.Fatalf("text file must not have a preview: %v", l)
	}
}
