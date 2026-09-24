//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

// kongFakeSessionCache 在内存里模拟 SessionLimitCache 的会话计数，语义与 Redis 脚本一致：上限 ≤ 0 时直接放行、
// 什么也不写；已在集合里则刷新；未满则加入；满则拒绝。
type kongFakeSessionCache struct {
	SessionLimitCache
	now      func() time.Time
	sessions map[int64]map[string]time.Time
	// stealOnRegister 模拟另一个新会话抢在本次登记之前占掉账号的名额。
	stealOnRegister map[int64]string
	batchErr        error
	// dropFromBatch 模拟批量查询里个别账号失败：结果里没有它，整体仍返回 nil 错误（与 Redis 实现一致）。
	dropFromBatch map[int64]bool
	registerErr   error
	registerCaps  []int
	registerIdles []time.Duration
}

func newKongFakeSessionCache(now func() time.Time) *kongFakeSessionCache {
	return &kongFakeSessionCache{now: now, sessions: map[int64]map[string]time.Time{}, stealOnRegister: map[int64]string{}}
}

func (f *kongFakeSessionCache) put(accountID int64, hash string) {
	if f.sessions[accountID] == nil {
		f.sessions[accountID] = map[string]time.Time{}
	}
	f.sessions[accountID][hash] = f.now()
}

func (f *kongFakeSessionCache) expire(accountID int64, idle time.Duration) {
	for hash, at := range f.sessions[accountID] {
		if !f.now().Before(at.Add(idle)) {
			delete(f.sessions[accountID], hash)
		}
	}
}

func (f *kongFakeSessionCache) has(accountID int64, hash string) bool {
	_, ok := f.sessions[accountID][hash]
	return ok
}

func (f *kongFakeSessionCache) RegisterSession(_ context.Context, accountID int64, hash string, maxSessions int, idle time.Duration) (bool, error) {
	f.registerCaps = append(f.registerCaps, maxSessions)
	f.registerIdles = append(f.registerIdles, idle)
	if f.registerErr != nil {
		return true, f.registerErr
	}
	if hash == "" || maxSessions <= 0 {
		return true, nil
	}
	if thief, ok := f.stealOnRegister[accountID]; ok {
		delete(f.stealOnRegister, accountID)
		f.put(accountID, thief)
	}
	f.expire(accountID, idle)
	if f.has(accountID, hash) || len(f.sessions[accountID]) < maxSessions {
		f.put(accountID, hash)
		return true, nil
	}
	return false, nil
}

func (f *kongFakeSessionCache) GetActiveSessionCountBatch(_ context.Context, ids []int64, idle map[int64]time.Duration) (map[int64]int, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		if f.dropFromBatch[id] {
			continue
		}
		f.expire(id, idle[id])
		out[id] = len(f.sessions[id])
	}
	return out, nil
}

var kongSessionT0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func kongSessionOAuth(id int64, priority, maxSessions int) Account {
	a := Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: priority, GroupIDs: []int64{1}}
	if maxSessions > 0 {
		a.Extra = map[string]any{"max_sessions": maxSessions, "session_idle_timeout_minutes": 15}
	}
	return a
}

type kongSessionHarness struct {
	svc    *OpenAIGatewayService
	cache  *kongFakeSessionCache
	sticky *stubGatewayCache
	clock  *time.Time
}

func newKongSessionHarness(accounts []Account, conc stubConcurrencyCache, bindings map[string]int64) *kongSessionHarness {
	clock := kongSessionT0
	now := func() time.Time { return clock }
	cache := newKongFakeSessionCache(now)
	sticky := &stubGatewayCache{sessionBindings: bindings}
	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: accounts},
		cache:              sticky,
		concurrencyService: NewConcurrencyService(conc),
	}
	svc.kongSessionLimit = newKongSessionLimit(cache, now)
	return &kongSessionHarness{svc: svc, cache: cache, sticky: sticky, clock: &clock}
}

func (h *kongSessionHarness) selectAccount(t *testing.T, ctx context.Context, sessionHash string) *AccountSelectionResult {
	t.Helper()
	groupID := int64(1)
	selection, err := h.svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "gpt-5.2", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatal("expected a selection")
	}
	return selection
}

