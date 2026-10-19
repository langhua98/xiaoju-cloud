package api

import (
	"encoding/hex"
	"net/http"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// GET /api/duplicates：内容完全一样的文件（整个文件的 SHA-256 相同），一组一组列出来，网页里挑一个留下、其余移进回收站。
//
//	{"groups": [{"sha256", "size", "files": [{文件, "path": "相册/2024", "in_private": true, "thumb"}]}], "extra_bytes": 多占的空间}
func (s *Server) listDuplicates(w http.ResponseWriter, r *http.Request) {
	groups, err := s.svc.Store().ListDuplicates(r.Context(), s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	type item struct {
		meta.Duplicate
		Thumb string `json:"thumb,omitempty"`
	}
	type group struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
		Files  []item `json:"files"`
	}
	out := make([]group, 0, len(groups))
	var extra int64
	for _, g := range groups {
		gr := group{SHA256: hex.EncodeToString(g[0].SHA256), Size: g[0].Size}
		for _, d := range g {
			it := item{Duplicate: d}
			if s.svc.CanThumbnail(d.File) {
				it.Thumb = s.thumbURL(d.ID)
			}
			gr.Files = append(gr.Files, it)
		}
		extra += gr.Size * int64(len(g)-1)
		out = append(out, gr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "extra_bytes": extra})
}
