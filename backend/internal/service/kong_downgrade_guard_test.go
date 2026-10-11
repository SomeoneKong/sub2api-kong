//go:build unit

package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 降档哨兵的判定与状态机：仓储、上游与指纹测试都是假的。假仓储按迁移 910 的语义执行带条件的写入
// （哨兵写入时 schedulable 变化给版本加一；人工改开关时版本加一并删掉持有记录）。

var kongGuardT0 = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

type kongGuardFakeAccount struct {
	acc   Account
	group bool
}

type kongGuardFakeStore struct {
	mu        sync.Mutex
	accounts  map[int64]*kongGuardFakeAccount
	events    []KongGuardEvent
	usage     []KongGuardUsageRow
	impacts   map[string]map[string]any
	impactRow []KongGuardImpactRow
	nextEvent int64
	failKind  map[string]bool // InsertEvent 对这些种类返回错误
	failHold  bool
	// 提交了但返回错误（响应丢失）：Hold / ReleaseFalseAlarm 照常生效，然后报错
	lostHold    bool
	lostRelease bool
	failRecent  int // RecentEvents 接下来失败的次数
	failScan    int // ScanUsage 接下来失败的次数
}

func newKongGuardFakeStore() *kongGuardFakeStore {
	return &kongGuardFakeStore{accounts: map[int64]*kongGuardFakeAccount{}, impacts: map[string]map[string]any{}, failKind: map[string]bool{}}
}

func (s *kongGuardFakeStore) addAccount(id int64, inGroup bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id] = &kongGuardFakeAccount{acc: Account{
		ID: id, Name: "acct", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, Extra: map[string]any{},
	}}
	s.setGroup(id, inGroup)
}

// setGroup 调整账号是否在服务分组（10）。调用方持锁或在单协程里调用。
func (s *kongGuardFakeStore) setGroup(id int64, inGroup bool) {
	fa := s.accounts[id]
	fa.group = inGroup
	fa.acc.GroupIDs = nil
	if inGroup {
		fa.acc.GroupIDs = []int64{10}
	}
}

// cloneAccount 模拟从库里读一次：extra 经过一次 JSON 往返。
func kongGuardCloneAccount(a Account) *Account {
	out := a
	b, _ := json.Marshal(a.Extra)
	out.Extra = map[string]any{}
	_ = json.Unmarshal(b, &out.Extra)
	return &out
}

func (s *kongGuardFakeStore) rev(id int64) int64 { return kongGuardRevOf(s.accounts[id].acc.Extra) }

func (s *kongGuardFakeStore) hold(id int64) *KongGuardHold {
	s.mu.Lock()
	defer s.mu.Unlock()
	return kongGuardHoldFromExtra(kongGuardCloneAccount(s.accounts[id].acc).Extra)
}

func (s *kongGuardFakeStore) schedulable(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[id].acc.Schedulable
}

// humanSet 模拟人用"可调度"开关：显式操作，版本加一并删掉持有记录（schedulable 变化时触发器再加一）。
func (s *kongGuardFakeStore) humanSet(id int64, schedulable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &s.accounts[id].acc
	rev := kongGuardRevOf(a.Extra) + 1
	if a.Schedulable != schedulable {
		rev++
	}
	delete(a.Extra, "kong_downgrade_guard_hold")
	a.Extra["kong_schedulable_rev"] = float64(rev)
	a.Schedulable = schedulable
}

func (s *kongGuardFakeStore) kinds(accountID int64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ev := range s.events {
		if ev.AccountID == accountID {
			out = append(out, ev.Kind)
		}
	}
	return out
}

func (s *kongGuardFakeStore) eventsOf(kind string) []KongGuardEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KongGuardEvent
	for _, ev := range s.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (s *kongGuardFakeStore) insertLocked(ev KongGuardEvent) {
	s.nextEvent++
	ev.ID = s.nextEvent
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = kongGuardT0
	}
	b, _ := json.Marshal(ev.Detail)
	ev.Detail = map[string]any{}
	_ = json.Unmarshal(b, &ev.Detail)
	s.events = append(s.events, ev)
}

func (s *kongGuardFakeStore) InsertEvent(_ context.Context, ev KongGuardEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKind[ev.Kind] {
		return errors.New("db down")
	}
	s.insertLocked(ev)
	return nil
}

func (s *kongGuardFakeStore) ScanUsage(_ context.Context, since time.Time, accountIDs []int64, limit int) ([]KongGuardUsageRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failScan > 0 {
		s.failScan--
		return nil, errors.New("db down")
	}
	watched := map[int64]bool{}
	for _, id := range accountIDs {
		watched[id] = true
	}
	var out []KongGuardUsageRow
	for _, r := range s.usage {
		if !r.CreatedAt.Before(since) && watched[r.AccountID] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *kongGuardFakeStore) Hold(_ context.Context, in KongGuardHoldInput) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failHold {
		return false, errors.New("db down")
	}
	fa := s.accounts[in.AccountID]
	if fa == nil || !fa.acc.Schedulable || kongGuardHoldFromExtra(fa.acc.Extra) != nil || s.rev(in.AccountID) != in.ExpectedRev || !fa.group {
		return false, nil
	}
	b, _ := json.Marshal(in.Hold)
	var h map[string]any
	_ = json.Unmarshal(b, &h)
	fa.acc.Schedulable = false
	fa.acc.Extra["kong_downgrade_guard_hold"] = h
	fa.acc.Extra["kong_schedulable_rev"] = float64(in.ExpectedRev + 1)
	s.insertLocked(in.Event)
	if s.lostHold {
		return false, errors.New("commit: connection reset")
	}
	return true, nil
}

func (s *kongGuardFakeStore) UpdateHold(_ context.Context, accountID int64, op string, expectedRev int64, patch map[string]any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fa := s.accounts[accountID]
	h, _ := fa.acc.Extra["kong_downgrade_guard_hold"].(map[string]any)
	if h == nil || h["op"] != op || s.rev(accountID) != expectedRev {
		return false, nil
	}
	b, _ := json.Marshal(patch)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	for k, v := range p {
		h[k] = v
	}
	return true, nil
}

func (s *kongGuardFakeStore) ReleaseFalseAlarm(_ context.Context, accountID int64, op string, expectedRev int64, ev KongGuardEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKind[ev.Kind] {
		return false, errors.New("db down")
	}
	fa := s.accounts[accountID]
	h, _ := fa.acc.Extra["kong_downgrade_guard_hold"].(map[string]any)
	if h == nil || h["op"] != op || h["stage"] != KongGuardStageSuspect || s.rev(accountID) != expectedRev ||
		fa.acc.Status != StatusActive || (fa.acc.AutoPauseOnExpired && fa.acc.ExpiresAt != nil && !fa.acc.ExpiresAt.After(kongGuardT0)) {
		return false, nil
	}
	fa.acc.Schedulable = true
	delete(fa.acc.Extra, "kong_downgrade_guard_hold")
	fa.acc.Extra["kong_schedulable_rev"] = float64(expectedRev + 1)
	s.insertLocked(ev)
	if s.lostRelease {
		return false, errors.New("commit: connection reset")
	}
	return true, nil
}

