package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 相册自动同步接口，给 iOS 快捷指令这类只能逐个发请求的客户端用。
// key 由客户端生成（如“拍摄时间_照片名”），作为根目录“相册”文件夹里的主文件名；
// 已有同一主文件名的文件（含回收站）就视为已同步。
const albumFolder = "相册"

// GET /api/album/check?key=... → 纯文本 "exists" 或 "missing"，方便快捷指令直接判断。
func (s *Server) albumCheck(w http.ResponseWriter, r *http.Request) {
	key := albumKey(albumQuery(r).Get("key"))
	if err := validName(key); err != nil {
		s.log.Warn("album check: invalid key", "key", key)
		writeError(w, http.StatusBadRequest, "invalid key")
		return
	}
	exists, err := s.albumHas(r.Context(), key)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("album check", "key", key, "exists", exists)
	if exists {
		writeText(w, http.StatusOK, "exists")
	} else {
		writeText(w, http.StatusOK, "missing")
	}
}

// POST /api/album/upload?key=...，请求体为照片 / 视频原始内容。扩展名和 MIME 取自
// Content-Type，没有时从文件头识别。已同步过的直接返回 200 "exists"，新文件返回 201。
func (s *Server) albumUpload(w http.ResponseWriter, r *http.Request) {
	key := albumKey(albumQuery(r).Get("key"))
	if err := validName(key); err != nil {
		s.log.Warn("album upload: invalid key", "key", key)
		writeError(w, http.StatusBadRequest, "invalid key")
		return
	}
	album, err := s.albumDir(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if exists, err := s.svc.Store().StemExists(r.Context(), s.userID, album.ID, key); err != nil {
		s.fail(w, err)
		return
	} else if exists {
		s.log.Info("album upload: already synced", "key", key)
		writeText(w, http.StatusOK, "exists")
		return
	}

	body := bufio.NewReaderSize(r.Body, 512)
	head, _ := body.Peek(512) // 不足 512 字节时返回已有内容
	ctype, ext := mediaType(r.Header.Get("Content-Type"), head)
	f, err := s.svc.Upload(r.Context(), service.UploadRequest{
		UserID: s.userID, ParentID: &album.ID, Name: key + ext, Mime: ctype, Body: body,
	})
	if errors.Is(err, meta.ErrConflict) { // 并发上传了同一张
		writeText(w, http.StatusOK, "exists")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("album upload", "name", f.Name, "mime", f.Mime, "size", f.Size)
	writeJSON(w, http.StatusCreated, f)
}

// albumQuery 解析相册接口的网址参数。快捷指令拼出来的网址可能把 & = 转义成 %26 %3D，
// 或夹带换行、空格（粘贴时带进去的），所以先完整解码、去掉所有空白再解析。
// key 和 token 本身只含字母、数字和下划线，这样处理不会误伤。
func albumQuery(r *http.Request) url.Values {
	raw := r.URL.RawQuery
	if dec, err := url.QueryUnescape(raw); err == nil {
		raw = dec
	}
	raw = strings.Map(func(c rune) rune {
		if unicode.IsSpace(c) {
			return -1
		}
		return c
	}, raw)
	q, _ := url.ParseQuery(raw)
	return q
}

var (
	nonKeyRun = regexp.MustCompile(`[^A-Za-z0-9]+`)
	stampRe   = regexp.MustCompile(`^(.*?)(\d{8}_\d{6})(.*)$`)
)

// albumKey 把客户端拼的 key 整理成“拍摄时间_照片名”：非字母数字的字符一律换成 _，
// 并把 yyyyMMdd_HHmmss 时间戳挪到最前面。这样“IMG_1234 + 时间”和“时间 + IMG_1234”
// 两种拼法得到同一个 key，快捷指令也可以把时间放在网址末尾（时间后面带上的隐形字符
// 会截断网址，放在末尾就不会丢掉照片名和令牌）。
func albumKey(raw string) string {
	k := strings.Trim(nonKeyRun.ReplaceAllString(raw, "_"), "_")
	if m := stampRe.FindStringSubmatch(k); m != nil {
		if name := strings.Trim(m[1]+"_"+m[3], "_"); name != "" {
			return m[2] + "_" + name
		}
		return m[2]
	}
	return k
}

var hexRun = regexp.MustCompile(`[0-9a-fA-F]{16,}`)

// redactQuery 用于日志：把长十六进制串（令牌）打码。
func redactQuery(raw string) string {
	return hexRun.ReplaceAllStringFunc(raw, func(m string) string { return "<hex" + strconv.Itoa(len(m)) + ">" })
}

func (s *Server) albumHas(ctx context.Context, key string) (bool, error) {
	album, err := s.svc.Store().FindFolder(ctx, s.userID, nil, albumFolder)
	if errors.Is(err, meta.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.svc.Store().StemExists(ctx, s.userID, album.ID, key)
}

// albumDir 返回根目录的“相册”文件夹，不存在则创建。
func (s *Server) albumDir(ctx context.Context) (*meta.File, error) {
	store := s.svc.Store()
	f, err := store.FindFolder(ctx, s.userID, nil, albumFolder)
	if !errors.Is(err, meta.ErrNotFound) {
		return f, err
	}
	f, err = store.CreateFolder(ctx, s.userID, nil, albumFolder)
	if errors.Is(err, meta.ErrConflict) { // 另一个请求刚建好
		return store.FindFolder(ctx, s.userID, nil, albumFolder)
	}
	return f, err
}

var mediaExt = map[string]string{
	"image/jpeg":      ".jpg",
	"image/heic":      ".heic",
	"image/heif":      ".heif",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"video/quicktime": ".mov",
	"video/mp4":       ".mp4",
}

// mediaType 根据 Content-Type（不可靠时看文件头）给出 MIME 类型和扩展名。
func mediaType(contentType string, head []byte) (ctype, ext string) {
	ctype, _, _ = mime.ParseMediaType(contentType)
	switch ctype {
	case "", "application/octet-stream", "application/x-www-form-urlencoded", "multipart/form-data":
		ctype = sniff(head)
	}
	if e, ok := mediaExt[ctype]; ok {
		return ctype, e
	}
	if exts, _ := mime.ExtensionsByType(ctype); len(exts) > 0 {
		return ctype, exts[0]
	}
	return ctype, ""
}

// sniff 识别 iPhone 常见格式：HEIC / MOV / MP4 用 ISO BMFF 的 ftyp 品牌区分，其余交给标准库。
func sniff(head []byte) string {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch string(head[8:12]) {
		case "heic", "heix", "hevc", "heim", "heis", "mif1", "msf1":
			return "image/heic"
		case "qt  ":
			return "video/quicktime"
		default:
			return "video/mp4"
		}
	}
	ctype, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	return ctype
}

func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, text)
}
