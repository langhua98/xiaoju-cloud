package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 格子口令：看照片、视频时随机抽查（见 Views）。页面出一张随机 4×4 数字格，主人按自己设定的规则心算出答案：
//
//	答案 = 格[乘数格] × 星期 + 格[加数格]，周一再加一个固定数；周日算 7。
//
// 星期按出题那一刻（配置的时区）计算并随题目保存，跨零点作答不受影响。
// 规则只存在数据库里（用主密钥加密），代码里只有通用的计算方式。
//
// 每次抽查有两次机会：第一次限时 CheckTimeFirst，答错或超时换一张新格子、限时再加 5 秒；
// 第二次也没答对就返回 ErrGridFailed，调用方要让这个会话退出登录。
// 题一旦出了就必须在时限内作答：不答、关掉弹窗或重新出题，都算这次机会用掉了。
const (
	GridCells         = 16
	CheckTimeFirst    = 15 * time.Second
	CheckTimeSecond   = CheckTimeFirst + 5*time.Second
	checkAttempts     = 2
	checkGrace        = 2 * time.Second // 网络延迟的余量
	setupChallengeTTL = 3 * time.Minute // 开启时的练习题不限时，只是会过期
	recoveryMaxFails  = 3               // 恢复码连续输错这么多次后锁定
	recoveryLockout   = time.Hour
	recoveryCodeCount = 10
)

var (
	ErrGridDisabled  = errors.New("grid passcode is not enabled")
	ErrGridEnabled   = errors.New("grid passcode is already enabled; turn it off first")
	ErrGridChallenge = errors.New("this grid has expired or was already answered")
	ErrGridWrong     = errors.New("wrong answer, one more chance")
	ErrGridTimeout   = errors.New("time is up, one more chance")
	ErrGridFailed    = errors.New("both chances used up")
	ErrGridRule      = errors.New("invalid rule: pick two different cells (0-15) and a Monday bonus of 0-99")
	ErrRecoveryCode  = errors.New("recovery code is wrong or already used")
)

// LockedError 表示恢复码连续输错太多次，在 Until 之前不再接受。
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many wrong recovery codes, try again after %s", e.Until.Format(time.RFC3339))
}

// GridRule 是主人设定的规则。格子按行编号：第 r 行第 c 列 = (r-1)*4 + (c-1)。
type GridRule struct {
	Mul         int `json:"mul"`          // 乘数格
	Add         int `json:"add"`          // 加数格
	MondayBonus int `json:"monday_bonus"` // 周一额外加的数
}

func (r GridRule) Validate() error {
	if r.Mul < 0 || r.Mul >= GridCells || r.Add < 0 || r.Add >= GridCells || r.Mul == r.Add ||
		r.MondayBonus < 0 || r.MondayBonus > 99 {
		return ErrGridRule
	}
	return nil
}

// Answer 按规则算出答案。weekday 是 1（周一）到 7（周日）。
func (r GridRule) Answer(cells [GridCells]int, weekday int) int {
	n := cells[r.Mul]*weekday + cells[r.Add]
	if weekday == 1 {
		n += r.MondayBonus
	}
	return n
}

// Weekday 返回 t 是星期几：周一 1 … 周日 7。
func Weekday(t time.Time) int {
	if w := int(t.Weekday()); w != 0 {
		return w
	}
	return 7
}

// GridStore 是格子口令需要的持久化操作（*meta.Store 实现）。
type GridStore interface {
	GetGridRule(ctx context.Context) ([]byte, error)
	SetGridRule(ctx context.Context, wrapped []byte, codeHashes [][]byte) error
	DeleteGridRule(ctx context.Context) error
	UseRecoveryCode(ctx context.Context, hash []byte) (bool, error)
	CountRecoveryCodes(ctx context.Context) (int, error)
}

// Challenge 是发给页面的一道题。
type Challenge struct {
	ID          string         `json:"id"`
	Grid        [GridCells]int `json:"grid"`
	Weekday     int            `json:"weekday"` // 按出题时间算的星期，1–7
	IssuedAt    time.Time      `json:"issued_at"`
	IssuedLocal string         `json:"issued_local"` // 出题时间（服务端时区），如 2026-10-15 23:58
	Timezone    string         `json:"timezone"`
	ExpiresAt   time.Time      `json:"expires_at"`
	Attempt     int            `json:"attempt,omitempty"` // 抽查：第几次机会（1 或 2）
	Seconds     int            `json:"seconds,omitempty"` // 抽查：限时秒数
}

type gridChallenge struct {
	id       string
	cells    [GridCells]int
	weekday  int
	deadline time.Time
}

