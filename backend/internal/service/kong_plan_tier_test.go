//go:build unit

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 定层与两条暂停路径的 7d 跳过（DESIGN-account-selection.md 第 2.2 节、第 4.2 节第 2 步）。

type kongPlanTierHarness struct {
	rt     *kongPlanRuntime
	store  *KongPlanStore
	pace   *KongOpenAIAccountPace
	ledger *KongPlanLedger
	lstore *kongPlanFlakyLedgerStore
	now    time.Time
	reset  time.Time
}

func newKongPlanTierHarness(t *testing.T) *kongPlanTierHarness {
	t.Helper()
	ctx := context.Background()
	h := &kongPlanTierHarness{now: time.Now()}
	h.reset = h.now.Add(48 * time.Hour).Truncate(time.Second)
	store := NewKongPlanStore(newKongPlanFakeRepo())
	store.Start(ctx)
	h.store = store
	h.control(t, 1, `"on"`, "[1]", 0)
	h.publish(t, 1, `[{"id": 1, "tier": "normal", "credits": {"level": 1, "expires_at": "`+h.now.Add(30*24*time.Hour).UTC().Format(time.RFC3339)+`"}}]`)
	h.lstore = &kongPlanFlakyLedgerStore{kongPlanFakeLedgerStore: newKongPlanFakeLedgerStore()}
	h.ledger = NewKongPlanLedger(h.lstore, store)
	h.pace = NewKongOpenAIAccountPace(&kongPaceFakeRepo{}, &kongPlanFakePaceStore{ledger: newKongPlanFakeLedgerStore()}, nil, t.TempDir()+"/absent.yaml")
	h.setPace(1, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})
	h.rt = &kongPlanRuntime{store: store, ledger: h.ledger, pace: h.pace, now: func() time.Time { return h.now },
		thresholds: func(context.Context) map[string]int { return nil }}
	kongPlanRT.Store(h.rt)
	t.Cleanup(func() { kongPlanRT.Store(nil) })
	return h
}

var kongPlanTierSeq int64

func (h *kongPlanTierHarness) control(t *testing.T, rev int64, mode, allow string, floor float64) {
	t.Helper()
	req, err := ParseKongPlanControl(strings.NewReader(fmt.Sprintf(`{"revision": %d, "credits_mode": %s, "allow": %s, "floor": %g,
		"per_point": null, "overflow_margin": {"default": 1}}`, rev, mode, allow, floor)))
	require.NoError(t, err)
	_, err = h.store.PutControl(context.Background(), req)
	require.NoError(t, err)
}

func (h *kongPlanTierHarness) publish(t *testing.T, gen int64, accounts string) {
	t.Helper()
	kongPlanTierSeq++
	req, err := ParseKongPlanDispatch(strings.NewReader(fmt.Sprintf(`{"generation": %d, "seq": %d, "plan_version": 1, "cycle_id": "c",
		"fallback": [], "snapshot": {"expires_at": %q, "accounts": %s}}`, gen, kongPlanTierSeq, h.now.Add(time.Hour).UTC().Format(time.RFC3339), accounts)))
	require.NoError(t, err)
	h.store.now = func() time.Time { return h.now.UTC() }
	_, err = h.store.PutDispatch(context.Background(), req)
	require.NoError(t, err)
}

func (h *kongPlanTierHarness) setPace(id int64, st kongPaceAccountState) {
	snap := h.pace.snapshot.Load()
	accounts := map[int64]kongPaceFacts{}
	if snap != nil {
		for k, v := range snap.accounts {
			accounts[k] = v
		}
	}
	accounts[id] = kongPaceFacts{Plan: "pro", State: st}
	h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: accounts})
}

func (h *kongPlanTierHarness) account(used7d, used5h float64) *Account {
	return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_scheduling_threshold": 99},
		Extra: map[string]any{
			"codex_7d_used_percent": used7d, "codex_7d_window_minutes": 10080, "codex_7d_reset_at": h.reset.UTC().Format(time.RFC3339),
			"codex_5h_used_percent": used5h, "codex_5h_window_minutes": 300, "codex_5h_reset_at": h.now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"codex_usage_updated_at": h.now.Add(-time.Minute).UTC().Format(time.RFC3339),
		}}
}