func TestKongSessionLimited(t *testing.T) {
	limited := kongSessionOAuth(1, 1, 2)
	unlimited := kongSessionOAuth(2, 1, 0)
	apiKey := kongSessionOAuth(3, 1, 2)
	apiKey.Type = AccountTypeAPIKey
	anthropic := kongSessionOAuth(4, 1, 2)
	anthropic.Platform = PlatformAnthropic
	for _, tc := range []struct {
		account *Account
		want    bool
	}{{&limited, true}, {&unlimited, false}, {&apiKey, false}, {&anthropic, false}, {nil, false}} {
		if got := kongSessionLimited(tc.account); got != tc.want {
			t.Errorf("account %+v: limited=%v, want %v", tc.account, got, tc.want)
		}
	}
	if got := kongSessionIdleTimeout(&limited); got != 15*time.Minute {
		t.Fatalf("idle timeout = %v", got)
	}
}

func TestKongSessionCountHash(t *testing.T) {
	ctx := KongWithSessionCountHash(context.Background(), "from-ctx")
	if got := kongSessionCountHash(ctx, "param"); got != "param" {
		t.Fatalf("参数优先，got %q", got)
	}
	if got := kongSessionCountHash(ctx, ""); got != "from-ctx" {
		t.Fatalf("参数为空时取上下文，got %q", got)
	}
	if got := kongSessionCountHash(context.Background(), ""); got != "" {
		t.Fatalf("都没有时为空，got %q", got)
	}
}

func TestKongSessionNewSessionPrefersAccountWithRoom(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	h.cache.put(1, "other")
	selection := h.selectAccount(t, context.Background(), "s-new")
	if selection.Account.ID != 2 {
		t.Fatalf("满额的高优先级账号不接新会话，应落到 2，got %d", selection.Account.ID)
	}
	if !h.cache.has(2, "s-new") || h.cache.has(1, "s-new") {
		t.Fatalf("会话应登记在 2 上：%v", h.cache.sessions)
	}
	if h.sticky.sessionBindings["openai:s-new"] != 2 {
		t.Fatalf("应绑定到 2：%v", h.sticky.sessionBindings)
	}
}

func TestKongSessionAllFullOverflows(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	h.cache.put(1, "a")
	h.cache.put(2, "b")
	selection := h.selectAccount(t, context.Background(), "s-new")
	if selection.Account.ID != 1 || selection.WaitPlan != nil {
		t.Fatalf("全满时第 2 层按原顺序溢出、直接抢槽，应落到 1，got account=%d plan=%+v", selection.Account.ID, selection.WaitPlan)
	}
	if !h.cache.has(1, "s-new") || len(h.cache.sessions[1]) != 2 {
		t.Fatalf("溢出要放行登记：%v", h.cache.sessions)
	}
	if st := h.svc.kongSessionLimit.stats; st.overflows != 1 || st.roomEmpty != 1 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestKongSessionConfirmRaceTriesNextAccount(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	h.cache.stealOnRegister[1] = "thief"
	selection := h.selectAccount(t, context.Background(), "s-new")
	if selection.Account.ID != 2 {
		t.Fatalf("1 的最后一个名额被抢，应换到 2，got %d", selection.Account.ID)
	}
	if h.cache.has(1, "s-new") || h.sticky.sessionBindings["openai:s-new"] != 2 {
		t.Fatalf("不能登记或绑定到被抢的账号：sessions=%v bindings=%v", h.cache.sessions, h.sticky.sessionBindings)
	}
	if st := h.svc.kongSessionLimit.stats; st.confirmRejected != 1 || st.overflows != 0 {
		t.Fatalf("stats=%+v", st)
	}
}

// 所有账号负载已满时第 2 层没有可选的，进入第 3 层排队。
func kongSessionAllBusy(ids ...int64) stubConcurrencyCache {
	loads := make(map[int64]*AccountLoadInfo, len(ids))
	for _, id := range ids {
		loads[id] = &AccountLoadInfo{AccountID: id, LoadRate: 100}
	}
	return stubConcurrencyCache{loadMap: loads}
}

func TestKongSessionFallbackWaitPrefersRoomThenOverflows(t *testing.T) {
	t.Run("room first", func(t *testing.T) {
		h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, kongSessionAllBusy(1, 2), nil)
		h.cache.put(1, "other")
		selection := h.selectAccount(t, context.Background(), "s-new")
		if selection.WaitPlan == nil || selection.Account.ID != 2 {
			t.Fatalf("第 3 层应在有余量的 2 上排队，got account=%d plan=%+v", selection.Account.ID, selection.WaitPlan)
		}
		if !h.cache.has(2, "s-new") {
			t.Fatalf("排队计划返回时就要登记：%v", h.cache.sessions)
		}
	})
	t.Run("room exhausted overflows", func(t *testing.T) {
		h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, kongSessionAllBusy(1, 2), nil)
		h.cache.put(1, "other")
		h.cache.stealOnRegister[2] = "thief"
		selection := h.selectAccount(t, context.Background(), "s-new")
		if selection.WaitPlan == nil || selection.Account.ID != 1 {
			t.Fatalf("有余量的账号用尽后应溢出到 1 排队，got account=%d", selection.Account.ID)
		}
		if !h.cache.has(1, "s-new") || h.svc.kongSessionLimit.stats.overflows != 1 {
			t.Fatalf("溢出要放行登记并计数：sessions=%v stats=%+v", h.cache.sessions, h.svc.kongSessionLimit.stats)
		}
	})
}

