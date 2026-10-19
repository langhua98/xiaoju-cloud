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
	video := sample("v.mp4", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=2", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2022-05-06T07:08:09Z")
	exif, err := os.ReadFile("../thumb/testdata/taken.jpg") // EXIF 里的拍摄时间是 2023-09-04 12:15:16 +08:00
	if err != nil {
		t.Fatal(err)
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
	oldID := upload("old.jpg", "image/jpeg", string(exif))
	for _, name := range []string{"b-sub", "a-sub"} {
		resp := do("POST", "/api/folders", fmt.Sprintf(`{"name":%q,"parent_id":%q}`, name, dir.ID), tok...)
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("create folder %s: %d", name, resp.StatusCode)
		}
	}

	type item struct {
		ID       string     `json:"id"`
		Name     string     `json:"name"`
		Thumb    string     `json:"thumb"`
		Duration float64    `json:"duration"`
		TakenAt  *time.Time `json:"taken_at"`
	}
	var order []string
	list := func() map[string]item {
		t.Helper()
		resp := do("GET", "/api/folders/"+dir.ID.String()+"/children", "", tok...)
		defer resp.Body.Close()
		var out struct{ Items []item }
		json.NewDecoder(resp.Body).Decode(&out)
		m := map[string]item{}
		order = order[:0]
		for _, it := range out.Items {
			m[it.ID] = it
			order = append(order, it.Name)
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

	// 拍摄时间：生成缩略图时读出来；排序是文件夹在前（按名字），文件按拍摄时间新的在前，没有拍摄时间的按上传时间
	if _, err := svc.Thumbnail(ctx, userID, uuid.MustParse(oldID)); err != nil {
		t.Fatal(err)
	}
	takenOld := time.Date(2023, 9, 4, 4, 15, 16, 0, time.UTC)
	items = list()
	if tk := items[oldID].TakenAt; tk == nil || !tk.Equal(takenOld) {
		t.Fatalf("photo taken_at: %v", tk)
	}
	if tk := items[videoID].TakenAt; tk == nil || !tk.Equal(time.Date(2022, 5, 6, 7, 8, 9, 0, time.UTC)) {
		t.Fatalf("video taken_at: %v", tk)
	}
	// 没有拍摄时间的 a.txt、broken.jpg、p.jpg 按上传时间算（刚传的，比 2023 年拍的 old.jpg 新），后传的在前
	if got, want := strings.Join(order, ","), "a-sub,b-sub,a.txt,broken.jpg,p.jpg,old.jpg,v.mp4"; got != want {
		t.Fatalf("listing order: %s, want %s", got, want)
	}

	// 旧版本生成的缩略图（那时没读拍摄时间）：启动后在后台重新生成，补上拍摄时间
	if _, err := pool.Exec(ctx, `UPDATE thumbnails SET version = 0 WHERE file_id = $1`, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE files SET taken_at = NULL WHERE id = $1`, oldID); err != nil {
		t.Fatal(err)
	}
	svc.BackfillThumbnails(ctx)
	if th, err := store.GetThumbnail(ctx, uuid.MustParse(oldID)); err != nil || th.Version != meta.ThumbnailVersion {
		t.Fatalf("backfill did not regenerate the old thumbnail: %+v %v", th, err)
	}
	if tk := list()[oldID].TakenAt; tk == nil || !tk.Equal(takenOld) {
		t.Fatalf("backfill did not restore taken_at: %v", tk)
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

	// 根目录里这个文件夹的封面：最新的、有缩略图的照片或视频（broken.jpg 更新，但做不了缩略图）
	resp = do("GET", "/api/folders/root/children", "", tok...)
	var root struct {
		Items []struct {
			ID     string `json:"id"`
			Cover  string `json:"cover"`
			Counts struct{ Photos, Videos, Others int }
		}
	}
	json.NewDecoder(resp.Body).Decode(&root)
	resp.Body.Close()
	var cover string
	for _, it := range root.Items {
		if it.ID == dir.ID.String() {
			cover = it.Cover
			if it.Counts.Photos != 3 || it.Counts.Videos != 1 || it.Counts.Others != 1 {
				t.Errorf("folder counts: %+v", it.Counts)
			}
		}
	}
	if !strings.HasPrefix(cover, "/thumb/"+photoID+"?exp=") {
		t.Fatalf("folder cover: %q, want the thumbnail of %s", cover, photoID)
	}
	if code, _, _ := getThumb(cover); code != 200 {
		t.Fatalf("cover link: %d", code)
	}

	// 永久删除时缩略图一起删掉
	if err := svc.Purge(ctx, userID, uuid.MustParse(photoID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetThumbnail(ctx, uuid.MustParse(photoID)); err != meta.ErrNotFound {
		t.Fatalf("thumbnail left behind after purge: %v", err)
	}
}
