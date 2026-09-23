//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// V7 故障演练与调用链用例（实施计划 8.1、8.2）里，要在选号调用链上断言的那几条：兜底与快照的档位、改约束之后的实际选号
// 与 WS 下一轮、非批量分支的槽满、登记竞争、窗口序号、只清快照后恢复发布。

func (h *kongPlanTierHarness) publishWith(t *testing.T, gen int64, fallback, snapshot string, expires time.Time) {
	t.Helper()
	kongPlanTierSeq++
	req, err := ParseKongPlanDispatch(strings.NewReader(fmt.Sprintf(`{"generation": %d, "seq": %d, "plan_version": 1, "cycle_id": "c",
		"fallback": %s, "snapshot": {"expires_at": %q, "accounts": %s}}`, gen, kongPlanTierSeq, fallback,
		expires.UTC().Format(time.RFC3339), snapshot)))
	require.NoError(t, err)
	h.store.now = func() time.Time { return h.now.UTC() }
	_, err = h.store.PutDispatch(context.Background(), req)
	require.NoError(t, err)
}

func (h *kongPlanTierHarness) controlBody(t *testing.T, body string) {
	t.Helper()
	req, err := ParseKongPlanControl(strings.NewReader(body))
	require.NoError(t, err)
	_, err = h.store.PutControl(context.Background(), req)
	require.NoError(t, err)
}

func TestKongPlanV7_FallbackAndSnapshotTiers(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanTierHarness(t)
	exp := h.now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fallback := `[{"id": 1, "tier": "standby", "active": true, "imminent": false, "credits": {"level": 1, "expires_at": "` + exp + `"}}]`
	snapshot := `[{"id": 1, "tier": "asap", "admit_below": {"6h": 50, "24h": 50}, "imminent": true, "credits": {"level": 1, "expires_at": "` + exp + `"}}]`
	h.publishWith(t, 1, fallback, snapshot, h.now.Add(time.Hour))
	acc := h.account(50, 10)

	res := h.rt.tier(ctx, acc, h.now)
	require.Equal(t, KongPlanInUseSnapshot, res.Input)
	require.Equal(t, KongPlanTierAsap, res.Entry.Tier, "快照有效：动态尽快档")
	require.True(t, res.Entry.Imminent)

	h.now = h.now.Add(2 * time.Hour)
	res = h.rt.tier(ctx, acc, h.now)
	require.Equal(t, KongPlanInUseFallback, res.Input, "快照过期：按兜底")
	require.Equal(t, KongPlanTierStandby, res.Entry.Tier, "兜底的基础备用档")
	require.False(t, res.Entry.Imminent)

	h.publishWith(t, 1, fallback, snapshot, h.now.Add(time.Hour))
	clear, err := ParseKongPlanPublish(strings.NewReader(`{"action": "disable", "clear": "snapshot", "note": "回到兜底"}`))
	require.NoError(t, err)
	_, err = h.store.PutPublish(ctx, clear)
	require.NoError(t, err)
	res = h.rt.tier(ctx, acc, h.now)
	require.Equal(t, KongPlanInUseFallback, res.Input, "只清快照：按兜底")
	require.Equal(t, KongPlanTierStandby, res.Entry.Tier)

	enable, err := ParseKongPlanPublish(strings.NewReader(`{"action": "enable", "note": "恢复"}`))
	require.NoError(t, err)
	pub, err := h.store.PutPublish(ctx, enable)
	require.NoError(t, err)
	h.publishWith(t, pub.Generation, fallback, snapshot, h.now.Add(time.Hour))
	require.Equal(t, KongPlanTierAsap, h.rt.tier(ctx, acc, h.now).Entry.Tier, "只清快照后恢复：下一次发布被接受、按新快照")
}

