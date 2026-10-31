// Package thumb 生成照片、视频的缩略图（相册网格用）：正方形、居中裁剪的 JPEG。
//
// 照片用 libvips 的 vips 命令（认得 iPhone 的 HEIC，按 EXIF 方向转正，转成 sRGB），视频用 ffmpeg 截第一帧。
// 没装 vips 时 JPEG、PNG、WebP、GIF 也用 ffmpeg；两个都没装就不生成，网页显示图标。
//
// 文件可能来自不需要认证的批量上传接口，所以先按文件开头的字节认格式，只把认得的格式交给外部程序，
// 并给 ffmpeg 指定解封装器、只允许读本机的地址，防止伪装成视频的播放列表让它去读别的文件或网址。
package thumb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/media"
)

const (
	// Size 是缩略图边长（像素）。手机上一行 3 格时每格约 130pt，3 倍屏约 390 像素，320 略放大也看不出糊，
	// 每张约 15 KB。
	Size    = 320
	quality = 72

	maxImageBytes = 200 << 20 // 再大的“照片”不生成（要整个拷到临时文件）
	imageTimeout  = time.Minute
	videoTimeout  = 2 * time.Minute
)

// ErrNoThumbnail 表示这个文件做不了缩略图（格式不支持或文件损坏），重试也没用。
var ErrNoThumbnail = errors.New("thumbnail: unsupported or undecodable file")

type Result struct {
	JPEG     []byte
	Duration time.Duration // 视频时长（照片为 0）
	Taken    time.Time     // 拍摄时间（读不到时为零值）
}

// Source 打开文件明文的一个独立读取器。读视频时 ffmpeg 会跳着读，可能同时发几个请求，每个请求各开一个。
type Source func(ctx context.Context) (io.ReadSeekCloser, error)

type Generator struct {
	vips, ffmpeg string
	tempDir      string
	// Location：照片的 EXIF 没写时区时，拍摄时间按这个时区算（nil 时按 UTC）。
	Location *time.Location
}

// New 在 PATH 里找 vips 和 ffmpeg；tempDir 为空时用系统临时目录。
func New(tempDir string) *Generator {
	g := &Generator{tempDir: tempDir}
	g.vips, _ = exec.LookPath("vips")
	g.ffmpeg, _ = exec.LookPath("ffmpeg")
	return g
}

// Tools 返回找到的外部程序（记日志用）。
func (g *Generator) Tools() []string {
	var t []string
	if g.vips != "" {
		t = append(t, "vips")
	}
	if g.ffmpeg != "" {
		t = append(t, "ffmpeg")
	}
	return t
}

// ffmpeg 能直接解的照片格式，以及对应的解封装器
var ffmpegImage = map[string]string{
	"image/jpeg": "jpeg_pipe", "image/png": "png_pipe", "image/gif": "gif", "image/webp": "webp_pipe",
}

// Supported 按数据库里记录的 MIME 类型判断能不能生成（列目录时用，不读文件；真正的格式以文件内容为准）。
func (g *Generator) Supported(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return g.vips != "" || g.ffmpeg != ""
	case "image/heic", "image/heif", "image/avif":
		return g.vips != ""
	case "video/mp4", "video/quicktime", "video/x-m4v", "video/3gpp", "video/webm", "video/x-matroska":
		return g.ffmpeg != ""
	}
	return false
}

// Generate 读文件开头认出格式，生成缩略图。做不了时返回 ErrNoThumbnail；读文件出错等其他错误可以稍后重试。
func (g *Generator) Generate(ctx context.Context, src Source) (*Result, error) {
	rd, err := src(ctx)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 64)
	n, err := io.ReadFull(rd, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		rd.Close()
		return nil, err
	}
	ext, typ := media.Sniff(head[:n])
	switch {
	case strings.HasPrefix(typ, "image/"):
		defer rd.Close()
		return g.image(ctx, rd, ext, typ)
	case strings.HasPrefix(typ, "video/"):
		rd.Close()
		return g.video(ctx, src, typ)
	}
	rd.Close()
	return nil, fmt.Errorf("%w: unrecognized header %x", ErrNoThumbnail, head[:min(n, 16)])
}