func TestKongSessionLoadErrorBranchPrefersRoom(t *testing.T) {
	conc := stubConcurrencyCache{loadBatchErr: errors.New("redis down")}
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, conc, nil)
	h.cache.put(1, "other")
	selection := h.selectAccount(t, context.Background(), "s-new")
	if selection.Account.ID != 2 || !h.cache.has(2, "s-new") {
		t.Fatalf("负载读取失败的降级分支同样只在有余量的账号里选，got %d", selection.Account.ID)
	}
}

func TestKongSessionStickySessionPassesThrough(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, map[string]int64{"openai:s-old": 1})
	h.cache.put(1, "other")
	selection := h.selectAccount(t, context.Background(), "s-old")
	if selection.Account.ID != 1 {
		t.Fatalf("已绑定的会话照常回到 1，got %d", selection.Account.ID)
	}
	if !h.cache.has(1, "s-old") || len(h.cache.sessions[1]) != 2 {
		t.Fatalf("粘性命中放行登记，允许超过上限：%v", h.cache.sessions)
	}
}

func TestKongSessionStickyWaitPassesThrough(t *testing.T) {
	conc := stubConcurrencyCache{acquireResults: map[int64]bool{1: false}}
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, conc, map[string]int64{"openai:s-old": 1})
	h.cache.put(1, "other")
	selection := h.selectAccount(t, context.Background(), "s-old")
	if selection.WaitPlan == nil || selection.Account.ID != 1 || !h.cache.has(1, "s-old") {
		t.Fatalf("在绑定账号上排队同样放行登记，got account=%d sessions=%v", selection.Account.ID, h.cache.sessions)
	}
}

func TestKongSessionCountHashFromContextWhenSelectionHasNone(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	h.cache.put(1, "other")
	ctx := KongWithSessionCountHash(context.Background(), "s-guardian")
	selection := h.selectAccount(t, ctx, "")
	if selection.Account.ID != 2 || !h.cache.has(2, "s-guardian") {
		t.Fatalf("不带哈希进入选号时用上下文里的计数身份，got %d sessions=%v", selection.Account.ID, h.cache.sessions)
	}
	if len(h.sticky.sessionBindings) != 0 {
		t.Fatalf("计数身份不改绑定：%v", h.sticky.sessionBindings)
	}
}

