package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 软件包更新提醒：有人往云盘根目录的「软件包」文件夹传了安装包，马上告诉正在待命的被控端（AI 令牌可能只看得到某个文件夹，所以「软件包」在哪一层都算），让它去查、去装，
// 不用等它自己定时来查（安卓被控端以前 6 小时才查一次）。
//
// 只是提醒：消息里没有配对口令，被控端收到后自己列 /软件包/、下载，装之前还要核对包名、版本和签名，
// 所以别人往这里塞一个包也装不上。只发给 GET 信箱时带了 &updates=<种类> 的信箱（iPhone 被控端不带，收不到）。

const packageFolder = "软件包"

// packageKind 按文件名看是哪种安装包，不是安装包返回空。
func packageKind(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".apk") {
		return "android"
	}
	return ""
}

// packageUploaded 是文件传完后的回调（service.SetOnUploaded）。
func (s *Server) packageUploaded(f *meta.File) {
	if f == nil || f.Type != meta.TypeFile || f.ParentID == nil || f.UserID != s.userID {
		return
	}
	kind := packageKind(f.Name)
	if kind == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent, err := s.svc.Store().Get(ctx, s.userID, *f.ParentID)
	if err != nil || parent.Name != packageFolder {
		return
	}
	body, _ := json.Marshal(map[string]any{"do": "update_hint", "name": f.Name, "item_id": f.ID, "size": f.Size})
	n := s.relay.hint(kind, body, time.Now())
	if s.log != nil {
		s.log.Info("package update hint", "name", f.Name, "boxes", n)
	}
}
