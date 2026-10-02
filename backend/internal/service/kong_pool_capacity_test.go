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

type kongPoolRepo struct {
	stubOpenAIAccountRepo
}

func (r kongPoolRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}

func (h *kongPlanTierHarness) forecast(t *testing.T, cycle string, computedAt time.Time, ids ...int64) {
	t.Helper()
	accs := make([]string, len(ids))
	for i, id := range ids {
		accs[i] = fmt.Sprintf(`{"id": %d, "k": 1, "depth_pp_per_hour": 2.5}`, id)
	}
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	req, err := ParseKongPlanForecast(strings.NewReader(fmt.Sprintf(`{"cycle_id": %q, "plan_version": 1, "computed_at": %q,
		"interval_min": 60, "per_session_pp_per_hour": 1.5, "avg_session_pp": 0.8, "demand_estimate_pp_per_hour": 10,
		"accounts": [%s],
		"forecast": [{"from": %q, "pp_per_hour": 2.5, "sessions": 1}, {"from": %q, "pp_per_hour": 12, "sessions": 8}],
		"runway_h": {"immediate": 67, "all_cards": null, "concentrate": null},
		"credits_runway_h": [{"level": 1, "hours": null}], "next_reset_at": %q}`,
		cycle, f(computedAt), strings.Join(accs, ","), f(h.now), f(h.now.Add(3*time.Hour)), f(h.reset))))
	require.NoError(t, err)
	_, err = h.store.PutForecast(context.Background(), req)
	require.NoError(t, err)
}

func kongPoolService(accounts ...*Account) *OpenAIGatewayService {
	list := make([]Account, len(accounts))
	for i, a := range accounts {
		list[i] = *a
	}
	return &OpenAIGatewayService{accountRepo: kongPoolRepo{stubOpenAIAccountRepo{accounts: list}}}
}

func TestKongPoolCapacity_SubscriptionThrottle(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.forecast(t, "c", h.now, 1)
	relay := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	newbie := h.account(10, 1)
	newbie.ID = 2 // 慢速部分里没有参数：不计
	view, err := kongPoolService(h.account(50, 10), relay, newbie).KongPoolCapacity(context.Background())
	require.NoError(t, err)
	cn := view.CapacityNow
	require.Equal(t, KongPoolLayerCap{PPPerHour: 2.5, Accounts: 1}, cn.ByLayer.Subscription, "min(阈值 − 用量, 深度线) × k")
	require.Empty(t, cn.ByLayer.Credits)
	require.Equal(t, 1, cn.Sessions)
	require.False(t, view.OnCredits)
	require.Equal(t, "fresh", view.Slow.State)
	require.Equal(t, KongPlanInUseSnapshot, view.Inputs.InUse)
	require.Equal(t, "throttle", view.Advice.Level, "当前能力低于需求估计")
	require.NotNil(t, view.Advice.Until)
	require.True(t, view.Advice.Until.Equal(h.now.Add(3*time.Hour).Truncate(time.Second)), "预报里第一个回到需求以上的点")
}

func TestKongPoolCapacity_OnlyCredits(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.forecast(t, "c", h.now, 1)
	view, err := kongPoolService(h.account(99, 10)).KongPoolCapacity(context.Background())
	require.NoError(t, err)
	cn := view.CapacityNow
	require.Zero(t, cn.ByLayer.Subscription.Accounts)
	require.Len(t, cn.ByLayer.Credits, 1)
	lv := cn.ByLayer.Credits[0]
	require.Equal(t, 1, lv.Level)
	require.Equal(t, 2.5, lv.PPPerHour, "余额高于保底就按吞吐计")
	require.NotNil(t, lv.BalanceCredits)
	require.Equal(t, 500.0, *lv.BalanceCredits)
	require.False(t, lv.RateKnown, "换算率未知")
	require.True(t, view.OnCredits)
	require.Contains(t, view.Advice.Reason, "只剩 credits 层可服务")
	require.Contains(t, view.Advice.Reason, "credits 续航无法估计")
}

func TestKongPoolCapacity_PauseAndSlowStates(t *testing.T) {
	h := newKongPlanTierHarness(t)
	stopped := h.account(50, 10)
	stopped.Schedulable = false
	view, err := kongPoolService(stopped).KongPoolCapacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stale", view.Slow.State, "从未收到慢速部分")
	require.Equal(t, "null", string(view.Forecast))
	require.Nil(t, view.DemandEstimatePPPerHour)
	require.Equal(t, "normal", view.Advice.Level)
	require.Contains(t, view.Advice.Reason, "还没有慢速部分")

	h.forecast(t, "other-cycle", h.now, 1)
	view, err = kongPoolService(stopped).KongPoolCapacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, "degraded", view.Slow.State, "与最近一次接受的发布不是同一周期")
	require.Equal(t, "pause", view.Advice.Level, "当前能力为 0")
	require.NotNil(t, view.Advice.Until)
	require.True(t, view.Advice.Until.Equal(h.now.Add(3*time.Hour).Truncate(time.Second)), "预报里第一个大于 0 的点")

	h.now = h.now.Add(3 * time.Hour)
	view, err = kongPoolService(stopped).KongPoolCapacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stale", view.Slow.State, "超过 2 个周期")
	require.NotEqual(t, "null", string(view.Forecast), "照常返回最后一份")
}

func TestKongPoolCapacity_ResetWindowCountsAsEmpty(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.forecast(t, "c", h.now, 1)
	absolute := h.account(100, 10)
	absolute.Extra["codex_7d_reset_at"] = h.now.Add(-time.Minute).UTC().Format(time.RFC3339)
	relative := h.account(100, 10)
	delete(relative.Extra, "codex_7d_reset_at")
	relative.Extra["codex_7d_reset_after_seconds"] = 60
	relative.Extra["codex_usage_updated_at"] = h.now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	pending := h.account(100, 10) // 重置时刻未到：照旧没有余量
	for name, c := range map[string]struct {
		acc  *Account
		want float64
	}{"绝对时刻已过": {absolute, 2.5}, "相对倒计时已过": {relative, 2.5}, "未重置": {pending, 0}} {
		t.Run(name, func(t *testing.T) {
			view, err := kongPoolService(c.acc).KongPoolCapacity(context.Background())
			require.NoError(t, err)
			require.Equal(t, c.want, view.CapacityNow.ByLayer.Subscription.PPPerHour)
			if c.want > 0 {
				require.NotEqual(t, "pause", view.Advice.Level, "旧窗口的用量不能让调用方停流")
			}
		})
	}
}