func TestKongPlanTier_Layers(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanTierHarness(t)

	res := h.rt.tier(ctx, h.account(50, 10), h.now)
	require.Equal(t, kongPlanLayerSubscription, res.Layer)
	require.Equal(t, KongPlanInUseSnapshot, res.Input)
	require.Equal(t, int64(4), res.WindowSeq)

	res = h.rt.tier(ctx, h.account(99, 10), h.now)
	require.Equal(t, kongPlanLayerCredits, res.Layer, "到停调阈值、三处条件都满足")
	require.True(t, res.Eligible)
	require.Equal(t, 1, res.Entry.Credits.Level)

	// 不看已写下的暂停标记：资格只看此刻的读数与设置
	marked := h.account(99, 10)
	until := h.reset
	marked.TempUnschedulableUntil = &until
	require.Equal(t, kongPlanLayerCredits, h.rt.tier(ctx, marked, h.now).Layer)

	stopped := h.account(99, 10)
	stopped.Schedulable = false
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, stopped, h.now).Layer, "人工停用")
	parent := int64(7)
	shadow := h.account(99, 10)
	shadow.ParentAccountID = &parent
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, shadow, h.now).Layer, "Spark 影子账号不进 credits 层")

	// 窗口未知：读数的重置时刻比节奏状态晚出容差，或节奏状态里还没有当前窗口
	later := h.account(99, 10)
	later.Extra["codex_7d_reset_at"] = h.reset.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	res = h.rt.tier(ctx, later, h.now)
	require.True(t, res.WindowUnknown)
	require.Equal(t, kongPlanLayerPaused, res.Layer)
	h.setPace(1, kongPaceAccountState{Plan: "pro", Balance: &kongPlanBalancePoint{At: h.now, Value: 500}})
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	h.setPace(1, kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4,
		Balance: &kongPlanBalancePoint{At: h.now.Add(-time.Hour), Value: 500}})

	// 保底：最近一次有数值的余额要高于保底；还没处理的观测也算
	h.control(t, 2, `"on"`, "[1]", 500)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	h.ledger.noteLatest(kongPlanObservation{AccountID: 1, At: h.now, Balance: kongPlanF(800)})
	require.Equal(t, kongPlanLayerCredits, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)

	// 人工约束：模式、白名单、暂停
	h.control(t, 3, `"shadow"`, "[1]", 0)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	h.control(t, 4, `"on"`, "[2]", 0)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	h.control(t, 5, `"on"`, "[1]", 0)
	hold, err := ParseKongPlanHold(strings.NewReader(`{"trigger": "manual:t", "reason": "manual"}`))
	require.NoError(t, err)
	_, err = h.store.Hold(ctx, hold)
	require.NoError(t, err)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	rel, err := ParseKongPlanRelease(strings.NewReader(`{"triggers": ["manual:t"], "note": "x"}`))
	require.NoError(t, err)
	_, err = h.store.Release(ctx, rel)
	require.NoError(t, err)
	require.Equal(t, kongPlanLayerCredits, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)

	// 作废时刻已到、计划没给级别
	h.publish(t, 1, `[{"id": 1, "tier": "normal", "credits": {"level": 2, "expires_at": "`+h.now.Add(-time.Minute).UTC().Format(time.RFC3339)+`"}}]`)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, h.account(99, 10), h.now).Layer)
	h.publish(t, 1, `[{"id": 1, "tier": "standby"}]`)
	res = h.rt.tier(ctx, h.account(99, 10), h.now)
	require.Equal(t, kongPlanLayerPaused, res.Layer)
	require.Equal(t, KongPlanTierStandby, res.Entry.Tier)
}

// 判定处的跳过：credits 层账号不按 7d 暂停，同时触发的 5h 照常；离开 credits 层后按 7d 写回。
func TestKongPlanTier_PausePathsSkip7d(t *testing.T) {
	h := newKongPlanTierHarness(t)
	ctx := context.Background()
	acc := h.account(99, 10)
	acc.Extra["auto_pause_7d_threshold"] = 0.99

	paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.False(t, paused, "A 路径：credits 层不按 7d 暂停")
	d := EvaluateAccountSchedulingThreshold(acc, nil, time.Now())
	require.False(t, d.ShouldPause, "B 路径：7d 不进候选")

	both := h.account(99, 99)
	d = EvaluateAccountSchedulingThreshold(both, nil, time.Now())
	require.True(t, d.ShouldPause)
	require.Equal(t, "5h", d.Window, "同时触发的 5h 照常")

	h.control(t, 9, `"shadow"`, "[1]", 0)
	paused, decision := shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.True(t, paused)
	require.Equal(t, "7d", decision.window)
	d = EvaluateAccountSchedulingThreshold(both, nil, time.Now())
	require.Equal(t, "7d", d.Window, "离开 credits 层后，下一次判定按 7d（重置较晚）写回")

	// 没有接上计划组件：行为与现状相同
	kongPlanRT.Store(nil)
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.True(t, paused)
}

func TestKongPlanAccountsView(t *testing.T) {
	h := newKongPlanTierHarness(t)
	pro := h.account(99, 0)
	pro.Extra = map[string]any{kongCodexWindowShapeKey: kongCodexShape7d, "codex_7d_used_percent": 0.0, "codex_7d_window_minutes": 0,
		"codex_usage_updated_at": h.now.UTC().Format(time.RFC3339)}
	plus := h.account(40, 10)
	plus.ID = 2
	unknown := h.account(0, 0)
	unknown.ID = 3
	unknown.Extra = map[string]any{"codex_7d_window_minutes": 10080}
	relay := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{}}
	views, err := KongPlanAccounts(context.Background(), &kongPlanListRepo{stubQuotaAccountRepo: repo, list: []Account{*plus, *pro, *unknown, *relay}})
	require.NoError(t, err)
	require.Len(t, views, 3)
	require.Equal(t, int64(1), views[0].ID)
	require.Equal(t, "not_applicable", views[0].Windows["5h"].State)
	require.Equal(t, "inactive", views[0].Windows["7d"].State)
	require.Equal(t, "subscription", views[0].Tier)
	require.Equal(t, "active", views[1].Windows["7d"].State)
	require.Equal(t, 99.0, views[1].Windows["7d"].Threshold)
	require.Equal(t, "unknown", views[2].Windows["7d"].State)
	require.Equal(t, []KongPlanTierEntry{}, views[1].OpenEntries)

	_, err = KongPlanAccounts(context.Background(), nil)
	require.Error(t, err)
}

type kongPlanListRepo struct {
	*stubQuotaAccountRepo
	list []Account
}

func (r *kongPlanListRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return r.list, nil
}
