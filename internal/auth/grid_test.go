package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

type memGridStore struct {
	rule  []byte
	codes map[string]bool // 哈希 → 已用
}

func (m *memGridStore) GetGridRule(context.Context) ([]byte, error) {
	if m.rule == nil {
		return nil, meta.ErrNotFound
	}
	return m.rule, nil
}

func (m *memGridStore) SetGridRule(_ context.Context, wrapped []byte, hashes [][]byte) error {
	m.rule = wrapped
	m.codes = map[string]bool{}
	for _, h := range hashes {
		m.codes[string(h)] = false
	}
	return nil
}

func (m *memGridStore) DeleteGridRule(context.Context) error {
	m.rule, m.codes = nil, nil
	return nil
}

func (m *memGridStore) UseRecoveryCode(_ context.Context, h []byte) (bool, error) {
	used, ok := m.codes[string(h)]
	if !ok || used {
		return false, nil
	}
	m.codes[string(h)] = true
	return true, nil
}

func (m *memGridStore) CountRecoveryCodes(context.Context) (int, error) {
	n := 0
	for _, used := range m.codes {
		if !used {
			n++
		}
	}
	return n, nil
}

// 示例规则：第 1 行第 2 格 × 星期 + 第 3 行第 4 格，周一再加 5
var testRule = GridRule{Mul: 1, Add: 11, MondayBonus: 5}

var testCells = [GridCells]int{
	3, 7, 1, 4,
	8, 2, 6, 0,
	5, 9, 4, 2,
	1, 6, 3, 8,
}

func TestGridRuleAnswer(t *testing.T) {
	want := map[int]int{1: 7 + 5 + 2, 2: 14 + 2, 3: 21 + 2, 4: 28 + 2, 5: 35 + 2, 6: 42 + 2, 7: 49 + 2}
	for wd, w := range want {
		if got := testRule.Answer(testCells, wd); got != w {
			t.Errorf("weekday %d: got %d, want %d", wd, got, w)
		}
	}
	for _, bad := range []GridRule{{Mul: 1, Add: 1}, {Mul: -1, Add: 2}, {Mul: 1, Add: 16}, {Mul: 1, Add: 2, MondayBonus: 100}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}

func TestWeekday(t *testing.T) {
	// 2026-10-12 是周一，2026-10-18 是周日
	for day, want := range map[int]int{12: 1, 14: 3, 17: 6, 18: 7} {
		if got := Weekday(time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC)); got != want {
			t.Errorf("Oct %d: got %d, want %d", day, got, want)
		}
	}
}

func newTestGrid(t *testing.T, now *time.Time) (*Grid, *memGridStore) {
	t.Helper()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	st := &memGridStore{}
	g, err := NewGrid(context.Background(), st, master, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return *now }
	return g, st
}

// enable 按 testRule 开启格子口令，返回恢复码
func enable(t *testing.T, g *Grid) []string {
	t.Helper()
	ch, err := g.NewChallenge("setup")
	if err != nil {
		t.Fatal(err)
	}
	codes, err := g.Setup(context.Background(), ch.ID, testRule, testRule.Answer(ch.Grid, ch.Weekday))
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func TestGridSetupAndVerify(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC) // 北京时间周三中午
	g, st := newTestGrid(t, &now)
	ctx := context.Background()

	if _, err := g.NewChallenge("login"); !errors.Is(err, ErrGridDisabled) {
		t.Fatalf("login challenge before setup: %v", err)
	}
	// 练习题答错：不开启，题目作废
	ch, _ := g.NewChallenge("setup")
	if _, err := g.Setup(ctx, ch.ID, testRule, testRule.Answer(ch.Grid, ch.Weekday)+1); !errors.Is(err, ErrGridWrong) {
		t.Fatalf("wrong practice answer: %v", err)
	}
	if _, err := g.Setup(ctx, ch.ID, testRule, testRule.Answer(ch.Grid, ch.Weekday)); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("practice challenge reused: %v", err)
	}
	if g.Enabled() {
		t.Fatal("must not be enabled after a wrong practice answer")
	}
	codes := enable(t, g)
	if len(codes) != recoveryCodeCount || len(codes[0]) != 9 || codes[0][4] != '-' {
		t.Fatalf("recovery codes: %v", codes)
	}
	if bytes.Contains(st.rule, []byte("monday")) {
		t.Fatal("rule must be stored encrypted")
	}
	if _, err := g.NewChallenge("setup"); !errors.Is(err, ErrGridEnabled) {
		t.Fatalf("setup while enabled: %v", err)
	}

	// 重启后从数据库恢复规则
	g2, err := NewGrid(ctx, st, g.master, g.loc)
	if err != nil || !g2.Enabled() || *g2.rule != testRule {
		t.Fatalf("reload: %v %+v", err, g2.rule)
	}

	ch, err = g.NewChallenge("login")
	if err != nil || ch.Weekday != 3 || ch.Timezone != "Asia/Shanghai" || ch.IssuedLocal != "2026-10-14 12:00" {
		t.Fatalf("login challenge: %v %+v", err, ch)
	}
	if err := g.Verify(ch.ID, testRule.Answer(ch.Grid, 3)); err != nil {
		t.Fatalf("correct answer: %v", err)
	}
	if err := g.Verify(ch.ID, testRule.Answer(ch.Grid, 3)); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("answered challenge reused: %v", err)
	}

	// 新出一题，旧题作废
	old, _ := g.NewChallenge("login")
	cur, _ := g.NewChallenge("login")
	if err := g.Verify(old.ID, testRule.Answer(old.Grid, 3)); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("superseded challenge: %v", err)
	}
	// 答错一次，题目作废，正确答案也不行
	if err := g.Verify(cur.ID, testRule.Answer(cur.Grid, 3)+1); !errors.Is(err, ErrGridWrong) {
		t.Fatalf("wrong answer: %v", err)
	}
	if err := g.Verify(cur.ID, testRule.Answer(cur.Grid, 3)); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("retry after wrong answer: %v", err)
	}
	// 题目 3 分钟过期
	ch, _ = g.NewChallenge("login")
	now = now.Add(gridChallengeTTL + time.Second)
	if err := g.Verify(ch.ID, testRule.Answer(ch.Grid, 3)); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("expired challenge: %v", err)
	}
}