func TestKongSessionInactiveCases(t *testing.T) {
	t.Run("no limits", func(t *testing.T) {
		h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 0), kongSessionOAuth(2, 2, 0)}, stubConcurrencyCache{}, nil)
		if h.selectAccount(t, context.Background(), "s").Account.ID != 1 || len(h.cache.registerCaps) != 0 {
			t.Fatalf("没有账号设上限时不登记：%v", h.cache.registerCaps)
		}
	})
	t.Run("no session", func(t *testing.T) {
		h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
		h.cache.put(1, "other")
		if h.selectAccount(t, context.Background(), "").Account.ID != 1 || len(h.cache.registerCaps) != 0 {
			t.Fatal("没有会话标识的请求不计数、不受约束")
		}
	})
	t.Run("count query fails open", func(t *testing.T) {
		h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
		h.cache.put(1, "other")
		h.cache.batchErr = errors.New("redis down")
		if h.selectAccount(t, context.Background(), "s").Account.ID != 1 {
			t.Fatal("计数查询失败时按不限制处理")
		}
	})
}

func TestKongSessionConfirmFailsOpen(t *testing.T) {
	cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
	cache.registerErr = errors.New("redis down")
	l := newKongSessionLimit(cache, func() time.Time { return kongSessionT0 })
	g := &kongSessionGate{limit: l, hash: "s", full: map[int64]bool{}}
	acc := kongSessionOAuth(1, 1, 1)
	if !g.confirm(context.Background(), &acc) || l.stats.errors != 1 {
		t.Fatalf("登记出错时失败开放并计数：stats=%+v", l.stats)
	}
}

func TestKongSessionTouchRegistersWithPositiveCap(t *testing.T) {
	cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
	svc := &OpenAIGatewayService{kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongSessionT0 })}
	full := kongSessionOAuth(1, 1, 1)
	cache.put(1, "other")
	svc.kongSessionTouch(context.Background(), &full, "s")
	if !cache.has(1, "s") || len(cache.registerCaps) != 1 || cache.registerCaps[0] <= 0 {
		t.Fatalf("放行登记必须以正数上限调用才会写入：caps=%v sessions=%v", cache.registerCaps, cache.sessions)
	}
	unlimited := kongSessionOAuth(2, 1, 0)
	svc.kongSessionTouch(context.Background(), &unlimited, "s")
	if len(cache.registerCaps) != 1 {
		t.Fatal("未设上限的账号不登记")
	}
}

func TestKongSessionRenewUsesContextHash(t *testing.T) {
	cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
	svc := &OpenAIGatewayService{kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongSessionT0 })}
	acc := kongSessionOAuth(1, 1, 1)
	svc.kongSessionRenew(context.Background(), &acc)
	if len(cache.registerCaps) != 0 {
		t.Fatal("上下文里没有计数身份时不续期")
	}
	svc.kongSessionRenew(KongWithSessionCountHash(context.Background(), "s-ws"), &acc)
	if !cache.has(1, "s-ws") {
		t.Fatalf("按建连选号的会话哈希续期：%v", cache.sessions)
	}
	var nilSvc *OpenAIGatewayService
	nilSvc.kongSessionRenew(context.Background(), &acc)
}

func TestKongSessionSegmentsRecordedInPaceDecision(t *testing.T) {
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, kongPaceT0.Add(72*time.Hour), kongPaceT0), kongPaceRow(2, "pro", 10, kongPaceT0.Add(72*time.Hour), kongPaceT0)}
	p, repo, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	cache := newKongFakeSessionCache(func() time.Time { return kongPaceT0 })
	cache.put(1, "other")
	svc := &OpenAIGatewayService{kongPace: p, kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongPaceT0 })}
	a1, a2 := kongSessionOAuth(1, 10, 1), kongSessionOAuth(2, 10, 1)
	d := svc.kongPaceBegin(context.Background(), nil, "s", "gpt", false)
	g := svc.kongSessionGateBegin(context.Background(), "s", []*Account{&a1, &a2}, d)
	out := d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1, &a2)), openAILegacyUpstreamRateOrder{})
	if len(out) != 1 || out[0].account.ID != 2 {
		t.Fatalf("满额账号不进入节奏排序：%v", out)
	}
	d.finalOrder(out)
	d.acquired(2)
	d.finish()
	close(p.stop)
	p.wg.Add(1)
	p.runDecisionWriter()
	if len(repo.decisions) != 1 || !strings.Contains(string(repo.decisions[0].Rounds), `"session_full":[1]`) {
		t.Fatalf("决策记录要带满额账号：%+v", repo.decisions)
	}
	if strings.Contains(string(repo.decisions[0].Rounds), "session_overflow") {
		t.Fatalf("有余量时不标溢出：%s", repo.decisions[0].Rounds)
	}
}

