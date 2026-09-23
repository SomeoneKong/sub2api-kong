//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const kongPaceTestYAML = `
enabled: true
gap_doubling_pp: 10
gap_cap_pp: 30
max_penalty_pp: 20
plans:
  pro:
    capacity: 1
    yield_pp: 0
    soft_line_percent:
      6h:  { start: 25, full: 50 }
      24h: { start: 40, full: 80 }
  prolite:
    capacity: 0.25
    yield_pp: 10
    soft_line_percent:
      6h:  { start: 25, full: 50 }
      24h: { start: 40, full: 80 }
default_plan: pro
concurrency:
  peak_window_minutes: 5
  idle_bonus_pp: 10
  busy_ratio: 0.667
  penalty_pp:
    free_2_plus: 10
    free_1: 30
    full: 50
decision_log:
  enabled: true
  retention_days: 90
`

var kongPaceT0 = time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)

func kongPaceTestConfig(t *testing.T) *KongPaceConfig {
	t.Helper()
	cfg, err := ParseKongPaceConfig([]byte(kongPaceTestYAML))
	if err != nil {
		t.Fatalf("parse sample config: %v", err)
	}
	return &cfg
}

func kongPaceF(v float64) *float64      { return &v }
func kongPaceI(v int) *int              { return &v }
func kongPaceTm(t time.Time) *time.Time { return &t }

func kongPaceRow(id int64, plan string, used float64, reset time.Time, updated time.Time) KongPaceAccountRow {
	return KongPaceAccountRow{ID: id, Plan: plan, Concurrency: 8, UsedPercent: kongPaceF(used),
		ResetAt: kongPaceTm(reset), WindowMinutes: kongPaceI(10080), UsageUpdatedAt: kongPaceTm(updated)}
}

func TestKongPaceConfig_SampleParses(t *testing.T) {
	cfg := kongPaceTestConfig(t)
	if !cfg.Enabled || cfg.GapCapPP != 30 || cfg.Plans["prolite"].Capacity != 0.25 || cfg.Concurrency.PenaltyPP.Free1 != 30 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if got := cfg.plan("plus"); got.Capacity != 1 {
		t.Fatalf("unknown plan must fall back to default plan, got %+v", got)
	}
}

func TestKongPaceConfig_Rejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"missing field": {func(s string) string { return strings.Replace(s, "gap_cap_pp: 30\n", "", 1) }, "gap_cap_pp: 缺失"},
		"unknown field": {func(s string) string { return s + "extra_knob: 1\n" }, "extra_knob"},
		"start not below": {func(s string) string {
			return strings.Replace(s, "6h:  { start: 25, full: 50 }", "6h:  { start: 50, full: 50 }", 1)
		}, "start < full"},
		"full above 100": {func(s string) string {
			return strings.Replace(s, "24h: { start: 40, full: 80 }", "24h: { start: 40, full: 120 }", 1)
		}, "0 < x <= 100"},
		"default not a plan": {func(s string) string { return strings.Replace(s, "default_plan: pro", "default_plan: plus", 1) }, "不在 plans 里"},
		"penalty order":      {func(s string) string { return strings.Replace(s, "free_1: 30", "free_1: 60", 1) }, "free_2_plus <= free_1 <= full"},
		"busy ratio":         {func(s string) string { return strings.Replace(s, "busy_ratio: 0.667", "busy_ratio: 1", 1) }, "0 < x < 1"},
		"peak window": {func(s string) string {
			return strings.Replace(s, "peak_window_minutes: 5", "peak_window_minutes: 61", 1)
		}, "1 <= x <= 60"},
		"weight overflow": {func(s string) string {
			return strings.Replace(s, "gap_doubling_pp: 10", "gap_doubling_pp: 0.01", 1)
		}, "权重会溢出"},
		"capacity too large": {func(s string) string {
			return strings.Replace(s, "capacity: 1\n", "capacity: 1000\n", 1)
		}, "0 < x <= 100"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKongPaceConfig([]byte(tc.mutate(kongPaceTestYAML)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestKongPaceConfigSource_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KongPaceConfigFileName)
	src := newKongPaceConfigSource(path)

	if cur, ev, err := src.check(kongPaceT0); cur != nil || ev != kongPaceConfigUnchanged || err != nil {
		t.Fatalf("absent file: cur=%v ev=%v err=%v", cur, ev, err)
	}

	// 启动时文件非法：功能关闭。
	if err := os.WriteFile(path, []byte("enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cur, ev, _ := src.check(kongPaceT0); cur != nil || ev != kongPaceConfigInvalid {
		t.Fatalf("invalid at start must stay off: cur=%v ev=%v", cur, ev)
	}

	writeAt := func(content string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	writeAt(kongPaceTestYAML, kongPaceT0.Add(time.Minute))
	cur, ev, err := src.check(kongPaceT0)
	if cur == nil || ev != kongPaceConfigLoaded || err != nil || len(cur.json) == 0 || len(cur.version) != 12 {
		t.Fatalf("valid file must load: cur=%v ev=%v err=%v", cur, ev, err)
	}
	version := cur.version

	// 运行中改坏：沿用上一份有效配置。
	writeAt("enabled: [\n", kongPaceT0.Add(2*time.Minute))
	cur, ev, _ = src.check(kongPaceT0)
	if cur == nil || cur.version != version || ev != kongPaceConfigInvalid {
		t.Fatalf("broken file must keep previous config: cur=%v ev=%v", cur, ev)
	}
	if _, lastErr := src.status(); lastErr == "" {
		t.Fatal("load error must be reported")
	}

	// 运行中删除：功能关闭。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if cur, ev, _ := src.check(kongPaceT0); cur != nil || ev != kongPaceConfigRemoved {
		t.Fatalf("removed file must turn off: cur=%v ev=%v", cur, ev)
	}
}

func TestKongPaceConfigSource_RetriesAfterReadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KongPaceConfigFileName)
	src := newKongPaceConfigSource(path)
	failing := true
	src.readFile = func(name string) ([]byte, error) {
		if failing {
			return nil, os.ErrPermission
		}
		return os.ReadFile(name)
	}
	if err := os.WriteFile(path, []byte(kongPaceTestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if cur, ev, _ := src.check(kongPaceT0); cur != nil || ev != kongPaceConfigInvalid {
		t.Fatalf("unreadable file: cur=%v ev=%v", cur, ev)
	}
	// 修好权限不改修改时间与大小，下一拍也要重新读。
	failing = false
	cur, ev, _ := src.check(kongPaceT0)
	if cur == nil || ev != kongPaceConfigLoaded {
		t.Fatalf("readable again must load: cur=%v ev=%v", cur, ev)
	}
	version := cur.version

	changed := strings.Replace(kongPaceTestYAML, "gap_cap_pp: 30", "gap_cap_pp: 40", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := kongPaceT0.Add(time.Hour)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	failing = true
	if cur, ev, _ := src.check(kongPaceT0); cur == nil || cur.version != version || ev != kongPaceConfigInvalid {
		t.Fatalf("unreadable change must keep the previous config: cur=%v ev=%v", cur, ev)
	}
	failing = false
	if cur, ev, _ := src.check(kongPaceT0); cur == nil || cur.version == version || cur.cfg.GapCapPP != 40 || ev != kongPaceConfigLoaded {
		t.Fatalf("the change must load once readable: cur=%v ev=%v", cur, ev)
	}
}

func TestKongPaceObserve_WindowJudgement(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	st, obs := kongPaceObserve(nil, kongPaceRow(1, "pro", 40, reset, kongPaceT0), kongPaceT0)
	if obs != kongPaceObsBaseline || !st.HasWindow || st.MaxUsed != 40 || len(st.Rises) != 0 {
		t.Fatalf("first sight must be a baseline without rises: obs=%v st=%+v", obs, st)
	}

	t1 := kongPaceT0.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "pro", 43, reset.Add(3*time.Minute), t1.Add(-10*time.Second)), t1)
	if obs != kongPaceObsSameWindow || kongPaceIncrease(st, t1, 6*time.Hour) != 3 {
		t.Fatalf("same window rise: obs=%v rises=%+v", obs, st.Rises)
	}

	// 回退 1 个点再恢复，不重复计。
	t2 := t1.Add(time.Minute)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 42, reset, t2.Add(-5*time.Second)), t2)
	t3 := t2.Add(time.Minute)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 43, reset, t3.Add(-5*time.Second)), t3)
	if got := kongPaceIncrease(st, t3, 6*time.Hour); got != 3 {
		t.Fatalf("flicker must not add consumption, got %v", got)
	}

	// 回滚：重置时刻早 1 小时以上，整条忽略，也不改权重用的读数。
	t4 := t3.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "pro", 90, reset.Add(-48*time.Hour), t4.Add(-5*time.Second)), t4)
	if obs != kongPaceObsRollback || st.MaxUsed != 43 || kongPaceIncrease(st, t4, 6*time.Hour) != 3 {
		t.Fatalf("rollback must be ignored: obs=%v st=%+v", obs, st)
	}

	// 重置：新窗口的读数就是上涨，跨过重置连续统计。
	t5 := t4.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "pro", 5, reset.Add(7*24*time.Hour), t5.Add(-5*time.Second)), t5)
	if obs != kongPaceObsReset || st.MaxUsed != 5 || kongPaceIncrease(st, t5, 6*time.Hour) != 8 {
		t.Fatalf("reset must continue accounting: obs=%v st=%+v", obs, st)
	}
}

func TestKongPaceObserve_PlanChangeRestartsAccounting(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	st, _ := kongPaceObserve(nil, kongPaceRow(1, "pro", 10, reset, kongPaceT0), kongPaceT0)
	t1 := kongPaceT0.Add(time.Minute)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 30, reset, t1), t1)

	// 套餐先变、读数未刷新：清空记录，等之后的新读数再建起点。
	t2 := t1.Add(time.Minute)
	st, obs := kongPaceObserve(st, kongPaceRow(1, "prolite", 30, reset, t1), t2)
	if obs != kongPaceObsPlanChange || st.HasWindow || len(st.Rises) != 0 || !st.TrackedSince.Equal(t2) {
		t.Fatalf("plan change must clear and wait: obs=%v st=%+v", obs, st)
	}
	if p := kongPaceComputeProgress(st, nil, t2); p.Gap != 0 || p.Running {
		t.Fatalf("waiting account must be treated as on-pace, got %+v", p)
	}
	// 发现变化之后才落库、但读数时刻不晚于发现时刻的读数，仍可能是旧套餐的。
	late := t2.Add(30 * time.Second)
	for _, at := range []time.Time{t2.Add(-time.Second), t2} {
		st, obs = kongPaceObserve(st, kongPaceRow(1, "prolite", 55, reset, at), late)
		if obs != kongPaceObsNone || st.HasWindow {
			t.Fatalf("reading at %v must not become the new baseline: obs=%v st=%+v", at, obs, st)
		}
	}
	t3 := t2.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "prolite", 12, reset.Add(-60*time.Hour), t3), t3)
	if obs != kongPaceObsBaseline || !st.HasWindow || st.MaxUsed != 12 || len(st.Rises) != 0 {
		t.Fatalf("fresh reading after plan change must become the baseline even if reset moves earlier: obs=%v st=%+v", obs, st)
	}
}

func TestKongPaceObserve_GapAttributedToGapStart(t *testing.T) {
	reset := kongPaceT0.Add(100 * time.Hour)
	st, _ := kongPaceObserve(nil, kongPaceRow(1, "pro", 10, reset, kongPaceT0), kongPaceT0)
	// 0 时之后中断，10 时上游读数涨到 60，12 时恢复处理。
	resume := kongPaceT0.Add(12 * time.Hour)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 60, reset, kongPaceT0.Add(10*time.Hour)), resume)
	if got := kongPaceIncrease(st, resume, 6*time.Hour); got != 0 {
		t.Fatalf("rise during a gap must be booked at the gap start, inc6=%v", got)
	}
	if got := kongPaceIncrease(st, resume, 24*time.Hour); got != 50 {
		t.Fatalf("inc24 = %v, want 50", got)
	}
	later := kongPaceT0.Add(26 * time.Hour)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 60, reset, kongPaceT0.Add(10*time.Hour)), later)
	if len(st.Rises) != 0 {
		t.Fatalf("rises older than retention must be trimmed: %+v", st.Rises)
	}
}

func TestKongPaceObserve_InactiveWindowIsIdle(t *testing.T) {
	inactive := func(at time.Time) KongPaceAccountRow {
		return KongPaceAccountRow{ID: 1, Plan: "pro", Concurrency: 8, WindowMinutes: kongPaceI(0), UsageUpdatedAt: kongPaceTm(at)}
	}
	exp := kongPaceT0.Add(48 * time.Hour)
	idleGap := 100 * (1 - 48.0/168)

	st, obs := kongPaceObserve(nil, inactive(kongPaceT0), kongPaceT0)
	if obs != kongPaceObsInactive || !st.Inactive {
		t.Fatalf("first sight of an inactive window: obs=%v st=%+v", obs, st)
	}
	if p := kongPaceComputeProgress(st, &exp, kongPaceT0); p.Running || math.Abs(p.Gap-idleGap) > 1e-9 {
		t.Fatalf("inactive account must use the idle formula: %+v", p)
	}

	// 激活：此前已知用量为 0，激活后的读数记为上涨。
	reset := kongPaceT0.Add(7 * 24 * time.Hour)
	t1 := kongPaceT0.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "pro", 3, reset, t1), t1)
	if obs != kongPaceObsBaseline || st.Inactive || !st.HasWindow || kongPaceIncrease(st, t1, 6*time.Hour) != 3 {
		t.Fatalf("activation: obs=%v st=%+v", obs, st)
	}

	// 再次未激活：窗口与消耗记录保留，进度按空闲算。
	t2 := t1.Add(time.Minute)
	st, obs = kongPaceObserve(st, inactive(t2), t2)
	if obs != kongPaceObsInactive || !st.Inactive || !st.HasWindow || kongPaceIncrease(st, t2, 6*time.Hour) != 3 {
		t.Fatalf("inactive again must keep history: obs=%v st=%+v", obs, st)
	}
	if p := kongPaceComputeProgress(st, &exp, t2); p.Running || p.Used != 0 || math.Abs(p.Gap-100*(1-float64(exp.Sub(t2))/float64(kongPaceStandardWindow))) > 1e-9 {
		t.Fatalf("inactive with a remembered window must still be idle: %+v", p)
	}

	// 重新激活同一窗口：不是换套餐，照常记上涨。
	t3 := t2.Add(time.Minute)
	st, obs = kongPaceObserve(st, kongPaceRow(1, "pro", 5, reset, t3), t3)
	if obs != kongPaceObsSameWindow || st.Inactive || kongPaceIncrease(st, t3, 6*time.Hour) != 5 {
		t.Fatalf("reactivation of the same window: obs=%v st=%+v", obs, st)
	}
}

func TestKongPaceProgress(t *testing.T) {
	window := 7 * 24 * time.Hour
	start := kongPaceT0.Add(-96 * time.Hour)
	st := &kongPaceAccountState{HasWindow: true, WindowMinutes: 10080, ResetAt: start.Add(window), MaxUsed: 43}
	p := kongPaceComputeProgress(st, nil, kongPaceT0)
	if !p.Running || math.Abs(p.Gap-(100*96.0/168-43)) > 1e-9 || p.DeadlineSource != "reset" {
		t.Fatalf("running progress: %+v", p)
	}

	// 订阅到期早于重置：分母是实际可用的时长。
	expires := start.Add(120 * time.Hour)
	p = kongPaceComputeProgress(st, &expires, kongPaceT0)
	if p.DeadlineSource != "subscription" || math.Abs(p.Gap-(100*96.0/120-43)) > 1e-9 {
		t.Fatalf("subscription cut progress: %+v", p)
	}

	// 空闲：重置时刻已过；订阅 2 天后到期。
	idle := &kongPaceAccountState{HasWindow: true, WindowMinutes: 10080, ResetAt: kongPaceT0.Add(-time.Hour), MaxUsed: 80}
	exp2 := kongPaceT0.Add(48 * time.Hour)
	p = kongPaceComputeProgress(idle, &exp2, kongPaceT0)
	if p.Running || math.Abs(p.Gap-100*(1-48.0/168)) > 1e-9 {
		t.Fatalf("idle with expiry: %+v", p)
	}
	if p = kongPaceComputeProgress(idle, nil, kongPaceT0); p.Gap != 0 {
		t.Fatalf("idle without expiry must be 0, got %+v", p)
	}
}

func TestKongPaceShortPenalty(t *testing.T) {
	plan := kongPaceTestConfig(t).Plans["pro"]
	q6, q24, q, at := kongPaceShortPenalty(40, 30, plan, 20)
	if q6 != 12 || q24 != 0 || q != 12 || at {
		t.Fatalf("linear band: q6=%v q24=%v q=%v at=%v", q6, q24, q, at)
	}
	q6, q24, q, at = kongPaceShortPenalty(30, 60, plan, 20)
	if q6 != 4 || q24 != 10 || q != 10 || at {
		t.Fatalf("max of windows, not sum: q6=%v q24=%v q=%v", q6, q24, q)
	}
	if _, _, q, at = kongPaceShortPenalty(10, 80, plan, 20); q != 20 || !at {
		t.Fatalf("24h at line: q=%v at=%v", q, at)
	}
}

func TestKongPaceConcurrencyAdjust(t *testing.T) {
	c := kongPaceTestConfig(t).Concurrency
	cases := []struct {
		peak, limit int
		recent      bool
		want        float64
	}{
		{0, 8, false, -10}, {0, 8, true, 0}, {5, 8, false, 0}, {6, 8, false, 10}, {7, 8, false, 30}, {8, 8, false, 50}, {10, 8, false, 50},
		{3, 5, false, 0}, {4, 5, false, 30}, {5, 5, false, 50},
	}
	for _, tc := range cases {
		if got := kongPaceConcurrencyAdjust(tc.peak, tc.limit, tc.recent, c); got != tc.want {
			t.Errorf("peak=%d limit=%d recent=%v: got %v want %v", tc.peak, tc.limit, tc.recent, got, tc.want)
		}
	}
}

func TestKongPaceWeight_CapOnlyTheGap(t *testing.T) {
	cfg := kongPaceTestConfig(t)
	pro, lite := cfg.Plans["pro"], cfg.Plans["prolite"]
	// 两个套餐都落后很多：上限只截进度差，让位量照样生效，比例保持 8:1。
	wPro := kongPaceWeight(70, pro, 0, 0, cfg)
	wLite := kongPaceWeight(70, lite, 0, 0, cfg)
	if math.Abs(wPro/wLite-8) > 1e-9 {
		t.Fatalf("yield must survive the cap: pro=%v prolite=%v", wPro, wLite)
	}
	// 追赶到上限且只剩 1 个槽的账号，与同套餐进度正常、槽位宽裕的账号相等。
	if a, b := kongPaceWeight(80, pro, 0, 30, cfg), kongPaceWeight(0, pro, 0, 0, cfg); math.Abs(a-b) > 1e-9 {
		t.Fatalf("free_1 == G must cancel the catch-up bonus: %v vs %v", a, b)
	}
}

func TestKongPacePeaks(t *testing.T) {
	var k kongPacePeaks
	k.byAccount = map[int64][]kongPacePeakBucket{}
	k.observe(1, 8, kongPaceT0)
	k.observe(1, 2, kongPaceT0.Add(30*time.Second))
	if got := k.peak(1, kongPaceT0.Add(4*time.Minute), 5*time.Minute); got != 8 {
		t.Fatalf("peak within window = %d, want 8", got)
	}
	if got := k.peak(1, kongPaceT0.Add(7*time.Minute), 5*time.Minute); got != 0 {
		t.Fatalf("peak must expire after the window, got %d", got)
	}
}

func TestKongPacePeaks_OutOfOrderAndRetention(t *testing.T) {
	var k kongPacePeaks
	k.byAccount = map[int64][]kongPacePeakBucket{}
	k.observe(1, 8, kongPaceT0)
	for i := 0; i < 200; i++ {
		k.observe(1, 0, kongPaceT0.Add(time.Duration(i%2)*time.Minute+time.Second))
	}
	if got := k.peak(1, kongPaceT0.Add(4*time.Minute), 5*time.Minute); got != 8 {
		t.Fatalf("interleaved minutes must not push the peak out, got %d", got)
	}
	if n := len(k.byAccount[1]); n != 2 {
		t.Fatalf("one bucket per minute expected, got %d", n)
	}

	var r kongPacePeaks
	r.byAccount = map[int64][]kongPacePeakBucket{}
	r.observe(1, 5, kongPaceT0)
	r.observe(1, 0, kongPaceT0.Add(60*time.Minute))
	if got := r.peak(1, kongPaceT0.Add(60*time.Minute), 60*time.Minute); got != 5 {
		t.Fatalf("the longest window must still see its first minute, got %d", got)
	}
	r.observe(1, 0, kongPaceT0.Add(61*time.Minute))
	if got := r.peak(1, kongPaceT0.Add(61*time.Minute), 60*time.Minute); got != 0 {
		t.Fatalf("minutes older than the longest window must be dropped, got %d", got)
	}
	r.forget([]int64{1})
	if _, ok := r.byAccount[1]; ok {
		t.Fatal("forgotten account must be removed")
	}
}

// --- 选号侧 ---

type kongPaceFakeRepo struct {
	mu        sync.Mutex
	rows      []KongPaceAccountRow
	listErr   error
	decisions []KongPaceDecisionRecord
}

func (r *kongPaceFakeRepo) ListOpenAIOAuthAccounts(context.Context) ([]KongPaceAccountRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]KongPaceAccountRow(nil), r.rows...), r.listErr
}

func (r *kongPaceFakeRepo) InsertDecisions(_ context.Context, recs []KongPaceDecisionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, recs...)
	return nil
}

func (r *kongPaceFakeRepo) DeleteDecisionsBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type kongPaceFakeStore struct {
	mu        sync.Mutex
	saved     map[int64][]byte
	published map[int64][]byte
	meta      []byte
	loadErr   error
}

func (s *kongPaceFakeStore) LoadAccountStates(context.Context) (map[int64][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64][]byte{}
	for k, v := range s.saved {
		out[k] = v
	}
	return out, s.loadErr
}

func (s *kongPaceFakeStore) SaveAccountStates(_ context.Context, states map[int64][]byte, deleted []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = map[int64][]byte{}
	}
	for k, v := range states {
		s.saved[k] = v
	}
	for _, id := range deleted {
		delete(s.saved, id)
	}
	return nil
}

func (s *kongPaceFakeStore) PublishState(_ context.Context, accounts map[int64][]byte, meta []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published, s.meta = accounts, meta
	return nil
}

func kongPaceTestComponent(t *testing.T, rows []KongPaceAccountRow, configYAML string) (*KongOpenAIAccountPace, *kongPaceFakeRepo, *kongPaceFakeStore) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, KongPaceConfigFileName)
	if configYAML != "" {
		if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo := &kongPaceFakeRepo{rows: rows}
	store := &kongPaceFakeStore{}
	p := NewKongOpenAIAccountPace(repo, store, nil, path)
	p.now = func() time.Time { return kongPaceT0 }
	return p, repo, store
}

func kongPaceAvailable(accs ...*Account) []accountWithLoad {
	out := make([]accountWithLoad, len(accs))
	for i, a := range accs {
		out[i] = accountWithLoad{account: a, loadInfo: &AccountLoadInfo{AccountID: a.ID}}
	}
	return out
}

func kongPaceOAuth(id int64, priority int) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Priority: priority, Concurrency: 8}
}

func TestKongPaceFactsTick_ConfigAbsentDoesNothing(t *testing.T) {
	p, _, store := kongPaceTestComponent(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 10, kongPaceT0.Add(time.Hour), kongPaceT0)}, "")
	p.factsTick()
	if p.active.Load() != nil || p.snapshot.Load() != nil || store.saved != nil || store.meta != nil {
		t.Fatal("without a config file nothing must be computed or written")
	}
	if d := (&OpenAIGatewayService{kongPace: p}).kongPaceBegin(context.Background(), nil, "h", "m", false); d != nil {
		t.Fatal("no decision without config")
	}
}

func TestKongPaceFactsTick_PersistsAndRestores(t *testing.T) {
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, kongPaceT0.Add(72*time.Hour), kongPaceT0.Add(-time.Minute))}
	p, repo, store := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	if p.snapshot.Load() == nil || len(store.saved) != 1 || len(store.published) != 1 || store.meta == nil {
		t.Fatalf("tick must snapshot, persist and publish: saved=%d published=%d", len(store.saved), len(store.published))
	}
	var meta map[string]any
	if err := json.Unmarshal(store.meta, &meta); err != nil || meta["config_version"] == "" {
		t.Fatalf("meta: %v %v", meta, err)
	}

	// 新进程从 Redis 恢复：之后的上涨照常记账。
	p2 := NewKongOpenAIAccountPace(repo, store, nil, p.config.path)
	later := kongPaceT0.Add(time.Minute)
	p2.now = func() time.Time { return later }
	repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 15, kongPaceT0.Add(72*time.Hour), later.Add(-10*time.Second))}
	p2.factsTick()
	f := p2.snapshot.Load().accounts[1]
	if got := kongPaceIncrease(&f.State, later, 6*time.Hour); got != 5 {
		t.Fatalf("restored state must continue accounting, inc6=%v", got)
	}
}

func TestKongPaceFactsTick_PublishUsesLastUsedAt(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	recent := kongPaceRow(1, "pro", 40, reset, kongPaceT0)
	recent.LastUsedAt = kongPaceTm(kongPaceT0.Add(-time.Minute))
	stale := kongPaceRow(2, "pro", 40, reset, kongPaceT0)
	stale.LastUsedAt = kongPaceTm(kongPaceT0.Add(-time.Hour))
	p, _, store := kongPaceTestComponent(t, []KongPaceAccountRow{recent, stale}, kongPaceTestYAML)
	p.factsTick()
	c := func(id int64) float64 {
		var v kongPaceAccountView
		if err := json.Unmarshal(store.published[id], &v); err != nil {
			t.Fatal(err)
		}
		return v.C
	}
	if c(1) != 0 || c(2) != -10 {
		t.Fatalf("idle bonus must respect the last use: recent=%v stale=%v", c(1), c(2))
	}
}

func TestKongPaceFactsTick_ListErrorKeepsSnapshot(t *testing.T) {
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, kongPaceT0.Add(72*time.Hour), kongPaceT0)}
	p, repo, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	first := p.snapshot.Load()
	repo.listErr = errors.New("db down")
	p.factsTick()
	if p.snapshot.Load() != first {
		t.Fatal("a failed read must keep the previous snapshot")
	}
}

func TestKongPaceReorder_TiersSegmentsAndProbability(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	rows := []KongPaceAccountRow{
		kongPaceRow(1, "pro", 40, reset, kongPaceT0),
		kongPaceRow(2, "pro", 40, reset, kongPaceT0),
		kongPaceRow(3, "pro", 40, reset, kongPaceT0),
	}
	p, _, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	// 账号 2 近 6h 已用 50%，到线。
	snap := p.snapshot.Load()
	f := snap.accounts[2]
	f.State.Rises = []kongPaceRise{{At: kongPaceT0.Add(-time.Hour), Points: 50}}
	snap.accounts[2] = f
	p.rand = func() float64 { return 0 }

	svc := &OpenAIGatewayService{kongPace: p}
	d := svc.kongPaceBegin(context.Background(), nil, "h", "gpt", false)
	apiKey := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 20, Concurrency: 5}
	in := kongPaceAvailable(kongPaceOAuth(2, 10), kongPaceOAuth(1, 10), kongPaceOAuth(3, 10), apiKey, kongPaceOAuth(4, 20))
	out := d.reorder(in)

	var ids []int64
	for _, item := range out {
		ids = append(ids, item.account.ID)
	}
	if ids[2] != 2 {
		t.Fatalf("at-line account must go behind its segment: %v", ids)
	}
	if ids[3] != 9 || ids[4] != 4 {
		t.Fatalf("mixed segment must keep its original order and priorities must not mix: %v", ids)
	}
	views := d.rounds[0].Candidates
	var probSum float64
	for _, v := range views {
		if v.AccountID == 2 && v.FirstChoice != 0 {
			t.Fatalf("at-line account must have zero first-choice probability when alternatives exist")
		}
		if v.Priority == 10 {
			probSum += v.FirstChoice
		}
	}
	if math.Abs(probSum-1) > 1e-9 {
		t.Fatalf("first-choice probabilities in the segment must sum to 1, got %v", probSum)
	}
}

func TestKongPaceReorder_NotAppliedKeepsOrderButRecords(t *testing.T) {
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 90, kongPaceT0.Add(72*time.Hour), kongPaceT0), kongPaceRow(2, "pro", 0, kongPaceT0.Add(72*time.Hour), kongPaceT0)}
	p, repo, _ := kongPaceTestComponent(t, rows, strings.Replace(kongPaceTestYAML, "enabled: true\ngap", "enabled: false\ngap", 1))
	p.factsTick()
	p.rand = func() float64 { return 0.999 }
	svc := &OpenAIGatewayService{kongPace: p}
	d := svc.kongPaceBegin(context.Background(), nil, "", "gpt", false)
	in := kongPaceAvailable(kongPaceOAuth(1, 10), kongPaceOAuth(2, 10))
	out := d.reorder(in)
	if out[0].account.ID != 1 || len(d.rounds) != 1 {
		t.Fatalf("disabled reordering must keep the original order but record the round")
	}
	d.finalOrder(out)
	d.acquired(1)
	d.finish()
	close(p.stop)
	p.wg.Add(1)
	p.runDecisionWriter()
	if len(repo.decisions) != 1 {
		t.Fatalf("decision must be written, got %d", len(repo.decisions))
	}
	rec := repo.decisions[0]
	if rec.Applied || rec.NotAppliedReason != "disabled" || rec.Reason != "no_session" || rec.Outcome != "acquired" || rec.AccountID != 1 || rec.AttemptIndex != 0 {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestKongPaceDecision_AppliedOnlyWhenASegmentIsPaced(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	p, _, _ := kongPaceTestComponent(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 40, reset, kongPaceT0), kongPaceRow(2, "pro", 40, reset, kongPaceT0)}, kongPaceTestYAML)
	p.factsTick()
	svc := &OpenAIGatewayService{kongPace: p}
	apiKey := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 10, Concurrency: 5}

	d := svc.kongPaceBegin(context.Background(), nil, "h", "gpt", false)
	mixed := kongPaceAvailable(kongPaceOAuth(1, 10), apiKey)
	if out := d.reorder(mixed); out[0].account.ID != 1 || out[1].account.ID != 9 {
		t.Fatal("mixed segment must keep its order")
	}
	d.finalOrder(mixed)
	d.acquired(1)
	d.finish()
	rec := <-p.decisions
	if rec.Applied || rec.NotAppliedReason != "mixed_segment" {
		t.Fatalf("only mixed segments: %+v", rec)
	}

	// 第一轮只有混合段、第二轮有可排的段：算实际重排。
	d = svc.kongPaceBegin(context.Background(), nil, "h", "gpt", false)
	d.reorder(mixed)
	single := kongPaceAvailable(kongPaceOAuth(2, 10))
	d.reorder(single)
	if v := d.rounds[1].Candidates[0]; !v.Paced || v.FirstChoice != 1 {
		t.Fatalf("a single OAuth candidate is a paced segment with certain first choice: %+v", v)
	}
	d.finalOrder(single)
	d.acquired(2)
	d.finish()
	rec = <-p.decisions
	if !rec.Applied || rec.NotAppliedReason != "" {
		t.Fatalf("second round paced: %+v", rec)
	}
}

func TestKongPaceDecision_OutcomesAndDrops(t *testing.T) {
	p, _, _ := kongPaceTestComponent(t, nil, kongPaceTestYAML)
	p.factsTick()
	svc := &OpenAIGatewayService{kongPace: p}

	d := svc.kongPaceBegin(context.Background(), nil, "h", "gpt", true)
	d.finish()
	rec := <-p.decisions
	if rec.Outcome != "no_available" || rec.Applied || rec.NotAppliedReason != "no_available" || rec.Reason != "sticky_spillover" {
		t.Fatalf("no candidates: %+v", rec)
	}

	d = svc.kongPaceBegin(context.Background(), nil, "h", "gpt", false)
	d.loadFailed()
	d.acquired(7)
	d.finish()
	rec = <-p.decisions
	if rec.Outcome != "acquired" || rec.Applied || rec.NotAppliedReason != "load_error" {
		t.Fatalf("load error fallback: %+v", rec)
	}

	for i := 0; i < kongPaceDecisionQueueCap+3; i++ {
		p.enqueueDecision(KongPaceDecisionRecord{})
	}
	if got := p.counters.decisionsDropped.Load(); got != 3 {
		t.Fatalf("full queue must drop and count, got %d", got)
	}
}

func TestKongPaceNilSafe(t *testing.T) {
	var d *kongPaceDecision
	in := kongPaceAvailable(kongPaceOAuth(1, 10))
	if out := d.reorder(in); len(out) != 1 {
		t.Fatal("nil decision must return input")
	}
	d.finalOrder(in)
	d.acquired(1)
	d.loadFailed()
	d.finish()
	if (&OpenAIGatewayService{}).kongPaceBegin(context.Background(), nil, "", "", false) != nil {
		t.Fatal("unwired service must not create decisions")
	}
}