func TestKongPlanV7_FallbackKeepsWeeklyQuotaBeforeCredits(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 1, 50), th.selAccount(2, 2, 1, 99)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{}, "[]", 2)
	exp := h.now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fallback := `[{"id": 1, "tier": "normal", "active": true, "imminent": false, "credits": null},
		{"id": 2, "tier": "normal", "active": true, "imminent": false, "credits": {"level": 1, "expires_at": "` + exp + `"}}]`
	h.publishWith(t, 1, fallback, "[]", h.now.Add(time.Hour))
	h.now = h.now.Add(2 * time.Hour) // 边车停了：快照过期，按兜底
	require.Equal(t, KongPlanInUseFallback, h.store.current().inUse(h.now))

	sel := h.sess.selectAccount(t, context.Background(), "s1")
	require.Equal(t, int64(1), sel.Account.ID, "兜底下周额度仍先于 credits")
	require.Empty(t, h.ledger.Entries(2))

	h.sess.cache.put(1, "a")
	h.sess.cache.put(1, "b")
	sel = h.sess.selectAccount(t, context.Background(), "s2")
	require.Equal(t, int64(2), sel.Account.ID, "第一层到上限加宽限：兜底里的 credits 按级别先于第一层溢出")
	require.Len(t, h.ledger.Entries(2), 1)
}

func TestKongPlanV7_ControlChangesReachSelectionAndWS(t *testing.T) {
	ctx := context.Background()
	th := newKongPlanTierHarness(t)
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5,
		Priority: 9, GroupIDs: []int64{1}}
	credits := th.selAccount(2, 1, 0, 99)
	credits.Extra["auto_pause_7d_threshold"] = 0.99 // 现状的暂停判定（A 路径）：不在 credits 层时按 7d 拦下
	h := newKongPlanSelHarness(t, []Account{credits, relay}, stubConcurrencyCache{}, "["+th.creditsEntry(2, 1)+"]", 2)
	svc := h.sess.svc
	ws := svc.KongPlanWSBegin(ctx, &credits)
	rev := int64(20)
	control := func(mode, allow string, floor float64, fileHold bool) {
		rev++
		hold := "null"
		if fileHold {
			hold = fmt.Sprintf(`{"since": %q}`, h.now.Add(-time.Minute).UTC().Format(time.RFC3339))
		}
		h.controlBody(t, fmt.Sprintf(`{"revision": %d, "credits_mode": %q, "allow": %s, "floor": %g, "per_point": null,
			"overflow_margin": {"default": 1}, "file_hold": %s}`, rev, mode, allow, floor, hold))
	}
	holds := 0
	latch := func(set bool) {
		if set {
			holds++
			req, err := ParseKongPlanHold(strings.NewReader(fmt.Sprintf(`{"trigger": "manual:v7-%d", "reason": "manual"}`, holds)))
			require.NoError(t, err)
			_, err = h.store.Hold(ctx, req)
			require.NoError(t, err)
			return
		}
		req, err := ParseKongPlanRelease(strings.NewReader(fmt.Sprintf(`{"triggers": ["manual:v7-%d"], "note": "解除"}`, holds)))
		require.NoError(t, err)
		_, err = h.store.Release(ctx, req)
		require.NoError(t, err)
	}
	restart := func() { // 网关重启：从同一个库重建存储
		restarted := NewKongPlanStore(h.store.repo)
		restarted.now = h.store.now
		restarted.Start(ctx)
		h.store, h.rt.store = restarted, restarted
	}
	steps := []struct {
		name    string
		apply   func()
		credits bool
	}{
		{"on", func() { control("on", "[2]", 0, false) }, true},
		{"切 shadow", func() { control("shadow", "[2]", 0, false) }, false},
		{"切回 on", func() { control("on", "[2]", 0, false) }, true},
		{"移出白名单", func() { control("on", "[]", 0, false) }, false},
		{"提高保底到余额以上", func() { control("on", "[2]", 600, false) }, false},
		{"文件暂停", func() { control("on", "[2]", 0, true) }, false},
		{"删掉文件暂停", func() { control("on", "[2]", 0, false) }, true},
		{"锁存暂停", func() { latch(true) }, false},
		{"锁存暂停期间重启网关", restart, false},
		{"解除锁存暂停", func() { latch(false) }, true},
		// 两种暂停的组合：解除其中一种，另一种仍然有效
		{"刹车 → 放文件", func() { latch(true); control("on", "[2]", 0, true) }, false},
		{"再删文件：刹车仍在", func() { control("on", "[2]", 0, false) }, false},
		{"解除刹车", func() { latch(false) }, true},
		{"放文件 → 刹车", func() { control("on", "[2]", 0, true); latch(true) }, false},
		{"两者并存时重启网关", restart, false},
		{"解除刹车：文件仍在", func() { latch(false) }, false},
		{"删文件", func() { control("on", "[2]", 0, false) }, true},
	}
	for i, step := range steps {
		step.apply()
		sel := h.sess.selectAccount(t, ctx, fmt.Sprintf("s%d", i))
		want := int64(3)
		if step.credits {
			want = 2
		}
		require.Equal(t, want, sel.Account.ID, "%s：实际选号", step.name)
		require.Equal(t, step.credits, svc.KongPlanWSTurnAllowed(ctx, &credits, ws), "%s：已建立的 WS 下一轮", step.name)
	}
}