func kongSessionPaceView(t *testing.T, round kongPaceRound, id int64) kongPaceAccountView {
	t.Helper()
	for _, v := range round.Candidates {
		if v.AccountID == id {
			return v
		}
	}
	t.Fatalf("候选里没有账号 %d", id)
	return kongPaceAccountView{}
}

func TestKongSessionOccupancyFeedsPaceWeights(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, reset, kongPaceT0), kongPaceRow(3, "pro", 10, reset, kongPaceT0)}
	for name, yaml := range map[string]string{
		"enabled":  kongPaceTestYAML,
		"disabled": strings.Replace(kongPaceTestYAML, "sessions:\n  enabled: true", "sessions:\n  enabled: false", 1),
	} {
		t.Run(name, func(t *testing.T) {
			p, _, _ := kongPaceTestComponent(t, rows, yaml)
			p.factsTick()
			cache := newKongFakeSessionCache(func() time.Time { return kongPaceT0 })
			cache.put(1, "x")
			svc := &OpenAIGatewayService{kongPace: p, kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongPaceT0 })}
			a1, a3 := kongSessionOAuth(1, 10, 2), kongSessionOAuth(3, 10, 0) // 1 有 1/2 个会话；3 未设上限
			d := svc.kongPaceBegin(context.Background(), nil, "s", "gpt", false)
			g := svc.kongSessionGateBegin(context.Background(), "s", []*Account{&a1, &a3}, d)
			d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1, &a3)), openAILegacyUpstreamRateOrder{})

			v1, v3 := kongSessionPaceView(t, d.rounds[0], 1), kongSessionPaceView(t, d.rounds[0], 3)
			if v1.ActiveSessions == nil || *v1.ActiveSessions != 1 || v1.MaxSessions != 2 {
				t.Fatalf("设了上限的候选要带会话计数：%+v", v1)
			}
			if v3.ActiveSessions != nil || v3.M != nil {
				t.Fatalf("未设上限的候选没有会话字段：%+v", v3)
			}
			wantM, wantRatio := 15.0, math.Exp2(1.5)
			if name == "disabled" {
				wantM, wantRatio = 0, 1
			}
			if v1.M == nil || *v1.M != wantM || math.Abs(v3.Weight/v1.Weight-wantRatio) > 1e-9 {
				t.Fatalf("M=%v w1=%v w3=%v，期望 M=%v、权重比 %v", v1.M, v1.Weight, v3.Weight, wantM, wantRatio)
			}
			// 有计数的候选即使 M 为 0 也要写出 m。
			if data, _ := json.Marshal(v1); !strings.Contains(string(data), fmt.Sprintf(`"m":%v`, wantM)) {
				t.Fatalf("决策记录要写出 m：%s", data)
			}
		})
	}
}

