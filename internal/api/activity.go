package api

import (
	"net/http"
	"sync/atomic"
	"time"
)

// activity 记着正在进行的上传，和最后一次上传活动的时间。部署前先问一下（GET /api/maintenance/activity，
// 见 deploy/huggingface/deploy.py），等上传停了几分钟再重启服务端，免得打断快捷指令正在传的文件。
type activity struct {
	active atomic.Int64
	last   atomic.Int64 // Unix 秒；0 表示服务启动后还没有过
}

// tracked 把一个上传类的请求算进 activity。
func (s *Server) tracked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.act.active.Add(1)
		s.act.last.Store(time.Now().Unix())
		defer func() {
			s.act.active.Add(-1)
			s.act.last.Store(time.Now().Unix())
		}()
		h(w, r)
	}
}

// GET /api/maintenance/activity → {"active": 正在进行的上传数, "last_activity": 时间, "idle_seconds": 多久没有上传了}
func (s *Server) activityStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"active": s.act.active.Load()}
	if last := s.act.last.Load(); last > 0 {
		out["last_activity"] = time.Unix(last, 0).UTC()
		out["idle_seconds"] = time.Now().Unix() - last
	} else {
		out["idle_seconds"] = int64(time.Since(s.started).Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}
