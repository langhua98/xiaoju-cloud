package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	ch, err := g.NewSetupChallenge()
	if err != nil {
		t.Fatal(err)
	}
	codes, err := g.Setup(context.Background(), ch.ID, testRule, testRule.Answer(ch.Grid, ch.Weekday))
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func TestGridSetup(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC) // 北京时间周三中午
	g, st := newTestGrid(t, &now)
	ctx := context.Background()

	if _, err := g.NewCheck("s1"); !errors.Is(err, ErrGridDisabled) {
		t.Fatalf("check before setup: %v", err)
	}
	// 练习题答错：不开启，题目作废
	ch, _ := g.NewSetupChallenge()
	if ch.Seconds != 0 || ch.Attempt != 0 {
		t.Fatalf("practice must not be timed: %+v", ch)
	}
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
	if _, err := g.NewSetupChallenge(); !errors.Is(err, ErrGridEnabled) {
		t.Fatalf("setup while enabled: %v", err)
	}
	// 重启后从数据库恢复规则
	g2, err := NewGrid(ctx, st, g.master, g.loc)
	if err != nil || !g2.Enabled() || *g2.rule != testRule {
		t.Fatalf("reload: %v %+v", err, g2.rule)
	}
}

func TestGridCheckTwoChances(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	enable(t, g)
	right := func(ch Challenge) int { return testRule.Answer(ch.Grid, ch.Weekday) }

	// 第一次就答对
	ch, err := g.NewCheck("s1")
	if err != nil || ch.Attempt != 1 || ch.Seconds != 15 || ch.Weekday != 3 || ch.IssuedLocal != "2026-10-14 12:00" {
		t.Fatalf("first challenge: %v %+v", err, ch)
	}
	if err := g.Check("s1", ch.ID, right(ch), false); err != nil {
		t.Fatalf("correct answer: %v", err)
	}
	if err := g.Check("s1", ch.ID, right(ch), false); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("answered challenge reused: %v", err)
	}

	// 第一次答错 → 换新格子、限时 20 秒 → 答对
	ch, _ = g.NewCheck("s1")
	if err := g.Check("s1", ch.ID, right(ch)+1, false); !errors.Is(err, ErrGridWrong) {
		t.Fatalf("first wrong answer: %v", err)
	}
	if err := g.Check("s1", ch.ID, right(ch), false); !errors.Is(err, ErrGridChallenge) {
		t.Fatalf("same grid must not be answered twice: %v", err)
	}
	ch2, _ := g.NewCheck("s1")
	if ch2.Attempt != 2 || ch2.Seconds != 20 || ch2.ID == ch.ID {
		t.Fatalf("second chance: %+v", ch2)
	}
	if err := g.Check("s1", ch2.ID, right(ch2), false); err != nil {
		t.Fatalf("second chance correct: %v", err)
	}
	// 答对后下一轮又是两次机会
	if ch, _ = g.NewCheck("s1"); ch.Attempt != 1 {
		t.Fatalf("new round: %+v", ch)
	}

	// 两次都答错 → 失败（调用方退出登录），之后重新开始
	if err := g.Check("s1", ch.ID, right(ch)+1, false); !errors.Is(err, ErrGridWrong) {
		t.Fatal(err)
	}
	ch, _ = g.NewCheck("s1")
	if err := g.Check("s1", ch.ID, right(ch)+1, false); !errors.Is(err, ErrGridFailed) {
		t.Fatalf("second wrong answer: %v", err)
	}
	if ch, _ = g.NewCheck("s1"); ch.Attempt != 1 {
		t.Fatalf("round must restart after failure: %+v", ch)
	}

	// 各个会话互不影响
	other, _ := g.NewCheck("s2")
	if other.Attempt != 1 || g.Check("s1", other.ID, right(other), false) == nil {
		t.Fatal("a session must not answer another session's grid")
	}
}

func TestGridCheckTimeLimits(t *testing.T) {
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	enable(t, g)
	right := func(ch Challenge) int { return testRule.Answer(ch.Grid, ch.Weekday) }

	// 第一次 15 秒（另有 2 秒网络余量）：16 秒时答对仍然有效
	ch, _ := g.NewCheck("s")
	now = now.Add(16 * time.Second)
	if err := g.Check("s", ch.ID, right(ch), false); err != nil {
		t.Fatalf("answer within grace: %v", err)
	}
	// 超过 15+2 秒：算超时，进入第二次机会（20 秒）
	ch, _ = g.NewCheck("s")
	now = now.Add(18 * time.Second)
	if err := g.Check("s", ch.ID, right(ch), false); !errors.Is(err, ErrGridTimeout) {
		t.Fatalf("late answer: %v", err)
	}
	ch, _ = g.NewCheck("s")
	now = now.Add(21 * time.Second) // 第二次多给了 5 秒
	if err := g.Check("s", ch.ID, right(ch), false); err != nil {
		t.Fatalf("second chance has 20s: %v", err)
	}

	// 页面倒计时到了主动报告超时
	ch, _ = g.NewCheck("s")
	if err := g.Check("s", ch.ID, 0, true); !errors.Is(err, ErrGridTimeout) {
		t.Fatalf("reported timeout: %v", err)
	}
	// 出了题不答、直接再要一道：算用掉一次机会；两次都这样就失败
	if _, err := g.NewCheck("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewCheck("s"); !errors.Is(err, ErrGridFailed) {
		t.Fatalf("abandoned second chance: %v", err)
	}
}