func (s *kongGuardFakeStore) TakeOverExpired(_ context.Context) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id, fa := range s.accounts {
		if _, ok := fa.acc.Extra["kong_downgrade_guard_hold"]; ok && fa.acc.AutoPauseOnExpired && fa.acc.ExpiresAt != nil && !fa.acc.ExpiresAt.After(kongGuardT0) {
			delete(fa.acc.Extra, "kong_downgrade_guard_hold")
			fa.acc.Extra["kong_schedulable_rev"] = float64(s.rev(id) + 1)
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *kongGuardFakeStore) RecentEvents(_ context.Context, kinds []string, since time.Time) ([]KongGuardEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRecent > 0 {
		s.failRecent--
		return nil, errors.New("db down")
	}
	var out []KongGuardEvent
	for _, ev := range s.events {
		for _, k := range kinds {
			if ev.Kind == k && !ev.CreatedAt.Before(since) {
				out = append(out, ev)
			}
		}
	}
	return out, nil
}

func (s *kongGuardFakeStore) ListEvents(_ context.Context, accountID int64, limit int) ([]KongGuardEvent, error) {
	return nil, nil
}

func (s *kongGuardFakeStore) PendingImpacts(_ context.Context, minAge time.Duration) ([]KongGuardEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []KongGuardEvent
	for _, ev := range s.events {
		if ev.Kind != KongGuardEventHold {
			continue
		}
		if d := s.impacts[ev.Op]; d != nil && d["finalized"] == true {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func (s *kongGuardFakeStore) ImpactRows(_ context.Context, accountID int64, from, holdAt, cutoff time.Time) ([]KongGuardImpactRow, error) {
	return s.impactRow, nil
}

func (s *kongGuardFakeStore) UpsertImpact(_ context.Context, accountID int64, op string, detail map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.impacts[op]; d != nil && d["finalized"] == true {
		return nil
	}
	s.impacts[op] = detail
	return nil
}

func (s *kongGuardFakeStore) DeleteEventsBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	return 0, nil
}

func (s *kongGuardFakeStore) GetByID(_ context.Context, id int64) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fa := s.accounts[id]
	if fa == nil {
		return nil, ErrAccountNotFound
	}
	return kongGuardCloneAccount(fa.acc), nil
}

func (s *kongGuardFakeStore) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Account
	for _, fa := range s.accounts {
		out = append(out, *kongGuardCloneAccount(fa.acc))
	}
	return out, nil
}

func (s *kongGuardFakeStore) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Account
	for _, fa := range s.accounts {
		if fa.group {
			out = append(out, *kongGuardCloneAccount(fa.acc))
		}
	}
	return out, nil
}

// kongGuardFakeProber 按账号给出探测结论：refuse 为真时第二次请求也下发新票。
type kongGuardFakeProber struct {
	mu     sync.Mutex
	refuse map[int64]bool
	noTick map[int64]bool
	fail   map[int64]error
	calls  int
}

func (p *kongGuardFakeProber) ResolveProxyURL(context.Context, *int64) (string, error) {
	return "", nil
}

func (p *kongGuardFakeProber) RunTicketProbe(_ context.Context, account *Account, _, _, state string) (KongTicketProbeAnswer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if err := p.fail[account.ID]; err != nil && state != "" {
		return KongTicketProbeAnswer{StatusCode: 429}, err
	}
	if p.noTick[account.ID] {
		return KongTicketProbeAnswer{StatusCode: 200}, nil
	}
	if state == "" || p.refuse[account.ID] {
		return KongTicketProbeAnswer{StatusCode: 200, Issued: "ticket-new"}, nil
	}
	return KongTicketProbeAnswer{StatusCode: 200}, nil
}

type kongGuardFakeFP struct {
	mu      sync.Mutex
	decided map[int64]string // 账号 → 指纹结论指向的模型；空串表示未完成
	err     error            // Begin 返回的错误（例如有人正在测）
	runs    []int64
	aborts  int
}

func (f *kongGuardFakeFP) Begin(_ context.Context, accountID int64, _ string) (kongGuardFPRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kongGuardFakeRun{f: f, id: accountID}, nil
}

type kongGuardFakeRun struct {
	f  *kongGuardFakeFP
	id int64
}

func (r *kongGuardFakeRun) Abort() {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.f.aborts++
}

func (r *kongGuardFakeRun) Run(context.Context) (*KongFingerprintTestResult, string, error) {
	f, accountID := r.f, r.id
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, accountID)
	d := f.decided[accountID]
	if d == "" {
		return &KongFingerprintTestResult{Execution: "failed"}, "t-x", nil
	}
	return &KongFingerprintTestResult{Execution: KongFingerprintExecCompleted, Decided: d, Parts: 1}, "t-1", nil
}

type kongGuardHarness struct {
	g     *KongDowngradeGuard
	store *kongGuardFakeStore
	probe *kongGuardFakeProber
	fp    *kongGuardFakeFP
	now   time.Time
	ops   int
	syncs []int64
}

func newKongGuardHarness(t *testing.T, mode string) *kongGuardHarness {
	t.Helper()
	h := &kongGuardHarness{store: newKongGuardFakeStore(), probe: &kongGuardFakeProber{refuse: map[int64]bool{}, noTick: map[int64]bool{}, fail: map[int64]error{}},
		fp: &kongGuardFakeFP{decided: map[int64]string{}}, now: kongGuardT0}
	cfg := KongDowngradeGuardConfig{Mode: mode, GroupID: 10, FreshAge: 3000 * time.Second, ProbeBusy: 10 * time.Minute,
		ProbeIdle: 60 * time.Minute, ProbeModel: "gpt-6.1-sol", TicketHoldMax: 2}
	h.g = newKongDowngradeGuard(cfg, h.store, h.store, h.probe, h.fp, func(id int64) { h.syncs = append(h.syncs, id) })
	h.g.now = func() time.Time { return h.now }
	h.g.probeGap = 0
	h.g.restored = true // 需要走恢复流程的用例自己置回 false
	h.g.newOp = func() string { h.ops++; return "op-" + string(rune('0'+h.ops)) }
	t.Cleanup(h.g.cancel)
	return h
}

// runJobs 同步执行排队的任务并交回主循环，直到队列空。
func (h *kongGuardHarness) runJobs() {
	for {
		select {
		case job := <-h.g.jobs:
			h.g.handleResult(h.g.run(job))
		default:
			return
		}
	}
}

// tick 跑一轮主循环并执行它派出的任务。
func (h *kongGuardHarness) tick() {
	h.g.tick()
	h.runJobs()
}

// usagePair 写一对用量行：第一行签发票，第二行在 gap 之后带回这张票；reissue 为真时第二行又下发了新票。
func (h *kongGuardHarness) usagePair(id, account int64, at time.Time, gap time.Duration, reissue bool, ticket string) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.usage = append(h.store.usage, KongGuardUsageRow{ID: id, CreatedAt: at, AccountID: account, GroupID: 10, Model: "gpt-6.1-sol",
		DurationMs: 1000, OutputTokens: 10, ReissuedFP: ticket})
	second := KongGuardUsageRow{ID: id + 1, CreatedAt: at.Add(gap), AccountID: account, GroupID: 10, Model: "gpt-6.1-sol",
		DurationMs: 1000, OutputTokens: 10, StateFP: ticket}
	if reissue {
		second.ReissuedFP = ticket + "-re"
	}
	h.store.usage = append(h.store.usage, second)
}

