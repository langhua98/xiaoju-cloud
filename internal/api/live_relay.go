package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/relay"
)

// 直播走中转：家里电脑往外送数据只有几 Mbps，实时画面经电脑转一道（被控端 → 电脑 → 看的人）要占两份上传，
// 电脑关机、睡着时也看不了。中转（Hugging Face Space，见 internal/relay）上开一个一样的直播通道，
// 画面和声音只在中转的内存里转一下，不经过家里电脑。
//
// 中转不连数据库，不认识 agent 令牌，所以由家里电脑发「通行证」：
//
//	GET /api/agent/v1/stream/relay?room=<房间名>&role=pub|sub   （家里电脑，agent 令牌，pub 要 upload、sub 要 read）
//	  → {"url": "wss://<中转>/live?room=..&role=..&exp=..&sig=.."}   5 分钟内连上有效
//	  → 404 relay_unavailable：没配置中转，或者中转现在不在线（这时照旧走家里电脑的直播通道）
//	GET /live?room&role&exp&sig[&ack=1&window=N]   （中转上）：和 /api/agent/v1/stream 一模一样的直播通道
//
// sig 是用管理员令牌算的（relay.Key，两边都有），别人没有电脑发的通行证连不上中转的直播通道。
// 被控端（安卓 0.41+）同时连着电脑和中转两条推流，只往有人看的那条发；看的一端先试 Wi-Fi 直连，再中转，最后电脑。

const liveTicketTTL = 5 * time.Minute

// liveSig：房间名 + 角色 + 过期时间的签名。
func liveSig(key, room, role string, exp int64) string {
	m := hmac.New(sha256.New, []byte("xiaoju live ticket\x00"+key))
	m.Write([]byte(room + "\x00" + role + "\x00" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// liveTicketURL 拼中转上的直播地址（https → wss）。
func liveTicketURL(relayURL, key, room, role string, now time.Time) string {
	exp := now.Add(liveTicketTTL).Unix()
	b := strings.TrimRight(relayURL, "/")
	if strings.HasPrefix(b, "https://") {
		b = "wss://" + strings.TrimPrefix(b, "https://")
	} else if strings.HasPrefix(b, "http://") {
		b = "ws://" + strings.TrimPrefix(b, "http://")
	}
	q := url.Values{"room": {room}, "role": {role}, "exp": {strconv.FormatInt(exp, 10)}, "sig": {liveSig(key, room, role, exp)}}
	return b + "/live?" + q.Encode()
}

// GET /api/agent/v1/stream/relay：发中转直播的通行证。
func (s *Server) streamRelayTicket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	room, role := q.Get("room"), q.Get("role")
	if room == "" || len(room) > streamMaxRoom {
		agentError(w, http.StatusBadRequest, "invalid_room", "room is required (at most 200 bytes)", "")
		return
	}
	if role != "pub" && role != "sub" {
		agentError(w, http.StatusBadRequest, "invalid_role", "role must be pub or sub", "")
		return
	}
	scope := ScopeRead
	if role == "pub" {
		scope = ScopeUpload
	}
	if !s.need(w, r, scope) {
		return
	}
	rs := s.videoRelay
	if rs == nil || !rs.ok.Load() {
		agentError(w, http.StatusNotFound, "relay_unavailable", "relay is not configured or offline",
			"Use /api/agent/v1/stream on this server instead.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"url": liveTicketURL(rs.url, rs.key, room, role, time.Now())})
}

// LiveRelayHandler 是中转上的直播通道 GET /live（中转模式用，见 cmd/xiaoju/relay.go）。
// 只认家里电脑发的通行证；自己有一个单独的房间表，和家里电脑上的不相干。
func LiveRelayHandler(adminToken string) http.Handler {
	key := relay.Key(adminToken)
	hub := newStreamHub()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
		if err != nil || time.Now().Unix() > exp ||
			!hmac.Equal([]byte(q.Get("sig")), []byte(liveSig(key, q.Get("room"), q.Get("role"), exp))) {
			agentError(w, http.StatusForbidden, "invalid_ticket", "invalid or expired live ticket",
				"Ask the home server: GET /api/agent/v1/stream/relay?room=..&role=..")
			return
		}
		name, pub, ack, window, ok := streamParams(w, r)
		if !ok {
			return
		}
		hub.serve(w, r, name, pub, ack, window)
	})
}
