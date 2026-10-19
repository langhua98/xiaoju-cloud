package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 断点续传（网页传大文件用，见 service/resumable.go）：
//
//	POST /api/uploads {"name", "parent_id", "size", "mime", "probe", "dedupe"}
//	     → 201 {"id", "size", "offset": 0, "chunk"}；同一个文件夹里有同名同大小、probe 一样的没传完的，
//	       返回 200 和它的进度（刷新页面后重新选了同一个文件，接着传）
//	PUT  /api/uploads/{id}?offset=N，请求体是文件里从 N 开始的一段（建议 chunk 字节）
//	     → 200 {"offset", ...}；传完时 201 {"offset": size, "file": {...}}；
//	       N 和服务端收到的对不上 409 {"offset": 实际收到的}；dedupe 时网盘里已有一样的 200 {"same_content": true, "file"}
//	GET  /api/uploads/{id} → {"offset", ...}；传完了（或被清理了）是 404
//
// 24 小时没收到新数据的上传会被清理。

// uploadIdle：这么久没收到数据就不等了（手机网络断了，服务端可能很久才知道），收到的都算数。
var uploadIdle = 30 * time.Second

func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string     `json:"name"`
		ParentID *uuid.UUID `json:"parent_id"`
		Size     int64      `json:"size"`
		Mime     string     `json:"mime"`
		Probe    string     `json:"probe"`
		Dedupe   bool       `json:"dedupe"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := validName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Mime == "" || req.Mime == "application/octet-stream" {
		req.Mime = "application/octet-stream"
		if guess := mime.TypeByExtension(filepath.Ext(req.Name)); guess != "" {
			req.Mime = guess
		}
	}
	p, err := s.svc.StartUpload(r.Context(), service.ResumableRequest{
		UserID: s.userID, ParentID: req.ParentID, Name: req.Name, Mime: req.Mime,
		Size: req.Size, Probe: req.Probe, SkipSameContent: req.Dedupe,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusOK
	if p.Offset == 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := s.svc.UploadStatus(r.Context(), s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) writeUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a byte position")
		return
	}
	rc := http.NewResponseController(w)
	body := &idleReader{r: r.Body, rc: rc, idle: uploadIdle}
	p, err := s.svc.WriteUpload(r.Context(), s.userID, id, offset, body, body.stop)
	var off *service.OffsetError
	var same *service.SameContentError
	switch {
	case errors.As(err, &off):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset mismatch", "offset": off.Offset})
	case errors.As(err, &same):
		writeJSON(w, http.StatusOK, map[string]any{"same_content": true, "file": same.File})
	case err != nil:
		s.fail(w, err)
	case p.File != nil:
		writeJSON(w, http.StatusCreated, p)
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

// idleReader 每次读之前把读超时往后推 idle：一直有数据就一直读，停了 idle 这么久就报错（收到的都算数）。
// stop 让它马上停下（同一个上传的新请求接手时）。
type idleReader struct {
	r       io.Reader
	rc      *http.ResponseController
	idle    time.Duration
	stopped atomic.Bool
}

var errTakenOver = errors.New("a newer request took over this upload")

func (b *idleReader) Read(p []byte) (int, error) {
	if b.stopped.Load() {
		return 0, errTakenOver
	}
	b.rc.SetReadDeadline(time.Now().Add(b.idle))
	if b.stopped.Load() { // stop 恰好发生在上面两行之间
		return 0, errTakenOver
	}
	return b.r.Read(p)
}

func (b *idleReader) stop() {
	b.stopped.Store(true)
	b.rc.SetReadDeadline(time.Now()) // 正在等数据的那次 Read 马上返回
}