func TestKongPlanV7_NonBatchSlotFullTriesNextWithoutWaiting(t *testing.T) {
	th := newKongPlanTierHarness(t)
	accounts := []Account{th.selAccount(1, 1, 0, 50), th.selAccount(2, 2, 0, 50)}
	h := newKongPlanSelHarness(t, accounts, stubConcurrencyCache{acquireResults: map[int64]bool{1: false}}, "[]")
	h.sess.svc.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
		StickySessionMaxWaiting: 3, StickySessionWaitTimeout: 45 * time.Second, FallbackWaitTimeout: 30 * time.Second,
		FallbackMaxWaiting: 100, LoadBatchEnabled: false, SlotCleanupInterval: 30 * time.Second}}}
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(2), sel.Account.ID, "第一个账号槽满：换下一个，不排队")
	require.Nil(t, sel.WaitPlan)
}

func TestKongPlanV7_RegistrationRaceOverflowBeforeRelay(t *testing.T) {
	th := newKongPlanTierHarness(t)
	relay := Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5,
		Priority: 0, GroupIDs: []int64{1}}
	h := newKongPlanSelHarness(t, []Account{th.selAccount(1, 5, 1, 50), relay}, stubConcurrencyCache{}, "[]")
	h.sess.cache.stealQueue = map[int64][]string{1: {"thief-1", "thief-2"}}
	sel := h.sess.selectAccount(t, context.Background(), "s-new")
	require.Equal(t, int64(1), sel.Account.ID, "普通名额、宽限名额先后被抢：溢出轮仍先于中转")
	require.Equal(t, []int{1, 2, kongSessionUnlimited}, h.sess.cache.registerCaps, "每段只试一次，循环有限")
}

func TestKongPlanV7_InactiveWindowAdvancesSequenceOnce(t *testing.T) {
	inactive := func(at time.Time) KongPaceAccountRow {
		return KongPaceAccountRow{ID: 1, Plan: "pro", Concurrency: 8, UsedPercent: kongPaceF(0), WindowMinutes: kongPaceI(0), UsageUpdatedAt: kongPaceTm(at)}
	}
	r1 := kongPaceT0.Add(24 * time.Hour)
	st, _ := kongPaceObserve(nil, kongPaceRow(1, "pro", 99, r1, kongPaceT0), kongPaceT0)
	require.Equal(t, int64(1), st.WindowSeq)
	at := kongPaceT0
	for i := 0; i < 3; i++ { // 重置之后、首个请求之前：窗口未激活
		at = at.Add(time.Hour)
		st, _ = kongPaceObserve(st, inactive(at), at)
		require.Equal(t, int64(1), st.WindowSeq, "未激活不推进")
	}
	at = at.Add(time.Minute)
	r2 := at.Add(7 * 24 * time.Hour) // 首个请求起算新窗口
	st, obs := kongPaceObserve(st, kongPaceRow(1, "pro", 1, r2, at), at)
	require.Equal(t, kongPaceObsReset, obs)
	require.Equal(t, int64(2), st.WindowSeq, "起算后加一")
	for i := 0; i < 3; i++ {
		at = at.Add(time.Minute)
		st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", float64(2+i), r2, at), at)
	}
	at = at.Add(time.Minute)
	st, _ = kongPaceObserve(st, inactive(at), at)
	at = at.Add(time.Minute)
	st, _ = kongPaceObserve(st, kongPaceRow(1, "pro", 6, r2, at), at)
	require.Equal(t, int64(2), st.WindowSeq, "同一窗口里的后续读数与再次激活都不再加")
}