// checkRound 是一个会话正在进行的一轮抽查。
type checkRound struct {
	attempt int            // 当前是第几次机会
	cur     *gridChallenge // 出了还没答的题
	started time.Time
}

// Grid 管理格子口令的规则、题目和恢复码。
type Grid struct {
	store  GridStore
	master []byte
	loc    *time.Location
	now    func() time.Time

	mu          sync.Mutex
	rule        *GridRule              // nil 表示未开启
	setup       *gridChallenge         // 开启时的练习题
	rounds      map[string]*checkRound // 会话 ID → 正在进行的抽查
	fails       int                    // 恢复码连续输错次数
	lockedUntil time.Time
}

func NewGrid(ctx context.Context, store GridStore, masterKey []byte, loc *time.Location) (*Grid, error) {
	g := &Grid{store: store, master: masterKey, loc: loc, now: time.Now, rounds: map[string]*checkRound{}}
	wrapped, err := store.GetGridRule(ctx)
	if errors.Is(err, meta.ErrNotFound) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := crypto.UnwrapKey(masterKey, wrapped)
	if err != nil {
		return nil, fmt.Errorf("decrypt grid rule: %w", err)
	}
	var rule GridRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("decode grid rule: %w", err)
	}
	if err := rule.Validate(); err != nil {
		return nil, fmt.Errorf("stored grid rule: %w", err)
	}
	g.rule = &rule
	return g, nil
}

// Enabled 报告是否开启了格子口令。
func (g *Grid) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rule != nil
}

func (g *Grid) newChallenge(now time.Time, ttl time.Duration) (*gridChallenge, Challenge, error) {
	cells, err := newGridCells()
	if err != nil {
		return nil, Challenge{}, err
	}
	id, err := randomString(18)
	if err != nil {
		return nil, Challenge{}, err
	}
	local := now.In(g.loc)
	c := &gridChallenge{id: id, cells: cells, weekday: Weekday(local), deadline: now.Add(ttl)}
	return c, Challenge{
		ID: id, Grid: cells, Weekday: c.weekday,
		IssuedAt: now.UTC(), IssuedLocal: local.Format("2006-01-02 15:04"), Timezone: g.loc.String(),
		ExpiresAt: c.deadline.UTC(),
	}, nil
}

// failAttempt 用掉当前这次机会。还有机会时返回 reason，否则结束这一轮并返回 ErrGridFailed。调用方持锁。
func (g *Grid) failAttempt(key string, r *checkRound, reason error) error {
	r.cur = nil
	if r.attempt >= checkAttempts {
		delete(g.rounds, key)
		return ErrGridFailed
	}
	r.attempt++
	return reason
}

// NewCheck 给会话 key 出一道抽查题。上一道题还没答（关掉弹窗、重新出题）就算这次机会用掉了：
// 两次机会都用完时返回 ErrGridFailed。
func (g *Grid) NewCheck(key string) (Challenge, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rule == nil {
		return Challenge{}, ErrGridDisabled
	}
	now := g.now()
	r := g.rounds[key]
	if r == nil {
		for k, old := range g.rounds { // 清掉早已没人管的
			if now.Sub(old.started) > SessionTTL {
				delete(g.rounds, k)
			}
		}
		r = &checkRound{attempt: 1, started: now}
		g.rounds[key] = r
	} else if r.cur != nil {
		if err := g.failAttempt(key, r, ErrGridTimeout); errors.Is(err, ErrGridFailed) {
			return Challenge{}, err
		}
	}
	limit := CheckTimeFirst
	if r.attempt > 1 {
		limit = CheckTimeSecond
	}
	c, ch, err := g.newChallenge(now, limit)
	if err != nil {
		return Challenge{}, err
	}
	r.cur = c
	ch.Attempt, ch.Seconds = r.attempt, int(limit/time.Second)
	return ch, nil
}

// Check 校验会话 key 的抽查答案（timedOut 表示页面倒计时已到、没有作答）。
// 答对返回 nil；第一次答错 / 超时返回 ErrGridWrong / ErrGridTimeout（再出一题）；
// 第二次也没答对返回 ErrGridFailed；题目对不上返回 ErrGridChallenge（不算机会）。
func (g *Grid) Check(key, id string, answer int, timedOut bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rule == nil {
		return ErrGridDisabled
	}
	r := g.rounds[key]
	if r == nil || r.cur == nil || r.cur.id != id {
		return ErrGridChallenge
	}
	c := r.cur
	switch {
	case timedOut || g.now().After(c.deadline.Add(checkGrace)):
		return g.failAttempt(key, r, ErrGridTimeout)
	case g.rule.Answer(c.cells, c.weekday) != answer:
		return g.failAttempt(key, r, ErrGridWrong)
	}
	delete(g.rounds, key)
	return nil
}