func TestGridWeekdayFixedAtIssue(t *testing.T) {
	// UTC 周二 20:00 = 北京时间周三 04:00：按周三算
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	enable(t, g)
	ch, _ := g.NewChallenge("login")
	if ch.Weekday != 3 {
		t.Fatalf("weekday = %d, want 3 (Wednesday in Shanghai)", ch.Weekday)
	}

	// 北京时间周三 23:59 出题、周四 00:01 作答：仍按周三算
	now = time.Date(2026, 10, 14, 15, 59, 0, 0, time.UTC)
	ch, _ = g.NewChallenge("login")
	now = now.Add(2 * time.Minute)
	if Weekday(now.In(g.loc)) != 4 {
		t.Fatal("test setup: answer time should be Thursday")
	}
	if err := g.Verify(ch.ID, testRule.Answer(ch.Grid, 3)); err != nil {
		t.Fatalf("answer across midnight must use the issue weekday: %v", err)
	}

	// 周一（北京时间）要加周一的固定数
	now = time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	ch, _ = g.NewChallenge("login")
	want := ch.Grid[1]*1 + 5 + ch.Grid[11]
	if ch.Weekday != 1 || g.Verify(ch.ID, want) != nil {
		t.Fatalf("Monday rule: weekday %d", ch.Weekday)
	}
	// 周日（北京时间）算 7
	now = time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	ch, _ = g.NewChallenge("login")
	if ch.Weekday != 7 || g.Verify(ch.ID, ch.Grid[1]*7+ch.Grid[11]) != nil {
		t.Fatalf("Sunday rule: weekday %d", ch.Weekday)
	}
}

func TestGridLockout(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	enable(t, g)
	wrong := func() error {
		ch, err := g.NewChallenge("login")
		if err != nil {
			return err
		}
		return g.Verify(ch.ID, testRule.Answer(ch.Grid, ch.Weekday)+1)
	}
	// 答对会清零计数
	wrong()
	wrong()
	ch, _ := g.NewChallenge("login")
	if err := g.Verify(ch.ID, testRule.Answer(ch.Grid, ch.Weekday)); err != nil {
		t.Fatal(err)
	}
	wrong()
	wrong()
	var locked *LockedError
	if err := wrong(); !errors.As(err, &locked) || !locked.Until.Equal(now.Add(gridLockout)) {
		t.Fatalf("third wrong answer should lock: %v", err)
	}
	if _, err := g.NewChallenge("login"); !errors.As(err, &locked) {
		t.Fatalf("no new challenge while locked: %v", err)
	}
	if err := g.UseRecoveryCode(context.Background(), "aaaa-aaaa"); !errors.As(err, &locked) {
		t.Fatalf("recovery codes are locked too: %v", err)
	}
	now = now.Add(gridLockout)
	if err := wrong(); !errors.Is(err, ErrGridWrong) {
		t.Fatalf("lock should expire: %v", err)
	}
}

func TestRecoveryCodes(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	ctx := context.Background()
	codes := enable(t, g)

	// 大小写、空格、连字符都不影响
	if err := g.UseRecoveryCode(ctx, " "+strings.ToUpper(strings.Replace(codes[0], "-", "", 1))+" "); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if err := g.UseRecoveryCode(ctx, codes[0]); !errors.Is(err, ErrRecoveryCode) {
		t.Fatalf("recovery code reused: %v", err)
	}
	if st, _ := g.Status(ctx); !st.Enabled || st.RecoveryLeft != recoveryCodeCount-1 {
		t.Fatalf("status: %+v", st)
	}
	if err := g.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.UseRecoveryCode(ctx, codes[1]); !errors.Is(err, ErrGridDisabled) {
		t.Fatalf("codes after disable: %v", err)
	}
}