func TestKongPlanV7_ReleasedTriggerStaysReleasedAfterManyAndRestart(t *testing.T) {
	ctx := context.Background()
	repo := newKongPlanFakeRepo()
	s, _ := newKongPlanTestStore(t, repo)
	hold := func(s *KongPlanStore, trigger string) string {
		req, err := ParseKongPlanHold(strings.NewReader(fmt.Sprintf(`{"trigger": %q, "reason": "daily_cap"}`, trigger)))
		require.NoError(t, err)
		res, err := s.Hold(ctx, req)
		require.NoError(t, err)
		return res.State
	}
	for i := 0; i < 1100; i++ {
		trigger := fmt.Sprintf("daily_cap:c%d", i)
		require.Equal(t, "active", hold(s, trigger))
		req, err := ParseKongPlanRelease(strings.NewReader(fmt.Sprintf(`{"triggers": [%q], "note": "人工确认"}`, trigger)))
		require.NoError(t, err)
		_, err = s.Release(ctx, req)
		require.NoError(t, err)
	}
	require.Equal(t, "released", hold(s, "daily_cap:c0"), "超过 1000 个之后，最早解除的编号迟到也不生效")
	again, _ := newKongPlanTestStore(t, repo) // 网关重启：从同一个库重建
	require.Equal(t, "released", hold(again, "daily_cap:c0"), "重启之后同样不生效")
	v, err := again.GetControl(ctx)
	require.NoError(t, err)
	require.Empty(t, v.LatchedHolds)
}

// kongPlanThresholdRL 是走 B 路径（平台停调阈值 99）写标记的限流服务。
func kongPlanThresholdRL(t *testing.T) (*RateLimitService, *rateLimitAccountRepoStub) {
	t.Helper()
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	t.Cleanup(func() {
		accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
		accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	})
	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":99}`
	repo := &rateLimitAccountRepoStub{}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))
	return rl, repo
}

func TestKongPlanV7_ThresholdMarkerSkips7dOnlyWhileInCredits(t *testing.T) {
	h := newKongPlanTierHarness(t)
	rl, repo := kongPlanThresholdRL(t)
	window := func() string {
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(repo.lastTempReason), &payload))
		return fmt.Sprint(payload["window"])
	}
	ctx := context.Background()

	require.False(t, rl.ApplyAccountSchedulingThreshold(ctx, h.account(99.5, 10)), "credits 层：只到 7d 不写标记")
	require.Zero(t, repo.tempCalls)
	require.True(t, rl.ApplyAccountSchedulingThreshold(ctx, h.account(99.5, 99.5)))
	require.Equal(t, "5h", window(), "credits 层：5h 与 7d 同时触发只按 5h 写")

	h.control(t, 2, `"shadow"`, "[1]", 0) // 离开 credits 层
	require.True(t, rl.ApplyAccountSchedulingThreshold(ctx, h.account(99.5, 10)))
	require.Equal(t, "7d", window(), "离开 credits 层后按 7d 写回")
	h.control(t, 3, `"on"`, "[1]", 600) // 余额到保底同样离开 credits 层
	require.True(t, rl.ApplyAccountSchedulingThreshold(ctx, h.account(99.5, 10)))
	require.Equal(t, "7d", window())
}

