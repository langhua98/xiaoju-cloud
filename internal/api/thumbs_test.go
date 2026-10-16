package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/internal/thumb"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：列目录给出缩略图地址，缩略图能生成、存库、复用；签名不能和下载链接互换；做不了的返回 404。
func TestThumbnails(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	sample := func(name string, args ...string) string {
		out := filepath.Join(t.TempDir(), name)
		if b, err := exec.Command(ff, append(append([]string{"-nostdin", "-v", "error"}, args...), "-y", out)...).CombinedOutput(); err != nil {
			t.Fatalf("make sample: %v\n%s", err, b)
		}
		b, _ := os.ReadFile(out)
		return string(b)
	}
	photo := sample("p.jpg", "-f", "lavfi", "-i", "testsrc=size=800x600", "-frames:v", "1")
	video := sample("v.mp4", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=2", "-c:v", "libx264", "-pix_fmt", "yuv420p")

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
	userID, _ := store.EnsureUser(ctx, "it-thumbs")
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{
		MasterKey: master, PartSize: 64 << 10, TempDir: t.TempDir(), Log: log, Thumbs: thumb.New(t.TempDir()),
	})
	srv := New(svc, Options{UserID: userID, Token: "tok", Log: log})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(method, path, body string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	tok := []string{"X-Xiaoju-Token", "tok"}
	folder := fmt.Sprintf("it-thumbs-%d", time.Now().UnixNano())
	resp := do("POST", "/api/folders", fmt.Sprintf(`{"name":%q}`, folder), tok...)
	var dir meta.File
	json.NewDecoder(resp.Body).Decode(&dir)
	resp.Body.Close()
	upload := func(name, mime, body string) string {
		t.Helper()
		resp := do("POST", "/api/files?parent_id="+dir.ID.String()+"&name="+name, body, append(tok, "Content-Type", mime)...)
		defer resp.Body.Close()
		var f meta.File
		json.NewDecoder(resp.Body).Decode(&f)
		if resp.StatusCode != 201 {
			t.Fatalf("upload %s: %d", name, resp.StatusCode)
		}
		t.Cleanup(func() { svc.Purge(context.Background(), userID, f.ID) })
		return f.ID.String()
	}
	photoID := upload("p.jpg", "image/jpeg", photo)
	videoID := upload("v.mp4", "video/mp4", video)
	brokenID := upload("broken.jpg", "image/jpeg", "\xFF\xD8\xFF\xE0 not really a jpeg")
	docID := upload("a.txt", "text/plain", "hello")

	type item struct {
		ID       string  `json:"id"`
		Thumb    string  `json:"thumb"`
		Duration float64 `json:"duration"`
	}
	list := func() map[string]item {
		t.Helper()
		resp := do("GET", "/api/folders/"+dir.ID.String()+"/children", "", tok...)
		defer resp.Body.Close()
		var out struct{ Items []item }
		json.NewDecoder(resp.Body).Decode(&out)
		m := map[string]item{}
		for _, it := range out.Items {
			m[it.ID] = it
		}
		return m
	}
	items := list()
	for _, id := range []string{photoID, videoID} {
		if !strings.HasPrefix(items[id].Thumb, "/thumb/"+id+"?exp=") {
			t.Fatalf("media file %s has no thumbnail link: %+v", id, items[id])
		}
	}
	if items[docID].Thumb != "" {
		t.Fatalf("text file must not get a thumbnail: %+v", items[docID])
	}

	getThumb := func(link string) (int, []byte, http.Header) {
		t.Helper()
		resp := do("GET", link, "")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	for _, id := range []string{photoID, videoID} {
		code, b, h := getThumb(items[id].Thumb)
		if code != 200 || h.Get("Content-Type") != "image/jpeg" || !strings.Contains(h.Get("Cache-Control"), "max-age=") {
			t.Fatalf("thumbnail %s: %d %v %s", id, code, h, b)
		}
		img, _, err := image.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != thumb.Size {
			t.Fatalf("thumbnail %s is not a %dpx jpeg: %v", id, thumb.Size, err)
		}
		// 存进了数据库（加密），下次直接用
		th, err := store.GetThumbnail(ctx, uuid.MustParse(id))
		if err != nil || bytes.Contains(th.Data, b[:64]) {
			t.Fatalf("thumbnail %s not stored encrypted: %v", id, err)
		}
	}
	if d := list()[videoID].Duration; d < 1.9 || d > 2.1 {
		t.Fatalf("video duration in listing: %v", d)
	}

	// 签名：改了不行；缩略图签名不能下载原文件，下载签名也不能当缩略图签名
	link := items[photoID].Thumb
	if code, _, _ := getThumb(strings.Replace(link, "sig=", "sig=0", 1)); code != 403 {
		t.Fatalf("tampered signature: %d", code)
	}
	if code, _, _ := getThumb(strings.Replace(link, "/thumb/", "/dl/", 1)); code != 403 {
		t.Fatalf("thumbnail signature must not download the original: %d", code)
	}
	resp = do("POST", "/api/files/"+photoID+"/link", "", tok...)
	var dl struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&dl)
	resp.Body.Close()
	if code, _, _ := getThumb(strings.Replace(dl.URL, "/dl/", "/thumb/", 1)); code != 403 {
		t.Fatalf("download signature must not work for thumbnails: %d", code)
	}

	// 解不开的文件：404，之后列目录不再给地址（上传后后台可能已经试过了，所以自己签一个地址）
	exp := time.Now().Add(time.Hour).Unix()
	code, b, _ := getThumb(fmt.Sprintf("/thumb/%s?exp=%d&sig=%s", brokenID, exp, srv.thumbSig(uuid.MustParse(brokenID), exp)))
	if code != 404 || !strings.Contains(string(b), "no_thumbnail") {
		t.Fatalf("broken image: %d %s", code, b)
	}
	if list()[brokenID].Thumb != "" {
		t.Fatal("broken image still advertised a thumbnail")
	}

	// 永久删除时缩略图一起删掉
	if err := svc.Purge(ctx, userID, uuid.MustParse(photoID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetThumbnail(ctx, uuid.MustParse(photoID)); err != meta.ErrNotFound {
		t.Fatalf("thumbnail left behind after purge: %v", err)
	}
}