func TestKongSessionConfirmRejectCountsAsFullInNextRound(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, reset, kongPaceT0), kongPaceRow(2, "pro", 10, reset, kongPaceT0)}
	p, _, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	cache := newKongFakeSessionCache(func() time.Time { return kongPaceT0 })
	cache.put(1, "x")
	cache.put(2, "y")
	cache.put(2, "z")
	svc := &OpenAIGatewayService{kongPace: p, kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongPaceT0 })}
	a1, a2 := kongSessionOAuth(1, 10, 2), kongSessionOAuth(2, 10, 2) // 1 有余量（1/2），2 已满
	ctx := context.Background()
	d := svc.kongPaceBegin(ctx, nil, "s", "gpt", false)
	g := svc.kongSessionGateBegin(ctx, "s", []*Account{&a1, &a2}, d)

	d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1, &a2)), openAILegacyUpstreamRateOrder{})
	if r := d.rounds[0]; len(r.Candidates) != 1 || *kongSessionPaceView(t, r, 1).M != 15 || len(r.SessionFull) != 1 || r.SessionFull[0] != 2 || r.SessionOverflow {
		t.Fatalf("第一轮只有有余量的 1：%+v", r)
	}

	// 1 的最后一个名额被别的新会话抢走：确认不通过，下一轮溢出，1 按占满计，与 2 同档。
	cache.stealOnRegister[1] = "thief"
	if g.confirm(ctx, &a1) {
		t.Fatal("名额被抢时确认应不通过")
	}
	d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1, &a2)), openAILegacyUpstreamRateOrder{})
	r := d.rounds[1]
	v1, v2 := kongSessionPaceView(t, r, 1), kongSessionPaceView(t, r, 2)
	if *v1.M != 30 || *v2.M != 30 || math.Abs(v1.Weight-v2.Weight) > 1e-12 {
		t.Fatalf("溢出时满额账号的 M 相同：v1=%+v v2=%+v", v1, v2)
	}
	if len(r.SessionFull) != 2 || r.SessionFull[0] != 1 || r.SessionFull[1] != 2 || !r.SessionOverflow {
		t.Fatalf("第二轮按确认之后的分段记录：%+v", r)
	}
}

func TestKongSessionConfirmRejectWithNewlySetCap(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 10, reset, kongPaceT0), kongPaceRow(2, "pro", 10, reset, kongPaceT0)}
	p, _, _ := kongPaceTestComponent(t, rows, kongPaceTestYAML)
	p.factsTick()
	cache := newKongFakeSessionCache(func() time.Time { return kongPaceT0 })
	cache.put(1, "x")
	cache.put(2, "y")
	svc := &OpenAIGatewayService{kongPace: p, kongSessionLimit: newKongSessionLimit(cache, func() time.Time { return kongPaceT0 })}
	// 候选快照里 1 还没有上限；复核读到的库里已设上限 1，且已有一个会话。
	a1Snap, a1DB, a2 := kongSessionOAuth(1, 10, 0), kongSessionOAuth(1, 10, 1), kongSessionOAuth(2, 10, 1)
	ctx := context.Background()
	d := svc.kongPaceBegin(ctx, nil, "s", "gpt", false)
	g := svc.kongSessionGateBegin(ctx, "s", []*Account{&a1Snap, &a2}, d)

	d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1Snap, &a2)), openAILegacyUpstreamRateOrder{})
	if g.confirm(ctx, &a1DB) {
		t.Fatal("按库里的上限已满，确认应不通过")
	}
	d.reorder(g.preferRoomLoads(kongPaceAvailable(&a1Snap, &a2)), openAILegacyUpstreamRateOrder{})
	r := d.rounds[1]
	v1, v2 := kongSessionPaceView(t, r, 1), kongSessionPaceView(t, r, 2)
	if v1.M == nil || *v1.M != 30 || v1.ActiveSessions != nil || *v2.M != 30 || math.Abs(v1.Weight-v2.Weight) > 1e-12 {
		t.Fatalf("确认时才记为满额的账号也按占满计：v1=%+v v2=%+v", v1, v2)
	}
	if len(r.SessionFull) != 2 || r.SessionFull[0] != 1 || r.SessionFull[1] != 2 || !r.SessionOverflow {
		t.Fatalf("满额记录要含确认时才记满的账号：%+v", r)
	}
}

func TestKongSessionPeriodicStatsLog(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	clock := kongSessionT0
	l := newKongSessionLimit(newKongFakeSessionCache(func() time.Time { return clock }), func() time.Time { return clock })
	l.record(func(st *kongSessionStats) { st.placements++ })
	if strings.Contains(buf.String(), "周期计数") {
		t.Fatal("未到间隔不该输出汇总")
	}
	clock = clock.Add(kongSessionStatsInterval)
	l.record(func(st *kongSessionStats) { st.overflows++ })
	if !strings.Contains(buf.String(), "周期计数") || !strings.Contains(buf.String(), "placements=1") || !strings.Contains(buf.String(), "overflows=1") {
		t.Fatalf("到间隔应输出累计汇总：%s", buf.String())
	}
}

