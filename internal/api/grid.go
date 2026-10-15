package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
)

// 格子口令：看照片、视频时随机抽查（见 mediaGate）。每次抽查两次机会，第一次 15 秒，
// 答错或超时换一张新格子、给 20 秒；第二次也没答对，当前会话立即退出登录。

type gridAnswerReq struct {
	Ticket   string `json:"ticket"`   // 关闭格子口令时：刚刚刷过脸的凭证（注册了刷脸设备时必填）
	ID       string `json:"id"`       // 题目 ID
	Answer   *int   `json:"answer"`   // 答案
	Timeout  bool   `json:"timeout"`  // 页面倒计时已到、没有作答
	Recovery string `json:"recovery"` // 关闭格子口令时：可以用恢复码代替答题
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// POST /api/auth/grid/challenge：给当前会话出一道抽查题（上一道没答完的算用掉一次机会）。
func (s *Server) gridChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	sid, _ := s.sessionID(r)
	ch, err := s.grid.NewCheck(sid)
	if errors.Is(err, auth.ErrGridFailed) {
		s.logoutSession(w, r)
		return
	}
	if err != nil {
		s.gridFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// checkAnswer 校验当前会话的抽查答案，答对返回 true。两次机会都没答对时让当前会话退出登录。
func (s *Server) checkAnswer(w http.ResponseWriter, r *http.Request, req gridAnswerReq) bool {
	if req.Answer == nil && !req.Timeout {
		writeError(w, http.StatusBadRequest, "answer required")
		return false
	}
	answer := 0
	if req.Answer != nil {
		answer = *req.Answer
	}
	sid, _ := s.sessionID(r)
	err := s.grid.Check(sid, req.ID, answer, req.Timeout)
	if errors.Is(err, auth.ErrGridFailed) {
		s.logoutSession(w, r)
		return false
	}
	if err != nil {
		s.gridFail(w, err)
		return false
	}
	return true
}

// logoutSession 两次机会都没答对：作废当前会话（服务重启后也不会恢复）。
func (s *Server) logoutSession(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Revoke(r.Context(), r.Header.Get("X-Xiaoju-Session")); err != nil {
		s.log.Error("revoke session", "err", err)
	}
	s.log.Warn("grid passcode failed twice: session logged out")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "grid passcode failed twice, logged out", "code": "grid_logout"})
}

// POST /api/auth/grid/verify {id, answer | timeout}：答对后可以继续看，重新随机 3–7 张。
func (s *Server) gridVerify(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req gridAnswerReq
	if !decodeBody(w, r, &req) || !s.checkAnswer(w, r, req) {
		return
	}
	sid, _ := s.sessionID(r)
	s.views.Reset(sid)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/auth/recovery/passkey {code}：刷脸设备丢了时，用恢复码代替刷脸登录。
func (s *Server) recoveryPasskey(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyRequired() || !s.gridRequired() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recovery codes exist only while the grid passcode is enabled", "code": "grid_disabled"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.grid.UseRecoveryCode(r.Context(), req.Code); err != nil {
		s.gridFail(w, err)
		return
	}
	s.log.Warn("recovery code used instead of face verification")
	s.writeSession(w)
}

// GET /api/auth/grid：是否开启、剩余恢复码、出题用的时区。
func (s *Server) gridStatus(w http.ResponseWriter, r *http.Request) {
	if s.grid == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "enabled": false})
		return
	}
	st, err := s.grid.Status(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "enabled": st.Enabled, "recovery_left": st.RecoveryLeft, "timezone": st.Timezone,
		"passkey_required": s.passkeyRequired(),
		"views_min":        auth.ViewsMin, "views_max": auth.ViewsMax,
		"seconds_first": int(auth.CheckTimeFirst / time.Second), "seconds_second": int(auth.CheckTimeSecond / time.Second),
	})
}

// POST /api/auth/grid/setup/begin：开启前出一道练习题（不限时）。
func (s *Server) gridSetupBegin(w http.ResponseWriter, r *http.Request) {
	if s.grid == nil {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	ch, err := s.grid.NewSetupChallenge()
	if err != nil {
		s.gridFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// POST /api/auth/grid/setup/finish {id, rule, answer}：按新规则答对练习题后开启，返回恢复码（只显示这一次）。
func (s *Server) gridSetupFinish(w http.ResponseWriter, r *http.Request) {
	if s.grid == nil {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req struct {
		ID     string        `json:"id"`
		Rule   auth.GridRule `json:"rule"`
		Answer *int          `json:"answer"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Answer == nil {
		writeError(w, http.StatusBadRequest, "answer required")
		return
	}
	codes, err := s.grid.Setup(r.Context(), req.ID, req.Rule, *req.Answer)
	if err != nil {
		s.gridFail(w, err)
		return
	}
	s.log.Info("grid passcode enabled")
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// DELETE /api/auth/grid {ticket, id, answer | timeout | recovery}：关闭格子口令。
// 除了会话，还要当场重新刷脸（ticket）并答对一道题（或用恢复码），防止被盗的会话拆掉验证；
// 答题规则和抽查一样，两次都没答对就退出登录。
func (s *Server) gridDisable(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req gridAnswerReq
	if !decodeBody(w, r, &req) {
		return
	}
	if s.passkeyRequired() && !s.sessions.Ticket(req.Ticket) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "face verification required first", "code": "passkey_required"})
		return
	}
	if req.Recovery != "" {
		if err := s.grid.UseRecoveryCode(r.Context(), req.Recovery); err != nil {
			s.gridFail(w, err)
			return
		}
	} else if !s.checkAnswer(w, r, req) {
		return
	}
	if err := s.grid.Disable(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Warn("grid passcode disabled")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) gridFail(w http.ResponseWriter, err error) {
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		s.log.Warn("recovery codes locked after repeated wrong attempts", "until", locked.Until)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too many wrong recovery codes", "code": "locked",
			"retry_after": int(math.Ceil(time.Until(locked.Until).Seconds())),
		})
	case errors.Is(err, auth.ErrGridWrong):
		s.log.Warn("wrong grid passcode answer")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "wrong_answer"})
	case errors.Is(err, auth.ErrGridTimeout):
		s.log.Warn("grid passcode answer timed out")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "timeout"})
	case errors.Is(err, auth.ErrRecoveryCode):
		s.log.Warn("invalid recovery code")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "recovery_invalid"})
	case errors.Is(err, auth.ErrGridChallenge):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "challenge_expired"})
	case errors.Is(err, auth.ErrGridRule):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_rule"})
	case errors.Is(err, auth.ErrGridDisabled):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "grid_disabled"})
	case errors.Is(err, auth.ErrGridEnabled):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "grid_enabled"})
	default:
		s.fail(w, err)
	}
}
