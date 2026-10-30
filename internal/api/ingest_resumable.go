package api

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/langhua98/xiaoju-cloud/internal/media"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// POST /api/ingest/uploads：App 用的断点续传上传（和网页的 /api/uploads 一样分段传，但不需要认证，只能往
// “相册”这几个文件夹里添加，同 POST /api/ingest）。
//
//	头部 X-File-Name、X-Xiaoju-Album、X-Xiaoju-Album-Name：同 POST /api/ingest
//	头部 X-File-Size：文件字节数
//	头部 X-Upload-Probe（可选）：文件开头、结尾各 1MB 的 SHA-256，同名同大小的旧上传只有对上了才接着传
//
// 已经传过（同名同大小）返回 200 {"result":"skipped"}；否则返回进度 {"id","offset","chunk","missing","lanes","part"}：
// 从 offset 开始一段段 PUT /api/ingest/uploads/{id}?offset=…，断了 GET /api/ingest/uploads/{id} 看收到多少再接着传。
// 也可以开 lanes 条连接，各从 missing 里挑一段同时传（…?offset=起点&parallel=1，见 uploads.go）。
// 同一个文件再来一次（名字、大小、probe 都一样）接着上次的进度。同名不同大小的已有文件：自动改名“名字 (2)”。
func (s *Server) startIngestUpload(w http.ResponseWriter, r *http.Request) {
	name, ok := ingestName(r, "")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file name (X-File-Name)", "code": "missing_name"})
		return
	}
	size := parseByteCount(r.Header.Get("X-File-Size"))
	if size <= 0 {
		writeError(w, http.StatusBadRequest, "X-File-Size must be the file size in bytes")
		return
	}
	folder, ok := ingestAlbums[strings.ToLower(strings.TrimSpace(r.Header.Get("X-Xiaoju-Album")))]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown album")
		return
	}
	album, err := s.albumFolder(r, folder)
	if err != nil {
		s.fail(w, err)
		return
	}
	sub, ok := ingestSubAlbum(r, folder)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad album name")
		return
	}
	if sub != "" {
		if album, err = s.subAlbumFolder(r, album, sub); err != nil {
			s.fail(w, err)
			return
		}
		folder += "/" + sub
	}
	if f, err := s.uploadedBefore(r.Context(), album.ID, name, size); err != nil {
		s.fail(w, err)
		return
	} else if f != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": "skipped", "name": f.Name})
		return
	}
	mimeType := media.ByExt(filepath.Ext(name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	for i := 1; i < 100; i++ {
		n := name
		if i > 1 {
			n = withCopySuffix(name, i)
		}
		p, err := s.svc.StartUpload(r.Context(), service.ResumableRequest{
			UserID: s.userID, ParentID: &album.ID, Name: n, Mime: mimeType, Size: size,
			Probe: r.Header.Get("X-Upload-Probe"), SkipSameContent: sub == "",
		})
		if errors.Is(err, meta.ErrConflict) { // 同名的已有文件（大小不同）：换个名字
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		s.log.Info("ingest upload started", "folder", folder, "name", n, "size", size, "offset", p.Offset,
			"client", clientIP(r), "agent", r.UserAgent())
		writeJSON(w, http.StatusOK, map[string]any{"result": "started", "id": p.ID, "size": p.Size, "offset": p.Offset, "chunk": p.Chunk,
			"missing": p.Missing, "lanes": p.Lanes, "part": p.Part})
		return
	}
	writeError(w, http.StatusConflict, "too many files with this name")
}
