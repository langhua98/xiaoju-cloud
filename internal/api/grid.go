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

// 格子口令：登录的第三步（令牌 → 刷脸 → 格子口令）。

type gridAnswerReq struct {
	Ticket   string `json:"ticket"`   // 刷脸后拿到的过渡票据（注册了刷脸设备时必填）
	ID       string `json:"id"`       // 题目 ID
	Answer   *int   `json:"answer"`   // 答案
	Recovery string `json:"recovery"` // 或者用恢复码代替这一步
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// passkeyStep 检查这次登录是否已经过了刷脸这一步（没有注册刷脸设备时不需要）。
func (s *Server) passkeyStep(ticket string) (ok, recovered bool) {
	if !s.passkeyRequired() {
		return true, false
	}
	return s.sessions.Ticket(ticket)
}

func writePasskeyFirst(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "face verification required first", "code": "passkey_required"})
}

// POST /api/auth/grid/challenge {ticket}：出一道登录题，之前没答的题作废。
func (s *Server) gridChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if ok, _ := s.passkeyStep(req.Ticket); !ok {
		writePasskeyFirst(w)
		return
	}
	ch, err := s.grid.NewChallenge("login")
	if err != nil {
		s.gridFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// checkGridAnswer 校验登录题答案或恢复码。一次登录最多用一个恢复码（刷脸已用恢复码代替时，这一步不能再用）。
func (s *Server) checkGridAnswer(w http.ResponseWriter, r *http.Request, req gridAnswerReq) bool {
	ok, recovered := s.passkeyStep(req.Ticket)
	if !ok {
		writePasskeyFirst(w)
		return false
	}
	var err error
	switch {
	case req.Recovery != "":
		if recovered {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only one recovery code per login", "code": "recovery_used"})
			return false
		}
		if err = s.grid.UseRecoveryCode(r.Context(), req.Recovery); err == nil {
			s.log.Warn("recovery code used instead of the grid passcode")
		}
	case req.Answer != nil:
		err = s.grid.Verify(req.ID, *req.Answer)
	default:
		writeError(w, http.StatusBadRequest, "answer or recovery code required")
		return false
	}
	if err != nil {
		s.gridFail(w, err)
		return false
	}
	return true
}

// POST /api/auth/grid/verify {ticket, id, answer | recovery}：答对后签发会话。
func (s *Server) gridVerify(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req gridAnswerReq
	if !decodeBody(w, r, &req) || !s.checkGridAnswer(w, r, req) {
		return
	}
	s.writeSession(w)
}

// POST /api/auth/recovery/passkey {code}：刷脸设备丢了时，用恢复码代替刷脸，之后仍要答格子口令。
func (s *Server) recoveryPasskey(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyRequired() || !s.gridRequired() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recovery codes can replace face verification only when both steps are enabled", "code": "grid_disabled"})
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
	writeJSON(w, http.StatusOK, map[string]any{"next": "grid", "ticket": s.sessions.IssueTicket(true)})
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
	})
}

// POST /api/auth/grid/setup/begin：开启前出一道练习题。
func (s *Server) gridSetupBegin(w http.ResponseWriter, r *http.Request) {
	if s.grid == nil {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	ch, err := s.grid.NewChallenge("setup")
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

// DELETE /api/auth/grid {ticket, id, answer | recovery}：关闭格子口令。
// 除了会话，还要当场重新刷脸（ticket）并答对一道题，防止被盗的会话拆掉验证。
func (s *Server) gridDisable(w http.ResponseWriter, r *http.Request) {
	if !s.gridRequired() {
		s.gridFail(w, auth.ErrGridDisabled)
		return
	}
	var req gridAnswerReq
	if !decodeBody(w, r, &req) || !s.checkGridAnswer(w, r, req) {
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
		s.log.Warn("grid passcode locked after repeated wrong answers", "until", locked.Until)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too many wrong answers", "code": "locked",
			"retry_after": int(math.Ceil(time.Until(locked.Until).Seconds())),
		})
	case errors.Is(err, auth.ErrGridWrong):
		s.log.Warn("wrong grid passcode answer")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "wrong_answer"})
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