// copyImage 把照片拷进临时文件交给外部程序（vips 要能跳着读）；太大的不做。用完要删掉返回的文件。
func (g *Generator) copyImage(rd io.ReadSeeker, ext string) (path string, size int64, err error) {
	if size, err = rd.Seek(0, io.SeekEnd); err != nil {
		return "", 0, err
	}
	if size > maxImageBytes {
		return "", 0, ErrNoThumbnail
	}
	if _, err := rd.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	in, err := os.CreateTemp(g.tempDir, "xiaoju-thumb-*"+ext)
	if err != nil {
		return "", 0, err
	}
	_, err = io.Copy(in, rd)
	if cerr := in.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(in.Name())
		return "", 0, err
	}
	return in.Name(), size, nil
}

func (g *Generator) image(ctx context.Context, rd io.ReadSeekCloser, ext, typ string) (*Result, error) {
	demuxer := ffmpegImage[typ]
	if g.vips == "" && (g.ffmpeg == "" || demuxer == "") {
		return nil, ErrNoThumbnail
	}
	path, size, err := g.copyImage(rd, ext)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	var taken time.Time
	if f, err := os.Open(path); err == nil {
		taken = photoTaken(f, size, typ, g.Location)
		f.Close()
	}

	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	if g.vips != "" {
		out := path + ".jpg"
		defer os.Remove(out)
		s := strconv.Itoa(Size)
		_, err := run(ctx, g.vips, "thumbnail", path, fmt.Sprintf("%s[Q=%d,strip]", out, quality), s,
			"--height", s, "--crop", "centre", "--export-profile", "srgb")
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(out)
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("%w: vips wrote no output", ErrNoThumbnail)
		}
		return &Result{JPEG: b, Taken: taken}, nil
	}
	out, err := run(ctx, g.ffmpeg, ffmpegArgs("file", demuxer, path)...)
	if err != nil {
		return nil, err
	}
	return &Result{JPEG: out.stdout, Taken: taken}, nil
}

// PreviewSize 是大图预览最长边的像素（手机屏幕 3 倍图约 1170×2532，1600 看着清楚，每张二三百 KB）。
const PreviewSize = 1600

// SupportsPreview 按 MIME 类型判断能不能做大图预览（只做照片）。
func (g *Generator) SupportsPreview(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/") && g.Supported(mimeType)
}

// Preview 把照片缩小成最长边不超过 PreviewSize 的 JPEG（按 EXIF 方向转正、不裁剪、不放大），大图查看时先显示它：
// 比原图小得多（HEIC、PNG 原图常常好几 MB 到几十 MB），而且 HEIC 在不支持的浏览器里也能看。不是照片返回 ErrNoThumbnail。
func (g *Generator) Preview(ctx context.Context, src Source) ([]byte, error) {
	rd, err := src(ctx)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	head := make([]byte, 64)
	n, err := io.ReadFull(rd, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	ext, typ := media.Sniff(head[:n])
	demuxer := ffmpegImage[typ]
	if !strings.HasPrefix(typ, "image/") || g.vips == "" && (g.ffmpeg == "" || demuxer == "") {
		return nil, ErrNoThumbnail
	}
	path, _, err := g.copyImage(rd, ext)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	s := strconv.Itoa(PreviewSize)
	if g.vips != "" {
		out := path + ".jpg"
		defer os.Remove(out)
		if _, err := run(ctx, g.vips, "thumbnail", path, out+"[Q=82,strip]", s, "--height", s, "--size", "down",
			"--export-profile", "srgb"); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(out)
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("%w: vips wrote no output", ErrNoThumbnail)
		}
		return b, nil
	}
	out, err := run(ctx, g.ffmpeg, "-nostdin", "-v", "error", "-protocol_whitelist", "file", "-f", demuxer, "-i", path,
		"-frames:v", "1", "-vf", fmt.Sprintf("scale=w='min(%s,iw)':h='min(%s,ih)':force_original_aspect_ratio=decrease", s, s),
		"-an", "-sn", "-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1")
	if err != nil {
		return nil, err
	}
	return out.stdout, nil
}