func TestKongPlanV7_ThresholdMarkerShortWhileCreditsUndecided(t *testing.T) {
	h := newKongPlanTierHarness(t)
	rl, _ := kongPlanThresholdRL(t)
	ctx := context.Background()
	apply := func(used5h float64) *Account {
		t.Helper()
		acc := h.account(99.5, used5h)
		require.True(t, rl.ApplyAccountSchedulingThreshold(ctx, acc), "判不出时照常拦下")
		require.NotNil(t, acc.TempUnschedulableUntil)
		return acc
	}

	h.pace.snapshot.Store(nil) // 节奏首拍之前：计划给了 credits、约束允许，只差当前窗口
	require.True(t, h.rt.tier(ctx, h.account(99.5, 10), h.now).Undecided)
	acc := apply(10)
	require.LessOrEqual(t, time.Until(*acc.TempUnschedulableUntil), kongPlanUndecidedHold, "只写短标记，不留到 7d 重置")
	acc = apply(99.5)
	require.WithinDuration(t, h.now.Add(2*time.Hour), *acc.TempUnschedulableUntil, time.Second, "同时触发的 5h 照常按 5h 写")

	h.publish(t, 1, `[{"id": 1, "tier": "normal"}]`) // 计划没给 credits：判得出是暂停
	require.False(t, h.rt.tier(ctx, h.account(99.5, 10), h.now).Undecided)
	acc = apply(10)
	require.WithinDuration(t, h.reset, *acc.TempUnschedulableUntil, time.Second, "按 7d 重置写")

	repo := newKongPlanFakeRepo()
	repo.loadErr = errors.New("db down")
	store := NewKongPlanStore(repo)
	store.Start(ctx) // 启动加载失败：不知道所用输入
	h.rt.store = store
	acc = apply(10)
	require.LessOrEqual(t, time.Until(*acc.TempUnschedulableUntil), kongPlanUndecidedHold)
}

func TestKongPlanTier_AutoResetCardsReadyFollowsPathA(t *testing.T) {
	h := newKongPlanTierHarness(t)
	ctx := context.Background()
	acc := h.account(95, 10)
	acc.Extra["auto_pause_7d_threshold"] = 0.9
	acc.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
	cards := func(n int) {
		acc.Extra[OpenAIAutoResetCreditStateExtraKey] = map[string]any{"status": OpenAIAutoResetStatusAvailable,
			"available_count": n, "checked_at": time.Now().UTC().Format(time.RFC3339)}
	}

	cards(1)
	require.Equal(t, kongPlanLayerSubscription, h.rt.tier(ctx, acc, h.now).Layer, "有卡放行：普通阈值不算到线")
	require.Equal(t, 99.0, h.rt.windowThresholdPercent(ctx, acc, "7d", h.now), "生效阈值是 B 的 99")
	cards(0)
	require.Equal(t, kongPlanLayerCredits, h.rt.tier(ctx, acc, h.now).Layer)
	require.Equal(t, 90.0, h.rt.windowThresholdPercent(ctx, acc, "7d", h.now))

	h.control(t, 2, `"shadow"`, "[1]", 0) // 不在 credits 层时，定层与 A 路径的判定一致
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.True(t, paused)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, acc, h.now).Layer)
	cards(1)
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.False(t, paused)
	require.Equal(t, kongPlanLayerSubscription, h.rt.tier(ctx, acc, h.now).Layer)

	acc.Extra[OpenAIAutoResetCredit7dThresholdExtraKey] = 0.94 // 到了消费阈值：有卡也暂停
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(ctx, acc)
	require.True(t, paused)
	require.Equal(t, kongPlanLayerPaused, h.rt.tier(ctx, acc, h.now).Layer)
}

func TestKongPlanTokenCount_ReusesButDoesNotBindWhilePlanInEffect(t *testing.T) {
	th := newKongPlanTierHarness(t)
	h := newKongPlanSelHarness(t, []Account{th.selAccount(1, 1, 0, 99.5)}, stubConcurrencyCache{}, "["+th.creditsEntry(1, 1)+"]", 1)
	group := int64(1)
	ctx := context.Background()
	acc, err := h.sess.svc.SelectAccountForTokenCount(ctx, &group, "s-tc", "gpt-5.2", "", PlatformOpenAI)
	require.NoError(t, err)
	require.Equal(t, int64(1), acc.ID)
	require.Empty(t, h.sess.sticky.sessionBindings, "这条路径不经两轮排序，计划生效时不写会话绑定")
	require.Empty(t, h.ledger.Entries(1), "也不登记入层")

	kongPlanRT.Store(nil) // 没有计划组件：与上游一样即时绑定
	_, err = h.sess.svc.SelectAccountForTokenCount(ctx, &group, "s-tc", "gpt-5.2", "", PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, h.sess.sticky.sessionBindings, 1)
}
