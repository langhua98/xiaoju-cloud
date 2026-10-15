package auth

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"
)

// 看照片、视频时的格子口令抽查：每个会话每看随机 ViewsMin–ViewsMax 张，下一张之前要先答对一道格子题。
// 张数在服务端随机，页面上看不到；服务重启后重新随机。
const (
	ViewsMin = 3
	ViewsMax = 7
)

type Views struct {
	now func() time.Time

	mu sync.Mutex
	m  map[string]*viewQuota // 会话 ID → 额度
}

type viewQuota struct {
	used, limit int
	seen        time.Time
}

func NewViews() *Views { return &Views{now: time.Now, m: map[string]*viewQuota{}} }

func randomLimit() int {
	n, err := rand.Int(rand.Reader, big.NewInt(ViewsMax-ViewsMin+1))
	if err != nil {
		return ViewsMin // 随机数不可用时取最严格的
	}
	return ViewsMin + int(n.Int64())
}

// Allow 记一次查看；额度用完时返回 false（要先答格子口令）。
func (v *Views) Allow(sessionID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	q := v.m[sessionID]
	if q == nil {
		v.prune(now)
		q = &viewQuota{limit: randomLimit()}
		v.m[sessionID] = q
	}
	q.seen = now
	if q.used >= q.limit {
		return false
	}
	q.used++
	return true
}

// Reset 答对格子口令后重新随机额度。
func (v *Views) Reset(sessionID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[sessionID] = &viewQuota{limit: randomLimit(), seen: v.now()}
}

// prune 清掉会话早已过期的记录。调用方持锁。
func (v *Views) prune(now time.Time) {
	for id, q := range v.m {
		if now.Sub(q.seen) > SessionTTL {
			delete(v.m, id)
		}
	}
}