func TestKongSessionMissingCountFailsOpen(t *testing.T) {
	h := newKongSessionHarness([]Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, stubConcurrencyCache{}, nil)
	h.cache.put(1, "other")
	h.cache.dropFromBatch = map[int64]bool{2: true}
	if got := h.selectAccount(t, context.Background(), "s-new").Account.ID; got != 1 {
		t.Fatalf("计数缺项不能当成 0，应按查询失败、不限制处理，got %d", got)
	}
	if st := h.svc.kongSessionLimit.stats; st.errors != 1 || st.placements != 0 {
		t.Fatalf("缺项要计为查询失败：%+v", st)
	}
}

// 名额竞争不能让本来能服务的请求落空：有余量的账号都在确认时被抢满，就按溢出接纳。
func TestKongSessionConfirmRaceNeverRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		conc     stubConcurrencyCache
		accounts []Account
		steal    []int64
		wantWait bool
	}{
		{name: "layer 2 single account", conc: stubConcurrencyCache{}, accounts: []Account{kongSessionOAuth(1, 1, 1)}, steal: []int64{1}},
		{name: "layer 2 every account", conc: stubConcurrencyCache{}, accounts: []Account{kongSessionOAuth(1, 1, 1), kongSessionOAuth(2, 2, 1)}, steal: []int64{1, 2}},
		{name: "load error branch", conc: stubConcurrencyCache{loadBatchErr: errors.New("redis down")}, accounts: []Account{kongSessionOAuth(1, 1, 1)}, steal: []int64{1}, wantWait: true},
		{name: "layer 3", conc: kongSessionAllBusy(1), accounts: []Account{kongSessionOAuth(1, 1, 1)}, steal: []int64{1}, wantWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newKongSessionHarness(tc.accounts, tc.conc, nil)
			for _, id := range tc.steal {
				h.cache.stealOnRegister[id] = "thief"
			}
			selection := h.selectAccount(t, context.Background(), "s-new")
			if (selection.WaitPlan != nil) != tc.wantWait {
				t.Fatalf("wait plan = %+v, want wait=%v", selection.WaitPlan, tc.wantWait)
			}
			if !h.cache.has(selection.Account.ID, "s-new") {
				t.Fatalf("溢出接纳要放行登记：%v", h.cache.sessions)
			}
			if st := h.svc.kongSessionLimit.stats; st.overflows != 1 || st.confirmRejected == 0 {
				t.Fatalf("stats=%+v", st)
			}
		})
	}
}

func TestKongSessionRenewUsesCurrentAccountConfig(t *testing.T) {
	ctx := KongWithSessionCountHash(context.Background(), "s-ws")
	current := kongSessionOAuth(1, 1, 2)
	current.Extra["session_idle_timeout_minutes"] = 30
	cache := newKongFakeSessionCache(func() time.Time { return kongSessionT0 })
	svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{current}}}
	svc.kongSessionLimit = newKongSessionLimit(cache, func() time.Time { return kongSessionT0 })

	stale := kongSessionOAuth(1, 1, 0) // 建连时还没设上限
	svc.kongSessionRenew(ctx, &stale)
	if !cache.has(1, "s-ws") || len(cache.registerIdles) != 1 || cache.registerIdles[0] != 30*time.Minute {
		t.Fatalf("续期要用账号当前的上限与空闲超时：sessions=%v idles=%v", cache.sessions, cache.registerIdles)
	}

	removed := kongSessionOAuth(2, 1, 0)
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{removed}}
	staleLimited := kongSessionOAuth(2, 1, 2)
	svc.kongSessionRenew(ctx, &staleLimited)
	if cache.has(2, "s-ws") {
		t.Fatal("上限已移除的账号不再登记")
	}

	svc.accountRepo = stubOpenAIAccountRepo{}
	missing := kongSessionOAuth(3, 1, 2)
	svc.kongSessionRenew(ctx, &missing)
	if !cache.has(3, "s-ws") || svc.kongSessionLimit.stats.errors != 1 {
		t.Fatalf("读不到当前配置时沿用连接上的账号并计错误：sessions=%v stats=%+v", cache.sessions, svc.kongSessionLimit.stats)
	}
}