func TestGridWeekdayFixedAtIssue(t *testing.T) {
	// UTC 周二 20:00 = 北京时间周三 04:00：按周三算
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	g, _ := newTestGrid(t, &now)
	enable(t, g)
	ch, _ := g.NewCheck("s")
	if ch.Weekday != 3 {
		t.Fatalf("weekday = %d, want 3 (Wednesday in Shanghai)", ch.Weekday)
	}
	g.Check("s", ch.ID, testRule.Answer(ch.Grid, 3), false)

	// 北京时间周三 23:59:50 出题、周四 00:00:02 作答：仍按周三算
	now = time.Date(2026, 10, 14, 15, 59, 50, 0, time.UTC)
	ch, _ = g.NewCheck("s")
	now = now.Add(12 * time.Second)
	if Weekday(now.In(g.loc)) != 4 {
		t.Fatal("test setup: answer time should be Thursday")
	}
	if err := g.Check("s", ch.ID, testRule.Answer(ch.Grid, 3), false); err != nil {
		t.Fatalf("answer across midnight must use the issue weekday: %v", err)
	}

	// 周一（北京时间）要加周一的固定数
	now = time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	ch, _ = g.NewCheck("s")
	if ch.Weekday != 1 || g.Check("s", ch.ID, ch.Grid[1]*1+5+ch.Grid[11], false) != nil {
		t.Fatalf("Monday rule: weekday %d", ch.Weekday)
	}
	// 周日（北京时间）算 7
	now = time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	ch, _ = g.NewCheck("s")
	if ch.Weekday != 7 || g.Check("s", ch.ID, ch.Grid[1]*7+ch.Grid[11], false) != nil {
		t.Fatalf("Sunday rule: weekday %d", ch.Weekday)
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
	// 连续输错 3 次锁定 1 小时，期间正确的也不收
	g.UseRecoveryCode(ctx, "aaaa-aaaa")
	var locked *LockedError
	if err := g.UseRecoveryCode(ctx, "bbbb-bbbb"); !errors.As(err, &locked) || !locked.Until.Equal(now.Add(recoveryLockout)) {
		t.Fatalf("third wrong code should lock: %v", err)
	}
	if err := g.UseRecoveryCode(ctx, codes[1]); !errors.As(err, &locked) {
		t.Fatalf("locked: %v", err)
	}
	now = now.Add(recoveryLockout)
	if err := g.UseRecoveryCode(ctx, codes[1]); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
	if err := g.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.UseRecoveryCode(ctx, codes[2]); !errors.Is(err, ErrGridDisabled) {
		t.Fatalf("codes after disable: %v", err)
	}
}

func TestGridCellsContainEveryDigit(t *testing.T) {
	// 答案靠点格子里的数字输入，所以每张格子都要有 0–9；同时不能总在固定位置
	pos := map[int]map[int]bool{} // 数字 → 出现过的位置
	for range 500 {
		cells, err := newGridCells()
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int]bool{}
		for i, n := range cells {
			if n < 0 || n > 9 {
				t.Fatalf("cell %d = %d", i, n)
			}
			seen[n] = true
			if pos[n] == nil {
				pos[n] = map[int]bool{}
			}
			pos[n][i] = true
		}
		if len(seen) != 10 {
			t.Fatalf("grid %v is missing digits", cells)
		}
	}
	for d := range 10 {
		if len(pos[d]) != GridCells {
			t.Fatalf("digit %d appeared only at positions %v; grid must be shuffled", d, pos[d])
		}
	}
}

func TestViews(t *testing.T) {
	v := NewViews()
	limits := map[int]bool{}
	for range 200 {
		id := fmt.Sprint(len(limits), "-", time.Now().UnixNano())
		n := 0
		for v.Allow(id) {
			n++
		}
		if n < ViewsMin || n > ViewsMax {
			t.Fatalf("quota %d outside %d-%d", n, ViewsMin, ViewsMax)
		}
		limits[n] = true
		if v.Allow(id) {
			t.Fatal("quota must stay used up until Reset")
		}
		v.Reset(id)
		if !v.Allow(id) {
			t.Fatal("Reset must give a new quota")
		}
	}
	if len(limits) < 4 {
		t.Fatalf("quota should be random across 3-7, got %v", limits)
	}
}
