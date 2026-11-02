package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// maxKnownSums 是一次最多问多少个摘要（一次选几千张照片也够）。
const maxKnownSums = 5000

// POST /api/files/known：网页上传照片前先问一下，哪些内容网盘里已经有了（按整个文件明文的 SHA-256）。
//
//	{"sha256": ["十六进制", ...], "parent_id": "要传进的文件夹"}
//	回答：{"known": {"十六进制": 那个文件}}，没有的不出现
//
// 规则和上传时按内容去重一样（见 meta.SameContent）：传进私密文件夹时，只认私密文件夹里的那份。
func (s *Server) knownContent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SHA256   []string   `json:"sha256"`
		ParentID *uuid.UUID `json:"parent_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(req.SHA256) > maxKnownSums {
		writeError(w, http.StatusBadRequest, "too many sums")
		return
	}
	sums := make([][]byte, 0, len(req.SHA256))
	for _, h := range req.SHA256 {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			writeError(w, http.StatusBadRequest, "sha256 must be 64 hex digits")
			return
		}
		sums = append(sums, b)
	}
	store := s.svc.Store()
	private, err := store.InPrivateFolder(r.Context(), s.userID, req.ParentID)
	if err != nil {
		s.fail(w, err)
		return
	}
	found, err := store.SameContent(r.Context(), s.userID, sums, private, req.ParentID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"known": found})
}

// POST /api/ingest/known：App 按指纹（照片、视频原件整个文件的 SHA-256）问网盘里有没有，一次一批。
// 按名字 + 拍摄日期认出来的只是“像”：同名的不一定是同一张（换过手机编号重来、微信存的图名字一样），
// 所以 App 把这些照片的原件算一遍指纹再来问，对得上才算传过，对不上的就传原件。
//
//	{"album": ""（相册）或 "hidden"（隐藏相册）, "sha256": "十六进制\n十六进制…"}
//	回答：{"found": "1\n3"（有的是第几个，从 1 开始）, "count": 2, "files": 3}
//
// 规则和上传时按内容去重一样（见 meta.SameContent）：对得上的那个文件上传时也会被当成已有、不再存。
// 和 exists 一样只透露“这个内容在不在”，不能列出网盘里有什么。
func (s *Server) ingestKnown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Album  string `json:"album"`
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "known request must be JSON with sha256")
		return
	}
	album := strings.ToLower(strings.TrimSpace(req.Album))
	folder, ok := ingestAlbums[album]
	if !ok || album == "data" {
		writeError(w, http.StatusBadRequest, "unknown album")
		return
	}
	lines := splitLines(req.SHA256)
	if len(lines) > maxKnownSums {
		writeError(w, http.StatusBadRequest, "too many sums")
		return
	}
	sums := make([][]byte, 0, len(lines))
	for _, h := range lines {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != 32 {
			writeError(w, http.StatusBadRequest, "sha256 must be 64 hex digits")
			return
		}
		sums = append(sums, b)
	}
	store := s.svc.Store()
	// 上传进哪个相册就按哪个相册的规则：隐藏相册（私密）只认私密文件夹里的那份
	var parent *uuid.UUID
	private := album == "hidden"
	switch f, err := store.FindFolder(r.Context(), s.userID, nil, folder); {
	case errors.Is(err, meta.ErrNotFound):
	case err != nil:
		s.fail(w, err)
		return
	default:
		parent = &f.ID
		if private, err = store.InPrivateFolder(r.Context(), s.userID, parent); err != nil {
			s.fail(w, err)
			return
		}
	}
	have, err := store.SameContent(r.Context(), s.userID, sums, private, parent)
	if err != nil {
		s.fail(w, err)
		return
	}
	var found []string
	for i, b := range sums {
		if have[hex.EncodeToString(b)] != nil {
			found = append(found, strconv.Itoa(i+1))
		}
	}
	s.log.Info("ingest known", "folder", folder, "files", len(sums), "found", len(found), "client", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"found": strings.Join(found, "\n"), "count": len(found), "files": len(sums)})
}