// NewSetupChallenge 出开启时的练习题（按提交的新规则作答，不限时，3 分钟内有效）。
func (g *Grid) NewSetupChallenge() (Challenge, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rule != nil {
		return Challenge{}, ErrGridEnabled
	}
	c, ch, err := g.newChallenge(g.now(), setupChallengeTTL)
	if err != nil {
		return Challenge{}, err
	}
	g.setup = c
	return ch, nil
}

// newGridCells 生成一张格子：0–9 每个数字至少出现一次（答案靠点格子里的数字输入，
// 缺了某个数字就点不出来），其余 6 格随机，再整体打乱。
func newGridCells() ([GridCells]int, error) {
	var cells [GridCells]int
	randN := func(n int) (int, error) {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		return int(v.Int64()), err
	}
	for i := range cells {
		if i < 10 {
			cells[i] = i
			continue
		}
		n, err := randN(10)
		if err != nil {
			return cells, err
		}
		cells[i] = n
	}
	for i := len(cells) - 1; i > 0; i-- { // Fisher–Yates
		j, err := randN(i + 1)
		if err != nil {
			return cells, err
		}
		cells[i], cells[j] = cells[j], cells[i]
	}
	return cells, nil
}

// UseRecoveryCode 校验并作废一个恢复码。连续输错 recoveryMaxFails 次后锁定 recoveryLockout。
func (g *Grid) UseRecoveryCode(ctx context.Context, code string) error {
	g.mu.Lock()
	if now := g.now(); now.Before(g.lockedUntil) {
		g.mu.Unlock()
		return &LockedError{Until: g.lockedUntil}
	}
	if g.rule == nil {
		g.mu.Unlock()
		return ErrGridDisabled
	}
	g.mu.Unlock()

	ok, err := g.store.UseRecoveryCode(ctx, hashRecoveryCode(code))
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !ok {
		if g.fails++; g.fails >= recoveryMaxFails {
			g.fails = 0
			g.lockedUntil = g.now().Add(recoveryLockout)
			return &LockedError{Until: g.lockedUntil}
		}
		return ErrRecoveryCode
	}
	g.fails = 0
	return nil
}

// Setup 开启格子口令：必须先答对一道按新规则出的练习题，防止设错把自己锁在外面。
// 返回一组新的恢复码（只返回这一次）。
func (g *Grid) Setup(ctx context.Context, id string, rule GridRule, answer int) ([]string, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.rule != nil {
		g.mu.Unlock()
		return nil, ErrGridEnabled
	}
	c := g.setup
	g.setup = nil // 一道题只能答一次
	g.mu.Unlock()
	if c == nil || c.id != id || g.now().After(c.deadline) {
		return nil, ErrGridChallenge
	}
	if rule.Answer(c.cells, c.weekday) != answer {
		return nil, ErrGridWrong
	}

	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	wrapped, err := crypto.WrapKey(g.master, raw)
	if err != nil {
		return nil, err
	}
	if err := g.store.SetGridRule(ctx, wrapped, hashes); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.rule = &rule
	g.fails = 0
	clear(g.rounds)
	g.mu.Unlock()
	return codes, nil
}

// Disable 关闭格子口令并作废全部恢复码（也用于紧急重置）。
func (g *Grid) Disable(ctx context.Context) error {
	if err := g.store.DeleteGridRule(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	g.rule = nil
	g.setup = nil
	clear(g.rounds)
	g.mu.Unlock()
	return nil
}

type GridStatus struct {
	Enabled      bool   `json:"enabled"`
	RecoveryLeft int    `json:"recovery_left"`
	Timezone     string `json:"timezone"`
}

func (g *Grid) Status(ctx context.Context) (GridStatus, error) {
	st := GridStatus{Enabled: g.Enabled(), Timezone: g.loc.String()}
	if !st.Enabled {
		return st, nil
	}
	n, err := g.store.CountRecoveryCodes(ctx)
	st.RecoveryLeft = n
	return st, err
}

// newRecoveryCodes 生成恢复码（形如 abcd-efgh，40 位随机）及其哈希。
func newRecoveryCodes() (codes []string, hashes [][]byte, err error) {
	for range recoveryCodeCount {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		s := strings.ToLower(base32.StdEncoding.EncodeToString(b))
		code := s[:4] + "-" + s[4:]
		codes = append(codes, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return codes, hashes, nil
}

func hashRecoveryCode(code string) []byte {
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte("xiaoju recovery code\x00" + norm))
	return sum[:]
}