func TestKongGuardJudgeBoundaries(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	g := h.g
	at := kongGuardT0
	g.snapshot = map[int64]*Account{1: {ID: 1}, 2: {ID: 2}}
	g.noteIssued(KongGuardUsageRow{AccountID: 1, Model: "gpt-6.1-sol", CreatedAt: at.Add(time.Second), DurationMs: 1000, ReissuedFP: "a"})
	row := func(acct int64, model string, startAfter time.Duration, reissued string) KongGuardUsageRow {
		return KongGuardUsageRow{AccountID: acct, Model: model, CreatedAt: at.Add(startAfter + time.Second), DurationMs: 1000, StateFP: "a", ReissuedFP: reissued}
	}
	cases := []struct {
		name string
		row  KongGuardUsageRow
		want string
	}{
		{"接受", row(1, "gpt-6.1-sol", time.Minute, ""), kongGuardProbeAccept},
		{"拒绝", row(1, "gpt-6.1-sol", time.Minute, "b"), kongGuardProbeRefuse},
		{"正好 3000 秒", row(1, "gpt-6.1-sol", 3000*time.Second, "b"), kongGuardProbeRefuse},
		{"超过 3000 秒", row(1, "gpt-6.1-sol", 3001*time.Second, "b"), ""},
		{"不同模型", row(1, "gpt-6-astra", time.Minute, "b"), ""},
		{"别的账号的票", row(2, "gpt-6.1-sol", time.Minute, "b"), ""},
		{"票龄为负", row(1, "gpt-6.1-sol", -time.Minute, "b"), ""},
		{"没见过的票", KongGuardUsageRow{AccountID: 1, Model: "gpt-6.1-sol", CreatedAt: at.Add(time.Minute), StateFP: "zz", ReissuedFP: "b"}, ""},
		{"没有回带票", KongGuardUsageRow{AccountID: 1, Model: "gpt-6.1-sol", CreatedAt: at.Add(time.Minute), ReissuedFP: "b"}, ""},
	}
	for _, c := range cases {
		if got := g.judge(c.row); got != c.want {
			t.Errorf("%s: judge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKongGuardScanLateRowsOnce(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.addAccount(1, true)
	h.usagePair(100, 1, kongGuardT0.Add(-2*time.Minute), time.Minute, false, "a")
	h.g.refreshSnapshot()
	h.g.scan(h.now)
	if got := len(h.g.seen); got != 2 {
		t.Fatalf("seen = %d, want 2", got)
	}
	// 较小的 id 晚提交：created_at 早于游标，仍在回扫范围内。
	h.store.mu.Lock()
	h.store.usage = append(h.store.usage, KongGuardUsageRow{ID: 50, CreatedAt: kongGuardT0.Add(-90 * time.Second), AccountID: 1, Model: "gpt-6.1-sol",
		DurationMs: 1000, StateFP: "a", ReissuedFP: "c"})
	h.store.mu.Unlock()
	h.now = h.now.Add(30 * time.Second)
	refusals := h.g.scan(h.now)
	if _, ok := refusals[1]; !ok {
		t.Fatalf("迟到的拒绝行没有被处理")
	}
	if refusals := h.g.scan(h.now); len(refusals) != 0 {
		t.Fatalf("同一行被处理了两次: %v", refusals)
	}
	// 之后很久没有新行：游标不动，回扫窗口里的行也不能被重判。
	for i := 0; i < 3; i++ {
		h.now = h.now.Add(20 * time.Minute)
		if refusals := h.g.scan(h.now); len(refusals) != 0 {
			t.Fatalf("没有新行时重判了旧行: %v", refusals)
		}
	}
}

func TestKongGuardStartDoesNotRejudgeOldRows(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	// 20 分钟前的拒绝：启动时只用来重建票表，不判定。
	h.usagePair(100, 1, kongGuardT0.Add(-20*time.Minute), time.Minute, true, "a")
	h.g.lastMaint = h.now
	h.tick()
	if len(h.store.eventsOf(KongGuardEventPassiveRefuse)) != 0 {
		t.Fatalf("启动后重判了 10 分钟以前的行")
	}
	if _, ok := h.g.issued["a"]; !ok {
		t.Fatalf("票表没有从回读重建")
	}
}

// 被动拒绝 → 探测拒绝 → 凭票关闭（待确认）→ 指纹 luna → 确认降档，之后保持关闭。
func TestKongGuardRefuseHoldConfirm(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.usagePair(100, 1, kongGuardT0.Add(-time.Minute), 20*time.Second, true, "a")
	h.probe.refuse[1] = true
	h.fp.decided[1] = "gpt-5.6-luna"
	h.g.lastMaint = h.now
	h.g.acct(2).lastVerdict, h.g.acct(2).lastProbe = h.now, h.now
	h.tick()

	hold := h.store.hold(1)
	if hold == nil || hold.Stage != KongGuardStageConfirmed || h.store.schedulable(1) {
		t.Fatalf("hold = %+v, schedulable = %v；want 确认降档且关闭", hold, h.store.schedulable(1))
	}
	if hold.LastGoodAt != nil {
		t.Fatalf("没有正常证据时 last_good_at 应为空: %v", hold.LastGoodAt)
	}
	holds := h.store.eventsOf(KongGuardEventHold)
	if len(holds) != 1 || holds[0].Detail["window_unknown"] != true {
		t.Fatalf("hold 事件 = %+v", holds)
	}
	if len(h.syncs) == 0 || h.syncs[0] != 1 {
		t.Fatalf("关闭后没有刷新调度快照: %v", h.syncs)
	}
	// 确认之后不再探测、不再测指纹。
	runs := len(h.fp.runs)
	h.now = h.now.Add(2 * time.Hour)
	h.tick()
	if len(h.fp.runs) != runs || h.store.schedulable(1) {
		t.Fatalf("确认降档后哨兵又动了账号")
	}
}

// 指纹正常 → 误报撤销并进入 24 小时冷却；冷却期内再拒绝只起检测任务，检测确认 luna 仍会关闭。
func TestKongGuardFalseAlarmCooldown(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	h.g.acct(2).lastOutput = h.now
	h.g.acct(2).lastVerdict, h.g.acct(2).lastProbe = h.now, h.now
	h.probe.refuse[1] = true
	h.fp.decided[1] = "gpt-6.1-sol"
	h.usagePair(100, 1, kongGuardT0.Add(-time.Minute), 20*time.Second, true, "a")
	h.tick()

	if h.store.hold(1) != nil || !h.store.schedulable(1) {
		t.Fatalf("误报没有撤销")
	}
	fa := h.store.eventsOf(KongGuardEventFalseAlarm)
	if len(fa) != 1 {
		t.Fatalf("false_alarm 事件 = %d", len(fa))
	}

	// 冷却期内：再次拒绝，不凭票关闭，起检测任务；检测任务正常就保持可调度。
	h.now = h.now.Add(10 * time.Minute)
	h.g.acct(2).lastOutput = h.now
	h.usagePair(200, 1, h.now.Add(-time.Minute), 20*time.Second, true, "b")
	h.tick()
	if h.store.hold(1) != nil || !h.store.schedulable(1) {
		t.Fatalf("冷却期内凭票关闭了")
	}
	skipped := h.store.eventsOf(KongGuardEventHoldSkipped)
	if len(skipped) == 0 || skipped[len(skipped)-1].Detail["reason"] != "cooldown" {
		t.Fatalf("hold_skipped = %+v", skipped)
	}
	runs := len(h.fp.runs)

	// 2 小时内不重复检测。
	h.now = h.now.Add(30 * time.Minute)
	h.g.acct(1).lastProbe = time.Time{}
	h.usagePair(300, 1, h.now.Add(-time.Minute), 20*time.Second, true, "c")
	h.tick()
	if len(h.fp.runs) != runs {
		t.Fatalf("冷却期的检测没有限频")
	}

	// 2 小时后检测确认 luna：直接确认降档。
	h.now = h.now.Add(2 * time.Hour)
	h.fp.decided[1] = "gpt-6-luna"
	h.g.acct(1).lastProbe = time.Time{}
	h.usagePair(400, 1, h.now.Add(-time.Minute), 20*time.Second, true, "d")
	h.tick()
	if hold := h.store.hold(1); hold == nil || hold.Stage != KongGuardStageConfirmed || h.store.schedulable(1) {
		t.Fatalf("冷却期内检测确认 luna 没有关闭: %+v", hold)
	}
}

// 冷却与检测限频在重启后从事件恢复。
func TestKongGuardRestoreCooldown(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	until := h.now.Add(20 * time.Hour)
	h.store.events = []KongGuardEvent{
		{AccountID: 1, Kind: KongGuardEventFalseAlarm, CreatedAt: h.now.Add(-4 * time.Hour), Detail: map[string]any{"cooldown_until": until.Format(time.RFC3339Nano)}},
		{AccountID: 1, Kind: KongGuardEventFPStart, CreatedAt: h.now.Add(-time.Hour), Detail: map[string]any{"purpose": kongGuardPurposeDetect}},
	}
	h.g.restore()
	a := h.g.acct(1)
	if !a.cooldown.Equal(until) {
		t.Fatalf("cooldown = %v, want %v", a.cooldown, until)
	}
	if !a.fpStart[kongGuardPurposeDetect].Equal(h.now.Add(-time.Hour)) {
		t.Fatalf("fpStart = %v", a.fpStart)
	}
}

// 名额：已有待确认的账号达到上限时不凭票关闭，改起检测任务。
func TestKongGuardQuota(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.g.cfg.TicketHoldMax = 1
	for id := int64(1); id <= 3; id++ {
		h.store.addAccount(id, true)
	}
	h.store.accounts[3].acc.Extra["kong_downgrade_guard_hold"] = map[string]any{"op": "old", "stage": KongGuardStageSuspect,
		"next_fp_at": h.now.Add(time.Hour).Format(time.RFC3339Nano)}
	h.store.accounts[3].acc.Schedulable = false
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if h.store.hold(1) != nil {
		t.Fatalf("名额已满仍凭票关闭")
	}
	skipped := h.store.eventsOf(KongGuardEventHoldSkipped)
	if len(skipped) != 1 || skipped[0].Detail["reason"] != "quota" {
		t.Fatalf("hold_skipped = %+v", skipped)
	}
	if len(h.fp.runs) != 1 {
		t.Fatalf("名额已满时没有起检测任务: %v", h.fp.runs)
	}
}

// 容量：其他组员近 10 分钟没有在探测模型上输出、或此刻不可调度、或带持有记录时都不算容量。
func TestKongGuardCapacity(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	for id := int64(1); id <= 4; id++ {
		h.store.addAccount(id, true)
	}
	until := time.Now().Add(time.Hour) // IsSchedulable 按真实时钟判断
	h.store.accounts[2].acc.RateLimitResetAt = &until
	h.store.accounts[3].acc.Extra["kong_downgrade_guard_hold"] = map[string]any{"op": "x", "stage": KongGuardStageConfirmed}
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.g.acct(3).lastOutput = h.now
	h.g.acct(4).lastOutput = h.now.Add(-11 * time.Minute)
	if h.g.hasCapacity(1, h.now) {
		t.Fatalf("没有可用的其他组员却判为有容量")
	}
	h.g.acct(4).lastOutput = h.now.Add(-5 * time.Minute)
	if !h.g.hasCapacity(1, h.now) {
		t.Fatalf("组员 4 近期在产出且可调度，应有容量")
	}
}

// 输出为 0 的行、不在服务分组的行、其他模型的行都不更新"最近输出"。
func TestKongGuardLastOutputOnlyCountsServingRows(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.store.usage = []KongGuardUsageRow{
		{ID: 1, CreatedAt: h.now.Add(-time.Minute), AccountID: 2, GroupID: 10, Model: "gpt-6.1-sol", OutputTokens: 0},
		{ID: 2, CreatedAt: h.now.Add(-time.Minute), AccountID: 2, GroupID: 11, Model: "gpt-6.1-sol", OutputTokens: 5},
		{ID: 3, CreatedAt: h.now.Add(-time.Minute), AccountID: 2, GroupID: 10, Model: "gpt-6-astra", OutputTokens: 5},
	}
	h.g.cursor = h.now.Add(-10 * time.Minute)
	h.g.scan(h.now)
	if !h.g.acct(2).lastOutput.IsZero() {
		t.Fatalf("lastOutput = %v, want zero", h.g.acct(2).lastOutput)
	}
}

// 确认任务进行中人动了开关：旧结论（正常）不得重新打开账号，扫描记一条 human_release。
func TestKongGuardHumanToggleVoidsOldResult(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.g.decideHold(1, 0, h.now)
	// 取出确认任务，先让人"再次设 false"，再交回结论。
	job := <-h.g.jobs
	h.store.humanSet(1, false)
	h.fp.decided[1] = "gpt-6.1-sol"
	h.g.handleResult(h.g.run(job))
	if h.store.schedulable(1) {
		t.Fatalf("人工设 false 之后旧的正常结论重新打开了账号")
	}
	if len(h.store.eventsOf(KongGuardEventFalseAlarm)) != 0 {
		t.Fatalf("人已撤销所有权时不应产生冷却记录")
	}
	h.g.lastMaint = h.now
	h.tick()
	rel := h.store.eventsOf(KongGuardEventHumanRelease)
	if len(rel) != 1 || rel[0].Op != "op-1" {
		t.Fatalf("human_release = %+v", rel)
	}
}

// 检测任务进行中人动了开关：旧的 luna 结论不关闭账号。
func TestKongGuardDetectVoidedByVersion(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.g.refreshSnapshot()
	h.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	job := <-h.g.jobs
	h.store.humanSet(1, true)
	h.fp.decided[1] = "gpt-5.6-luna"
	h.g.handleResult(h.g.run(job))
	if h.store.hold(1) != nil || !h.store.schedulable(1) {
		t.Fatalf("版本已变，旧的 luna 结论不应关闭账号")
	}
}

// 未确认：保持关闭、10 分钟后重测；不到时刻不重测；第二次未确认仍保持关闭。
func TestKongGuardInconclusiveRetest(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	h.g.acct(2).lastOutput = h.now
	h.g.acct(2).lastVerdict, h.g.acct(2).lastProbe = h.now, h.now
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	hold := h.store.hold(1)
	if hold == nil || hold.Stage != KongGuardStageSuspect || hold.FPAttempts != 1 || hold.NextFPAt == nil || !hold.NextFPAt.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("hold = %+v", hold)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.tick()
	if len(h.fp.runs) != 1 {
		t.Fatalf("没到重测时刻就重测了: %v", h.fp.runs)
	}
	h.now = h.now.Add(6 * time.Minute)
	h.tick()
	if hold := h.store.hold(1); len(h.fp.runs) != 2 || hold == nil || hold.FPAttempts != 2 || h.store.schedulable(1) {
		t.Fatalf("重测后 hold = %+v, runs = %v", hold, h.fp.runs)
	}
}

// fp_start 写不进就不发起测试，也不算开始。
func TestKongGuardFPStartWriteFailure(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.g.refreshSnapshot()
	h.store.failKind[KongGuardEventFPStart] = true
	h.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h.runJobs()
	if len(h.fp.runs) != 0 || h.fp.aborts != 1 || !h.g.acct(1).fpStart[kongGuardPurposeDetect].IsZero() {
		t.Fatalf("fp_start 写失败仍发起了测试，或没有放弃执行权: runs=%v aborts=%d", h.fp.runs, h.fp.aborts)
	}
	if len(h.store.eventsOf(KongGuardEventFingerprint)) != 0 {
		t.Fatalf("没开始的测试不应记 fingerprint 事件")
	}
}

// 有人正在测同一个账号：不算一次确认。
func TestKongGuardFingerprintBusyNotCounted(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.fp.err = ErrKongFingerprintBusy
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if hold := h.store.hold(1); hold == nil || hold.FPAttempts != 0 {
		t.Fatalf("测试忙时不应计入确认次数: %+v", hold)
	}
	if n := len(h.store.eventsOf(KongGuardEventFPStart)); n != 0 {
		t.Fatalf("没取得执行权却写了 fp_start: %d", n)
	}
}

// 误报撤销的事件写不进：撤销也不生效，保持待确认。
func TestKongGuardReleaseNeedsEvent(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.store.failKind[KongGuardEventFalseAlarm] = true
	h.fp.decided[1] = "gpt-6.1-sol"
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if hold := h.store.hold(1); hold == nil || h.store.schedulable(1) || !h.g.acct(1).cooldown.IsZero() {
		t.Fatalf("事件写失败时撤销生效了: %+v", hold)
	}
}

// 关闭写库失败：不算生效，记 hold_skipped。
func TestKongGuardHoldWriteFailure(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.store.failHold = true
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if h.store.hold(1) != nil || !h.store.schedulable(1) || len(h.fp.runs) != 0 {
		t.Fatalf("写库失败时关闭生效了")
	}
	skipped := h.store.eventsOf(KongGuardEventHoldSkipped)
	if len(skipped) != 1 || skipped[0].Detail["reason"] != "write_failed" {
		t.Fatalf("hold_skipped = %+v", skipped)
	}
}

// observe：不写持有记录，测指纹，6 小时内不重复测。
func TestKongGuardObserve(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.fp.decided[1] = "gpt-5.6-luna"
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if h.store.hold(1) != nil || !h.store.schedulable(1) {
		t.Fatalf("observe 写了持有记录")
	}
	h.now = h.now.Add(5 * time.Hour)
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if len(h.fp.runs) != 1 {
		t.Fatalf("observe 6 小时内重复测了指纹: %v", h.fp.runs)
	}
	h.now = h.now.Add(2 * time.Hour)
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if len(h.fp.runs) != 2 {
		t.Fatalf("observe 过了 6 小时没有再测: %v", h.fp.runs)
	}
}

// 组外账号只记录：被动拒绝不探测、不关闭。
func TestKongGuardOutsideGroupOnlyRecords(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, false)
	h.g.lastMaint = h.now
	h.usagePair(100, 1, kongGuardT0.Add(-time.Minute), 20*time.Second, true, "a")
	h.g.refreshSnapshot()
	h.g.initialized = true
	h.g.cursor = h.now.Add(-10 * time.Minute)
	h.tick()
	if len(h.store.eventsOf(KongGuardEventPassiveRefuse)) != 1 {
		t.Fatalf("组外账号的拒绝没有记录")
	}
	if h.probe.calls != 0 || h.store.hold(1) != nil {
		t.Fatalf("组外账号被探测或关闭")
	}
}

// 到期接管：删掉持有记录、版本加一、保持关闭，记 human_release；之后旧的正常结论不能打开。
func TestKongGuardExpiredTakeover(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.g.decideHold(1, 0, h.now)
	job := <-h.g.jobs
	exp := h.now.Add(-time.Minute)
	h.store.accounts[1].acc.AutoPauseOnExpired = true
	h.store.accounts[1].acc.ExpiresAt = &exp
	h.g.lastMaint = h.now
	h.g.tick()
	if h.store.hold(1) != nil || h.store.schedulable(1) {
		t.Fatalf("到期没有接管")
	}
	// 续期后旧的正常结论到达。
	h.store.accounts[1].acc.ExpiresAt = nil
	h.fp.decided[1] = "gpt-6.1-sol"
	h.g.handleResult(h.g.run(job))
	if h.store.schedulable(1) {
		t.Fatalf("到期接管后旧的正常结论重新打开了账号")
	}
	if rel := h.store.eventsOf(KongGuardEventHumanRelease); len(rel) != 1 || rel[0].Detail["reason"] != "expired" {
		t.Fatalf("human_release = %+v", rel)
	}
	// 再扫一次不重复接管。
	rev := h.store.rev(1)
	h.store.accounts[1].acc.ExpiresAt = &exp
	h.g.tick()
	if h.store.rev(1) != rev {
		t.Fatalf("重复接管加了版本")
	}
}

func TestKongGuardProbeOutcomes(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	for id := int64(1); id <= 4; id++ {
		h.store.addAccount(id, true)
	}
	h.probe.refuse[2] = true
	h.probe.noTick[3] = true
	h.probe.fail[4] = errors.New("探测请求返回 429")
	want := map[int64]string{1: kongGuardProbeAccept, 2: kongGuardProbeRefuse, 3: kongGuardProbeInconclusive, 4: kongGuardProbeInconclusive}
	for id, w := range want {
		if got := h.g.probe(context.Background(), id).Verdict; got != w {
			t.Errorf("账号 %d: probe = %q, want %q", id, got, w)
		}
	}
}

func TestKongGuardClassify(t *testing.T) {
	done := func(decided, verdict string) *KongFingerprintTestResult {
		return &KongFingerprintTestResult{Execution: KongFingerprintExecCompleted, Decided: decided, Verdict: verdict}
	}
	cases := []struct {
		name string
		r    *KongFingerprintTestResult
		want string
	}{
		{"luna", done("gpt-5.6-luna", ""), kongGuardFPLuna},
		{"6-luna", done("gpt-6-luna", ""), kongGuardFPLuna},
		{"请求的模型", done("gpt-6.1-sol", ""), kongGuardFPNormal},
		{"match", done("", KongFingerprintVerdictMatch), kongGuardFPNormal},
		{"难分的一对", done("gpt-6-astra|gpt-6.1-sol", ""), kongGuardFPNormal},
		{"别的模型", done("gpt-6-sol", ""), kongGuardFPInconclusive},
		{"含别的模型的一对", done("gpt-6-sol|gpt-6.1-sol", ""), kongGuardFPInconclusive},
		{"未完成", &KongFingerprintTestResult{Execution: "failed"}, kongGuardFPInconclusive},
		{"空", nil, kongGuardFPInconclusive},
	}
	for _, c := range cases {
		if got := kongGuardClassify(c.r, "gpt-6.1-sol"); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// 影响汇总：首次满 30 分钟，之后每小时重算；过了截止时刻 30 分钟定稿，定稿后不再计算；空会话单列。
func TestKongGuardImpact(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	heldAt := h.now.Add(-40 * time.Minute)
	h.store.events = []KongGuardEvent{{ID: 1, AccountID: 1, Op: "op-x", Kind: KongGuardEventHold, CreatedAt: heldAt, Detail: map[string]any{
		"held_at": heldAt.Format(time.RFC3339Nano), "window_from": heldAt.Add(-20 * time.Minute).Format(time.RFC3339Nano), "window_unknown": false}}}
	h.store.impactRow = []KongGuardImpactRow{
		{APIKeyID: 1, SessionID: "s1", First: heldAt.Add(-10 * time.Minute), Last: heldAt.Add(time.Minute), Count: 5},
		{APIKeyID: 1, SessionID: "", First: heldAt.Add(-8 * time.Minute), Last: heldAt.Add(-7 * time.Minute), Count: 2},
		{APIKeyID: 2, SessionID: "", First: heldAt.Add(-9 * time.Minute), Last: heldAt.Add(-6 * time.Minute), Count: 1},
	}
	h.g.runImpacts(h.now)
	d := h.store.impacts["op-x"]
	if d == nil || d["finalized"] != false || d["total"] != int64(8) {
		t.Fatalf("首次汇总 = %+v", d)
	}
	ns, _ := d["no_session"].([]KongGuardImpactRow)
	if len(ns) != 2 || ns[0].APIKeyID != 1 || ns[0].Count != 2 || ns[1].APIKeyID != 2 || ns[1].Count != 1 {
		t.Fatalf("空会话应按 API key 各列一行: %+v", ns)
	}
	if sessions, _ := d["sessions"].([]KongGuardImpactRow); len(sessions) != 1 {
		t.Fatalf("会话 = %+v", d["sessions"])
	}
	computed := d["computed_at"]
	h.now = h.now.Add(30 * time.Minute)
	h.g.runImpacts(h.now)
	if h.store.impacts["op-x"]["computed_at"] != computed {
		t.Fatalf("一小时内重算了")
	}
	// 截止时刻之后 30 分钟：定稿。重启（内存清空）后照样定稿且只定稿一次。
	h.now = heldAt.Add(kongGuardImpactCutoff + kongGuardImpactSettle)
	h.g.impactRun = map[string]time.Time{}
	h.g.runImpacts(h.now)
	if h.store.impacts["op-x"]["finalized"] != true {
		t.Fatalf("没有定稿: %+v", h.store.impacts["op-x"])
	}
	final := h.store.impacts["op-x"]["computed_at"]
	h.now = h.now.Add(2 * time.Hour)
	h.g.runImpacts(h.now)
	if h.store.impacts["op-x"]["computed_at"] != final {
		t.Fatalf("定稿后又重算了")
	}
}

// 巡检：有流量而最近没有结论的按 ProbeBusy，没有流量的按 ProbeIdle；不可调度、带持有记录的不巡检。
func TestKongGuardPatrolIntervals(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	for id := int64(1); id <= 4; id++ {
		h.store.addAccount(id, true)
	}
	h.store.accounts[3].acc.Schedulable = false
	h.store.accounts[4].acc.Extra["kong_downgrade_guard_hold"] = map[string]any{"op": "x", "stage": KongGuardStageConfirmed}
	h.g.refreshSnapshot()
	busy, idle := h.g.acct(1), h.g.acct(2)
	busy.lastTraffic, busy.lastVerdict, busy.lastProbe = h.now.Add(-time.Minute), h.now.Add(-11*time.Minute), h.now.Add(-11*time.Minute)
	idle.lastVerdict, idle.lastProbe = h.now.Add(-30*time.Minute), h.now.Add(-30*time.Minute)
	h.g.patrol(h.now)
	var got []int64
	for len(h.g.jobs) > 0 {
		got = append(got, (<-h.g.jobs).accountID)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("巡检 = %v, want [1]", got)
	}
}

func TestKongGuardConfigFromEnv(t *testing.T) {
	t.Setenv("KONG_DOWNGRADE_GUARD", "")
	t.Setenv("KONG_DOWNGRADE_GUARD_GROUP_ID", "")
	t.Setenv("KONG_DOWNGRADE_GUARD_PROBE_MODEL", "")
	t.Setenv("KONG_DOWNGRADE_GUARD_FRESH_SECONDS", "")
	cfg, err := KongDowngradeGuardConfigFromEnv()
	if err != nil || cfg.Mode != KongGuardModeObserve || cfg.GroupID != 0 || cfg.FreshAge != 3000*time.Second || cfg.ProbeModel != "gpt-6.1-sol" || cfg.TicketHoldMax != 2 {
		t.Fatalf("缺省配置 = %+v, err = %v", cfg, err)
	}
	t.Setenv("KONG_DOWNGRADE_GUARD", "on")
	t.Setenv("KONG_DOWNGRADE_GUARD_GROUP_ID", "10")
	t.Setenv("KONG_DOWNGRADE_GUARD_FRESH_SECONDS", "99999")
	t.Setenv("KONG_DOWNGRADE_GUARD_PROBE_MODEL", "gpt-6-astra")
	cfg, err = KongDowngradeGuardConfigFromEnv()
	if err != nil || cfg.Mode != KongGuardModeOn || cfg.GroupID != 10 || cfg.FreshAge != 3000*time.Second || cfg.ProbeModel != "gpt-6-astra" {
		t.Fatalf("配置 = %+v, err = %v", cfg, err)
	}
	for _, m := range []string{"gpt-6-luna", "gpt-5.6-luna", "gpt-6-sol"} {
		t.Setenv("KONG_DOWNGRADE_GUARD_PROBE_MODEL", m)
		if _, err := KongDowngradeGuardConfigFromEnv(); err == nil {
			t.Errorf("探测模型 %s 应被拒绝", m)
		}
	}
	t.Setenv("KONG_DOWNGRADE_GUARD_PROBE_MODEL", "")
	t.Setenv("KONG_DOWNGRADE_GUARD", "maybe")
	if _, err := KongDowngradeGuardConfigFromEnv(); err == nil {
		t.Errorf("非法模式应被拒绝")
	}
}

func TestKongGuardNewRequiresTester(t *testing.T) {
	cfg := KongDowngradeGuardConfig{Mode: KongGuardModeOn, ProbeModel: "gpt-6.1-sol"}
	if _, err := NewKongDowngradeGuard(cfg, nil, nil, nil, nil, nil); err == nil {
		t.Fatalf("没有指纹测试器时应拒绝创建")
	}
}

// 原生 WebSocket 的轮间复核：账号已关调度就要求重连；账号已删除不放行；读取失败按建连时的对象判断。
type kongGuardWSRepo struct {
	AccountRepository
	acc *Account
	err error
}

func (r kongGuardWSRepo) GetByID(context.Context, int64) (*Account, error) { return r.acc, r.err }

func TestKongWSTurnSchedulable(t *testing.T) {
	bound := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Schedulable: true}
	cases := []struct {
		name string
		repo kongGuardWSRepo
		want bool
	}{
		{"仍可调度", kongGuardWSRepo{acc: &Account{ID: 1, Schedulable: true}}, true},
		{"已关调度", kongGuardWSRepo{acc: &Account{ID: 1, Schedulable: false}}, false},
		{"已删除", kongGuardWSRepo{err: ErrAccountNotFound}, false},
		{"读取失败", kongGuardWSRepo{err: errors.New("db down")}, true},
	}
	for _, c := range cases {
		s := &OpenAIGatewayService{accountRepo: c.repo}
		if got := s.KongWSTurnSchedulable(context.Background(), bound); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	setupToken := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Schedulable: true}
	s := &OpenAIGatewayService{accountRepo: kongGuardWSRepo{acc: &Account{ID: 2, Schedulable: false}}}
	if s.KongWSTurnSchedulable(context.Background(), setupToken) {
		t.Errorf("setup-token 账号关调度后仍放行")
	}
}

// 回放夹具：2026-10-11 08:40–10:30 真实的 Responses 用量行（时刻、账号、模型、耗时；票指纹换成了序号），
// 账号 6 在这段时间里降档。按扫描的顺序（created_at、行号）喂给被动判据：只有账号 6 被判拒绝，第一次在 10:15:26，
// 共 30 次；它最后一次正常证据是 09:46:37 开始的那一轮。其余账号都有接受、没有拒绝。
func TestKongGuardReplay20261011(t *testing.T) {
	f, err := os.Open("testdata/kong_downgrade_guard/replay_20261011.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	var rows []KongGuardUsageRow
	for i, rec := range records[1:] {
		clock, err := time.Parse("15:04:05.000", rec[0])
		if err != nil {
			t.Fatal(err)
		}
		account, _ := strconv.ParseInt(rec[1], 10, 64)
		dur, _ := strconv.ParseInt(rec[3], 10, 64)
		rows = append(rows, KongGuardUsageRow{ID: int64(i + 1), AccountID: account, GroupID: 10, Model: rec[2], DurationMs: dur,
			CreatedAt: day.Add(clock.Sub(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC))), StateFP: rec[4], ReissuedFP: rec[5]})
	}

	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.usage = rows
	accounts := map[int64]bool{}
	for _, r := range rows {
		accounts[r.AccountID] = true
		h.g.snapshot[r.AccountID] = &Account{ID: r.AccountID}
	}
	h.g.initialized = true
	h.g.cursor = rows[0].CreatedAt
	h.now = rows[len(rows)-1].CreatedAt
	refusals := h.g.scan(h.now)

	at := func(clock string) time.Time {
		c, _ := time.Parse("15:04:05.000", clock)
		return day.Add(c.Sub(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)))
	}
	if len(refusals) != 1 || !refusals[6].CreatedAt.Equal(at("10:15:26.692")) {
		t.Fatalf("拒绝 = %+v，want 只有账号 6、第一次在 10:15:26.692", refusals)
	}
	if got := h.g.acct(6).lastGood; !got.Equal(at("09:46:37.631")) {
		t.Fatalf("账号 6 最后一次正常证据 = %v，want 09:46:37.631", got)
	}
	for id := range accounts {
		if id != 6 && h.g.acct(id).lastGood.IsZero() {
			t.Errorf("账号 %d 没有接受", id)
		}
	}

	// 全部拒绝的次数：用同样的顺序再判一遍。
	g := newKongGuardHarness(t, KongGuardModeObserve).g
	g.snapshot = h.g.snapshot
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	n := 0
	for _, r := range rows {
		if g.judge(r) == kongGuardProbeRefuse {
			if r.AccountID != 6 {
				t.Fatalf("账号 %d 在 %v 被判拒绝", r.AccountID, r.CreatedAt)
			}
			n++
		}
		g.noteIssued(r)
	}
	if n != 30 {
		t.Fatalf("拒绝次数 = %d, want 30", n)
	}
}

// 冷却与限频恢复失败时这一轮什么都不做，下一轮重试成功后照常。
func TestKongGuardRestoreRetried(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	until := h.now.Add(20 * time.Hour)
	h.store.events = []KongGuardEvent{{AccountID: 1, Kind: KongGuardEventFalseAlarm, CreatedAt: h.now.Add(-time.Hour),
		Detail: map[string]any{"cooldown_until": until.Format(time.RFC3339Nano)}}}
	h.store.failRecent = 1
	h.g.lastMaint = h.now
	h.g.restored = false
	h.tick()
	if h.g.restored || h.g.initialized || h.probe.calls != 0 {
		t.Fatalf("恢复失败时不应继续这一轮")
	}
	h.tick()
	if !h.g.restored || !h.g.acct(1).cooldown.Equal(until) {
		t.Fatalf("第二轮没有恢复冷却: %v", h.g.acct(1).cooldown)
	}
}

// 启动时 10 分钟以前的行只重建票表，不判定；之后的照常判定。
func TestKongGuardStartupFloor(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.usagePair(100, 1, h.now.Add(-12*time.Minute-30*time.Second), 20*time.Second, true, "a")
	h.usagePair(200, 2, h.now.Add(-9*time.Minute), 20*time.Second, true, "b")
	h.g.lastMaint = h.now
	h.g.restored = h.g.restore()
	h.g.refreshSnapshot()
	refusals := h.g.scan(h.now)
	if _, ok := refusals[1]; ok {
		t.Fatalf("12 分钟前的拒绝被重判了")
	}
	if _, ok := refusals[2]; !ok {
		t.Fatalf("9 分钟前的拒绝没有判定")
	}
}

// 容量按现读的账号判断：快照之后被移出服务分组的账号不算。
func TestKongGuardCapacityRereadsGroup(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.store.setGroup(2, false)
	if h.g.hasCapacity(1, h.now) {
		t.Fatalf("已移出服务分组的账号被算作容量")
	}
}

// 检测确认 luna 之后关闭写库失败：下一轮重新提交、不再测指纹；期间人动了开关就放弃。
func TestKongGuardPendingHoldRetried(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.restored = true
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	for _, id := range []int64{1, 2} {
		h.g.acct(id).lastVerdict, h.g.acct(id).lastProbe = h.now, h.now
	}
	h.fp.decided[1] = "gpt-5.6-luna"
	h.store.failHold = true
	h.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h.runJobs()
	if h.store.hold(1) != nil || len(h.g.pending) != 1 {
		t.Fatalf("写库失败时应留待重试: pending=%v", h.g.pending)
	}
	h.store.failHold = false
	h.now = h.now.Add(30 * time.Second)
	h.tick()
	if hold := h.store.hold(1); hold == nil || hold.Stage != KongGuardStageConfirmed || len(h.fp.runs) != 1 {
		t.Fatalf("重新提交没有生效或又测了指纹: hold=%+v runs=%v", hold, h.fp.runs)
	}

	h2 := newKongGuardHarness(t, KongGuardModeOn)
	h2.store.addAccount(1, true)
	h2.g.restored = true
	h2.g.refreshSnapshot()
	h2.g.lastMaint = h2.now
	h2.g.acct(1).lastVerdict, h2.g.acct(1).lastProbe = h2.now, h2.now
	h2.fp.decided[1] = "gpt-5.6-luna"
	h2.store.failHold = true
	h2.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h2.runJobs()
	h2.store.failHold = false
	h2.store.humanSet(1, true)
	h2.tick()
	if h2.store.hold(1) != nil || !h2.store.schedulable(1) || len(h2.g.pending) != 0 {
		t.Fatalf("版本变了仍补关")
	}
}

// observe 不动遗留的持有记录：待确认的不重测、不撤销，到期的不接管。
func TestKongGuardObserveLeavesHolds(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	for id, stage := range map[int64]string{1: KongGuardStageSuspect, 2: KongGuardStageSuspect} {
		fa := h.store.accounts[id]
		fa.acc.Schedulable = false
		fa.acc.Extra["kong_schedulable_rev"] = float64(1)
		fa.acc.Extra["kong_downgrade_guard_hold"] = map[string]any{"op": "old", "stage": stage,
			"next_fp_at": h.now.Add(-time.Minute).Format(time.RFC3339Nano)}
	}
	exp := h.now.Add(-time.Minute)
	h.store.accounts[2].acc.AutoPauseOnExpired = true
	h.store.accounts[2].acc.ExpiresAt = &exp
	h.fp.decided[1] = "gpt-6.1-sol"
	h.g.lastMaint = h.now
	h.tick()
	for _, id := range []int64{1, 2} {
		if h.store.hold(id) == nil || h.store.schedulable(id) || h.store.rev(id) != 1 {
			t.Fatalf("observe 改动了账号 %d 的持有记录", id)
		}
	}
	if len(h.fp.runs) != 0 {
		t.Fatalf("observe 重测了遗留的待确认账号: %v", h.fp.runs)
	}
}

// 手动探测在调用方协程里执行；账号正忙时直接返回忙；结论交给主循环，拒绝照常走关闭流程。
func TestKongGuardManualProbe(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.restored = true
	h.g.refreshSnapshot()
	h.g.acct(2).lastOutput = h.now
	h.probe.refuse[1] = true

	h.g.claim(1)
	if _, err := h.g.ManualProbe(context.Background(), 1); !errors.Is(err, ErrKongGuardBusy) {
		t.Fatalf("账号正忙时应返回忙: %v", err)
	}
	h.g.release(1)

	out, err := h.g.ManualProbe(context.Background(), 1)
	if err != nil || out.Verdict != kongGuardProbeRefuse {
		t.Fatalf("手动探测 = %+v, %v", out, err)
	}
	h.g.handleResult(<-h.g.results)
	if hold := h.store.hold(1); hold == nil || hold.Stage != KongGuardStageSuspect {
		t.Fatalf("手动探测拒绝后没有凭票关闭: %+v", hold)
	}
}

// 检测任务没能启动（有人正在测、取执行权失败）：不计限频，下一轮不等新流量就重新派发；版本变了就放弃。
func TestKongGuardDetectRetriedWhenNotStarted(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.restored = true
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	for _, id := range []int64{1, 2} {
		h.g.acct(id).lastVerdict, h.g.acct(id).lastProbe = h.now, h.now
	}
	h.fp.err = ErrKongFingerprintBusy
	h.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h.runJobs()
	h.fp.err = errors.New("读账号失败")
	h.now = h.now.Add(30 * time.Second)
	h.tick()
	if len(h.fp.runs) != 0 || !h.g.acct(1).fpStart[kongGuardPurposeDetect].IsZero() || len(h.g.detects) != 1 {
		t.Fatalf("没启动的检测不应计限频，且应留待重试: runs=%v detects=%v", h.fp.runs, h.g.detects)
	}
	if n := len(h.store.eventsOf(KongGuardEventFPStart)) + len(h.store.eventsOf(KongGuardEventFingerprint)); n != 0 {
		t.Fatalf("没启动的检测留下了 %d 条指纹事件", n)
	}
	h.fp.err = nil
	h.fp.decided[1] = "gpt-5.6-luna"
	h.now = h.now.Add(30 * time.Second)
	h.tick()
	if hold := h.store.hold(1); hold == nil || hold.Stage != KongGuardStageConfirmed || len(h.fp.runs) != 1 {
		t.Fatalf("恢复后没有重新检测并关闭: hold=%+v runs=%v", hold, h.fp.runs)
	}

	h2 := newKongGuardHarness(t, KongGuardModeOn)
	h2.store.addAccount(1, true)
	h2.g.restored = true
	h2.g.refreshSnapshot()
	h2.g.lastMaint = h2.now
	h2.g.acct(1).lastVerdict, h2.g.acct(1).lastProbe = h2.now, h2.now
	h2.fp.err = ErrKongFingerprintBusy
	h2.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h2.runJobs()
	h2.fp.err = nil
	h2.store.humanSet(1, true)
	h2.tick()
	if len(h2.fp.runs) != 0 || len(h2.g.detects) != 0 {
		t.Fatalf("版本变了仍重试检测: runs=%v", h2.fp.runs)
	}
}

// 启动时票表重建失败：下一轮重试，补回启动下界之前签发的票，更早的行仍不判定。
func TestKongGuardIssuedRebuildRetried(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeObserve)
	h.store.addAccount(1, true)
	h.usagePair(100, 1, h.now.Add(-20*time.Minute), 20*time.Second, true, "a")
	h.g.restored = true
	h.g.lastMaint = h.now
	h.store.failScan = 1
	h.g.acct(1).lastVerdict, h.g.acct(1).lastProbe = h.now, h.now
	h.tick()
	if h.g.issuedReady || len(h.g.issued) != 0 {
		t.Fatalf("重建失败却标成了已完成")
	}
	h.now = h.now.Add(30 * time.Second)
	// 这一行引用启动前 20 分钟签发的票，重建补回之后应判接受。
	h.store.usage = append(h.store.usage, KongGuardUsageRow{ID: 300, CreatedAt: h.now.Add(-5 * time.Second), AccountID: 1, GroupID: 10,
		Model: "gpt-6.1-sol", DurationMs: 1000, OutputTokens: 10, StateFP: "a"})
	h.tick()
	if !h.g.issuedReady || h.g.acct(1).lastGood.IsZero() {
		t.Fatalf("第二轮没有补回票表: ready=%v lastGood=%v", h.g.issuedReady, h.g.acct(1).lastGood)
	}
	if n := len(h.store.eventsOf(KongGuardEventPassiveRefuse)); n != 0 {
		t.Fatalf("启动下界之前的拒绝被判定了: %d", n)
	}
}

// 指纹已确认降档、关闭还没写进库时，之后的凭票关闭不能把它降成待确认：写库恢复后直接按确认关闭，不过容量、不再测指纹。
func TestKongGuardConfirmedPendingNotDowngraded(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	for _, id := range []int64{1, 2} {
		h.g.acct(id).lastVerdict, h.g.acct(id).lastProbe = h.now, h.now
	}
	h.g.acct(2).lastOutput = h.now
	h.fp.decided[1] = "gpt-5.6-luna"
	h.store.failHold = true
	h.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h.runJobs()
	// 写库仍然失败时又来了一次探测拒绝。
	h.g.restored = true
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if p := h.g.pending[1]; p.stage != KongGuardStageConfirmed {
		t.Fatalf("已确认的结论被降级: %+v", p)
	}
	// 写库恢复，但别的账号已经移出服务分组（没有容量）。
	h.store.failHold = false
	h.store.setGroup(2, false)
	h.now = h.now.Add(30 * time.Second)
	h.tick()
	if hold := h.store.hold(1); hold == nil || hold.Stage != KongGuardStageConfirmed || len(h.fp.runs) != 1 {
		t.Fatalf("恢复后应直接按确认关闭: hold=%+v runs=%v", hold, h.fp.runs)
	}

	// 写库在新的探测拒绝到来时恰好恢复：也写确认，不写待确认。
	h2 := newKongGuardHarness(t, KongGuardModeOn)
	h2.store.addAccount(1, true)
	h2.store.addAccount(2, true)
	h2.g.refreshSnapshot()
	h2.g.acct(2).lastOutput = h2.now
	h2.fp.decided[1] = "gpt-5.6-luna"
	h2.store.failHold = true
	h2.g.startDetect(1, 0, kongGuardPurposeDetect, kongGuardDetectMin, "capacity")
	h2.runJobs()
	h2.store.failHold = false
	h2.g.restored = true
	h2.g.decideHold(1, 0, h2.now)
	h2.runJobs()
	if hold := h2.store.hold(1); hold == nil || hold.Stage != KongGuardStageConfirmed || len(h2.fp.runs) != 1 {
		t.Fatalf("应写入确认降档: hold=%+v runs=%v", hold, h2.fp.runs)
	}
}

// 误报撤销提交了但返回错误：冷却要从库里重新读回来，之后的拒绝不得在冷却期内凭票关闭。
func TestKongGuardLostReleaseKeepsCooldown(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.store.addAccount(1, true)
	h.store.addAccount(2, true)
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	for _, id := range []int64{1, 2} {
		h.g.acct(id).lastVerdict, h.g.acct(id).lastProbe = h.now, h.now
	}
	h.g.acct(2).lastOutput = h.now
	h.fp.decided[1] = "gpt-6.1-sol"
	h.store.lostRelease = true
	h.g.decideHold(1, 0, h.now)
	h.runJobs()
	if !h.store.schedulable(1) || h.g.restored {
		t.Fatalf("前提不成立：撤销应已提交且控制状态待核实")
	}
	// 核实之前又来一次探测拒绝：不凭票关闭，结论留着。
	h.g.refreshSnapshot()
	h.g.decideHold(1, h.store.rev(1), h.now)
	h.runJobs()
	if h.store.hold(1) != nil {
		t.Fatalf("控制状态待核实时凭票关闭了")
	}
	// 下一轮重新读回冷却，留着的结论走冷却分支：不关，只起检测。
	h.now = h.now.Add(30 * time.Second)
	h.g.acct(2).lastOutput = h.now
	h.tick()
	if h.store.hold(1) != nil || !h.store.schedulable(1) {
		t.Fatalf("冷却期内凭票关闭了误报账号")
	}
	if a := h.g.acct(1); !a.cooldown.After(h.now) || a.knownOp != "" {
		t.Fatalf("冷却没有恢复: %+v", a)
	}
	if n := len(h.store.eventsOf(KongGuardEventHumanRelease)); n != 0 {
		t.Fatalf("哨兵自己的撤销被记成了人工解除: %d", n)
	}
}

// 关闭提交了但返回错误：核实之前不再凭票关闭别的账号，待确认数不超过名额。
func TestKongGuardLostHoldKeepsQuota(t *testing.T) {
	h := newKongGuardHarness(t, KongGuardModeOn)
	h.g.cfg.TicketHoldMax = 1
	for id := int64(1); id <= 3; id++ {
		h.store.addAccount(id, true)
	}
	h.g.refreshSnapshot()
	h.g.lastMaint = h.now
	for id := int64(1); id <= 3; id++ {
		h.g.acct(id).lastVerdict, h.g.acct(id).lastProbe, h.g.acct(id).lastOutput = h.now, h.now, h.now
	}
	h.store.lostHold = true
	h.g.decideHold(1, 0, h.now)
	h.store.lostHold = false
	h.g.decideHold(2, 0, h.now)
	h.runJobs()
	h.now = h.now.Add(30 * time.Second)
	h.tick()
	suspects := 0
	for id := int64(1); id <= 3; id++ {
		if hold := h.store.hold(id); hold != nil && hold.Stage == KongGuardStageSuspect {
			suspects++
		}
	}
	if suspects != 1 || h.store.hold(1) == nil {
		t.Fatalf("待确认数 = %d，want 1（账号 1）", suspects)
	}
}
