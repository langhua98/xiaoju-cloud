package api

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
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
