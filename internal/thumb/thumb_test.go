package thumb

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/media"
)

// 用 ffmpeg 生成测试素材；没装 ffmpeg 时跳过。
func sample(t *testing.T, name string, args ...string) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(ff, append(append([]string{"-nostdin", "-v", "error"}, args...), "-y", out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make sample: %v\n%s", err, b)
	}
	return out
}

func fileSource(path string) Source {
	return func(context.Context) (io.ReadSeekCloser, error) { return os.Open(path) }
}

func checkJPEG(t *testing.T, b []byte) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	if format != "jpeg" || img.Bounds().Dx() != Size || img.Bounds().Dy() != Size {
		t.Fatalf("thumbnail is %s %v, want %dx%d jpeg", format, img.Bounds(), Size, Size)
	}
}

func TestImage(t *testing.T) {
	in := sample(t, "a.jpg", "-f", "lavfi", "-i", "testsrc=size=800x600", "-frames:v", "1")
	g := New(t.TempDir())
	for _, vips := range []bool{true, false} {
		if vips && g.vips == "" {
			continue
		}
		gg := *g
		if !vips {
			gg.vips = "" // ffmpeg 兜底
		}
		res, err := gg.Generate(context.Background(), fileSource(in))
		if err != nil {
			t.Fatalf("vips=%v: %v", vips, err)
		}
		checkJPEG(t, res.JPEG)
		if res.Duration != 0 {
			t.Errorf("photo has duration %v", res.Duration)
		}
	}
}

// iPhone 的 HEIC 只能用 vips（libheif）解；没装 vips 时跳过（服务器镜像里由 CI 检查）。
func TestHEIC(t *testing.T) {
	g := New(t.TempDir())
	if g.vips == "" {
		t.Skip("vips not installed")
	}
	res, err := g.Generate(context.Background(), fileSource("testdata/sample.heic"))
	if err != nil {
		t.Fatal(err)
	}
	checkJPEG(t, res.JPEG)
}

func TestVideo(t *testing.T) {
	in := sample(t, "v.mp4", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart")
	// 索引（moov）在文件末尾的也要能读（iPhone 拍的视频常常这样）
	tail := sample(t, "tail.mov", "-f", "lavfi", "-i", "testsrc=size=360x640:rate=25:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p")
	g := New(t.TempDir())
	for path, want := range map[string]time.Duration{in: 3 * time.Second, tail: 2 * time.Second} {
		res, err := g.Generate(context.Background(), fileSource(path))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		checkJPEG(t, res.JPEG)
		if d := res.Duration - want; d < -100*time.Millisecond || d > 100*time.Millisecond {
			t.Errorf("%s: duration %v, want %v", filepath.Base(path), res.Duration, want)
		}
	}
}

func TestUnsupported(t *testing.T) {
	g := New(t.TempDir())
	dir := t.TempDir()
	for name, content := range map[string]string{
		"text":    "hello world, not an image",
		"broken":  "\xFF\xD8\xFF\xE0 this is not really a jpeg",
		"empty":   "",
		"m3u8":    "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10.0,\nfile:///etc/passwd\n",
		"fakemp4": "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00 then garbage",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := g.Generate(context.Background(), fileSource(p)); !errors.Is(err, ErrNoThumbnail) {
			t.Errorf("%s: got %v, want ErrNoThumbnail", name, err)
		}
	}
}

func TestSourceError(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	in := sample(t, "v.mp4", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	boom := errors.New("telegram unavailable")
	opened := 0
	src := func(context.Context) (io.ReadSeekCloser, error) {
		opened++
		if opened == 1 { // 认格式时能读，ffmpeg 来读时存储端出错
			return os.Open(in)
		}
		return nil, boom
	}
	_, err := New(t.TempDir()).Generate(context.Background(), src)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the storage error (so it can be retried)", err)
	}
}

func TestPhotoTaken(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Time{
		"taken.jpg":   time.Date(2023, 9, 4, 4, 15, 16, 0, time.UTC),    // 12:15:16 +08:00（EXIF 里写了时区）
		"taken.heic":  time.Date(2021, 2, 2, 20, 5, 6, 0, time.UTC),     // 04:05:06，没写时区，按上海时间算
		"taken.png":   time.Date(2019, 12, 31, 15, 59, 58, 0, time.UTC), // 只有 IFD0 的 DateTime
		"sample.heic": {},                                               // 没有 EXIF
	} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		_, typ := media.Sniff(b)
		if got := photoTaken(bytes.NewReader(b), int64(len(b)), typ, shanghai); !got.Equal(want) {
			t.Errorf("%s (%s): taken %v, want %v", name, typ, got, want)
		}
		// 截断的、改坏的文件：读不出来就算了，不能出错
		for n := 0; n < len(b); n++ {
			bad := append([]byte(nil), b...)
			bad[n] ^= 0xA5
			photoTaken(bytes.NewReader(bad), int64(len(bad)), typ, shanghai)
			photoTaken(bytes.NewReader(b[:n]), int64(n), typ, shanghai)
		}
	}

	// 生成缩略图时一起读出来
	g := New(t.TempDir())
	g.Location = shanghai
	res, err := g.Generate(context.Background(), fileSource("testdata/taken.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2023, 9, 4, 4, 15, 16, 0, time.UTC); !res.Taken.Equal(want) {
		t.Errorf("Generate: taken %v, want %v", res.Taken, want)
	}
}

func TestVideoTaken(t *testing.T) {
	src := []string{"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p"}
	iphone := sample(t, "iphone.mov", append(src, "-movflags", "use_metadata_tags",
		"-metadata", "com.apple.quicktime.creationdate=2023-09-04T12:15:16+0800", "-metadata", "creation_time=2023-09-04T04:15:20Z")...)
	plain := sample(t, "plain.mp4", append(src, "-metadata", "creation_time=2022-05-06T07:08:09Z")...)
	none := sample(t, "none.mp4", src...)
	g := New(t.TempDir())
	for path, want := range map[string]time.Time{
		iphone: time.Date(2023, 9, 4, 4, 15, 16, 0, time.UTC), // 优先用 iPhone 写的（带时区）
		plain:  time.Date(2022, 5, 6, 7, 8, 9, 0, time.UTC),
		none:   {},
	} {
		res, err := g.Generate(context.Background(), fileSource(path))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !res.Taken.Equal(want) {
			t.Errorf("%s: taken %v, want %v", filepath.Base(path), res.Taken, want)
		}
	}
	if got := videoTaken("    creation_time   : 1970-01-01T00:00:00.000000Z\n"); !got.IsZero() {
		t.Errorf("unset camera clock read as %v", got)
	}
}