// ffmpegArgs：从 input 截第一帧，缩放并居中裁剪成 Size×Size 的 JPEG，写到标准输出。
func ffmpegArgs(protocols, demuxer, input string) []string {
	s := strconv.Itoa(Size)
	return []string{
		"-nostdin", "-hide_banner", "-v", "info",
		"-protocol_whitelist", protocols, "-f", demuxer, "-i", input,
		"-frames:v", "1", "-an", "-sn",
		"-vf", "scale=" + s + ":" + s + ":force_original_aspect_ratio=increase,crop=" + s + ":" + s,
		"-q:v", "4", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1",
	}
}

var durationRe = regexp.MustCompile(`Duration: (\d+):(\d\d):(\d\d(?:\.\d+)?)`)

// video：ffmpeg 通过本机的临时 HTTP 地址读视频（支持 Range），只拉取需要的部分（索引和第一帧），
// 不用把整个视频下载下来。
func (g *Generator) video(ctx context.Context, src Source, typ string) (*Result, error) {
	if g.ffmpeg == "" {
		return nil, ErrNoThumbnail
	}
	demuxer := "mov" // mov 解封装器也处理 MP4、M4V、3GP
	if typ == "video/webm" {
		demuxer = "matroska"
	}
	ctx, cancel := context.WithTimeout(ctx, videoTimeout)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	path := "/" + hex.EncodeToString(tok)
	var readErr firstError
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			rd, err := src(ctx)
			if err != nil {
				readErr.set(err)
				http.Error(w, "open failed", http.StatusInternalServerError)
				return
			}
			defer rd.Close()
			http.ServeContent(w, r, "", time.Time{}, &errReader{rd, &readErr})
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	out, err := run(ctx, g.ffmpeg, ffmpegArgs("http,tcp", demuxer, "http://"+ln.Addr().String()+path)...)
	if err != nil {
		if rerr := readErr.get(); rerr != nil && ctx.Err() == nil {
			return nil, rerr // 读存储端出错，不是文件本身的问题
		}
		return nil, err
	}
	res := &Result{JPEG: out.stdout, Taken: videoTaken(out.stderr)}
	if m := durationRe.FindStringSubmatch(out.stderr); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		sec, _ := strconv.ParseFloat(m[3], 64)
		res.Duration = time.Duration((float64(h*3600+min*60) + sec) * float64(time.Second))
	}
	return res, nil
}

type output struct {
	stdout []byte
	stderr string
}

// run 运行外部程序。程序失败（文件解不开）返回 ErrNoThumbnail；超时、取消返回 ctx 的错误。
func run(ctx context.Context, name string, args ...string) (*output, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	base := name[strings.LastIndexAny(name, `/\`)+1:]
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v: %s", ErrNoThumbnail, base, err, lastLine(stderr.String()))
	}
	if strings.Contains(strings.Join(args, " "), "pipe:1") && stdout.Len() == 0 {
		return nil, fmt.Errorf("%w: %s produced no image: %s", ErrNoThumbnail, base, lastLine(stderr.String()))
	}
	return &output{stdout: stdout.Bytes(), stderr: stderr.String()}, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// tailBuffer 只保留最后 max 字节（ffmpeg 的日志可能很长；时长在开头，错误在结尾，开头另外保留一段）。
type tailBuffer struct {
	max  int
	head []byte
	buf  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(b.head) < b.max {
		n := min(len(p), b.max-len(b.head))
		b.head = append(b.head, p[:n]...)
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	if len(b.head) < b.max {
		return string(b.head) // 全部内容都在 head 里
	}
	return string(b.head) + "\n…\n" + string(b.buf)
}

// firstError 记录第一个读文件的错误。
type firstError struct {
	mu  sync.Mutex
	err error
}

func (e *firstError) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *firstError) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

type errReader struct {
	io.ReadSeeker
	errs *firstError
}

func (r *errReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeeker.Read(p)
	if err != nil && err != io.EOF {
		r.errs.set(err)
	}
	return n, err
}
