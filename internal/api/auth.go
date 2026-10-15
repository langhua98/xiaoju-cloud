package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
)

const maxCeremonyBody = 64 << 10

func (s *Server) tokenOK(r *http.Request) bool {
	// 私有 Hugging Face Space 的 Authorization 头被 HF 自己的 token 占用，所以也接受 X-Xiaoju-Token。
	got := r.Header.Get("X-Xiaoju-Token")
	if got == "" {
		got, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) sessionOK(r *http.Request) bool {
	t := r.Header.Get("X-Xiaoju-Session")
	return t != "" && s.sessions.Valid(t)
}

// passkeyRequired：已经注册过 Face ID / 指纹设备后，仅凭令牌不能访问文件。
func (s *Server) passkeyRequired() bool { return s.passkeys != nil && s.passkeys.Required() }

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case s.sessionOK(r), s.tokenOK(r) && !s.passkeyRequired():
			next.ServeHTTP(w, r)
		case s.tokenOK(r):
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "face verification required", "code": "passkey_required"})
		default:
			writeError(w, http.StatusUnauthorized, "unauthorized")
		}
	})
}

func (s *Server) requireTokenOrSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessionOK(r) && !s.tokenOK(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeSession(w http.ResponseWriter) {
	tok, exp := s.sessions.Issue()
	writeJSON(w, http.StatusOK, map[string]any{"session": tok, "expires_at": exp})
}

// POST /api/auth/session：令牌正确后的第一步。还没注册设备时直接签发会话；
// 已注册时返回 passkey_required，前端接着走刷脸。
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	available := s.passkeys != nil && s.passkeys.Available()
	if s.passkeyRequired() {
		writeJSON(w, http.StatusOK, map[string]any{"passkey_required": true, "passkeys_available": available})
		return
	}
	tok, exp := s.sessions.Issue()
	writeJSON(w, http.StatusOK, map[string]any{
		"passkey_required": false, "passkeys_available": available, "session": tok, "expires_at": exp,
	})
}

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	id, opts, err := s.passkeys.BeginLogin(r.Context())
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	err := s.passkeys.FinishLogin(r.Context(), r.URL.Query().Get("ceremony"), http.MaxBytesReader(w, r.Body, maxCeremonyBody))
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	s.writeSession(w)
}

type registerBeginReq struct {
	Name string `json:"name"`
	Code string `json:"code"` // 添加码（已有设备时，从没登录的新设备上注册需要）
}

// POST /api/auth/passkey/register/begin：第一台设备只需令牌；之后需要已登录的会话，或另一台设备生成的添加码。
func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	var req registerBeginReq
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	if s.passkeyRequired() && !s.sessionOK(r) && !s.passkeys.UseAddCode(req.Code) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": auth.ErrNotAllowed.Error(), "code": "add_code_required"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "未命名设备"
	}
	if utf8.RuneCountInString(name) > 60 {
		name = string([]rune(name)[:60])
	}
	id, opts, err := s.passkeys.BeginRegistration(r.Context(), name)
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	err := s.passkeys.FinishRegistration(r.Context(), r.URL.Query().Get("ceremony"), http.MaxBytesReader(w, r.Body, maxCeremonyBody))
	if err != nil {
		s.passkeyFail(w, err)
		return
	}
	s.log.Info("passkey registered")
	s.writeSession(w)
}

func (s *Server) listPasskeys(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "items": []auth.Device{}})
		return
	}
	items, err := s.passkeys.List(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": s.passkeys.Available(), "items": items})
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	if err := s.passkeys.Delete(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("passkey deleted")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) newAddCode(w http.ResponseWriter, r *http.Request) {
	if s.passkeys == nil || !s.passkeys.Available() {
		s.passkeyFail(w, auth.ErrUnavailable)
		return
	}
	code, exp, err := s.passkeys.NewAddCode()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": exp})
}

func (s *Server) passkeyFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnavailable):
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error(), "code": "passkeys_unavailable"})
	case errors.Is(err, auth.ErrCeremony):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "ceremony_expired"})
	case errors.Is(err, auth.ErrVerification):
		s.log.Warn("passkey verification failed", "err", err)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": auth.ErrVerification.Error(), "code": "verification_failed"})
	default:
		s.fail(w, err)
	}
}
