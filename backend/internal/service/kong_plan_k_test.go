//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 套餐系数 K 与它的时间线、单位倍数：契约校验、按时刻取 K、余额记账与当前能力的换算、节奏权重的容量系数、规格倍数
// 变化时已记上涨的折算（工作区 DESIGN-plan-tiers.md 第 3.1、3.5 节）。

// kongPlanKSwitch 是老 pro 预计切换的时刻（北京时间 10-30 15:00）。
var kongPlanKSwitch = time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)

func kongPlanParseForecastAccounts(accounts string) (*KongPlanForecast, error) {
	return ParseKongPlanForecast(strings.NewReader(kongPlanForecastWith("", accounts)))
}

func TestKongPlanForecastAccount_KAt(t *testing.T) {
	a := KongPlanForecastAccount{ID: 3, K: 0.8, KTimeline: []KongPlanKSegment{
		{From: kongPlanKSwitch.Add(-48 * time.Hour), K: 1}, {From: kongPlanKSwitch, K: 0.5}}}
	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"早于第一段：用 k", kongPlanKSwitch.Add(-49 * time.Hour), 0.8},
		{"第一段起点", kongPlanKSwitch.Add(-48 * time.Hour), 1},
		{"切换前一刻", kongPlanKSwitch.Add(-time.Nanosecond), 1},
		{"切换时刻本身", kongPlanKSwitch, 0.5},
		{"切换之后", kongPlanKSwitch.Add(30 * 24 * time.Hour), 0.5},
	}
	for _, c := range cases {
		require.Equal(t, c.want, a.kAt(c.at), c.name)
	}
	require.Equal(t, 0.8, KongPlanForecastAccount{K: 0.8}.kAt(kongPlanKSwitch), "没有时间线")
	future := KongPlanForecastAccount{K: 1, KTimeline: []KongPlanKSegment{{From: kongPlanKSwitch, K: 0.5}}}
	require.Equal(t, 1.0, future.kAt(kongPlanKSwitch.Add(-time.Hour)), "时间线全在未来")
}

func TestParseKongPlanForecast_KTimeline(t *testing.T) {
	f, err := kongPlanParseForecastAccounts(`[{"id": 3, "k": 1, "depth_pp_per_hour": 2,
		"k_timeline": [{"from": "2026-10-23T15:00:00+08:00", "k": 1}, {"from": "2026-10-30T15:00:00+08:00", "k": 0.5}]},
		{"id": 4, "k": 0.25, "depth_pp_per_hour": 1, "k_timeline": []}, {"id": 5, "k": 1, "depth_pp_per_hour": 1, "k_timeline": null},
		{"id": 6, "k": 0.5, "depth_pp_per_hour": 1}]`)
	require.NoError(t, err)
	tl := f.Accounts[0].KTimeline
	require.Len(t, tl, 2)
	require.Equal(t, time.UTC, tl[1].From.Location(), "起点统一成 UTC")
	require.True(t, tl[1].From.Equal(kongPlanKSwitch))
	for _, a := range f.Accounts[1:] {
		require.Nil(t, a.KTimeline, "账号 %d：空数组、null 与缺省都是没有时间线", a.ID)
	}

	seg := func(segs string) string {
		return `[{"id": 3, "k": 1, "depth_pp_per_hour": 2, "k_timeline": [` + segs + `]}]`
	}
	for name, accounts := range map[string]string{
		"起点乱序":       seg(`{"from": "2026-10-30T07:00:00Z", "k": 0.5}, {"from": "2026-10-23T07:00:00Z", "k": 1}`),
		"起点相同（换时区写）": seg(`{"from": "2026-10-30T15:00:00+08:00", "k": 1}, {"from": "2026-10-30T07:00:00Z", "k": 0.5}`),
		"k 为 0":      seg(`{"from": "2026-10-30T07:00:00Z", "k": 0}`),
		"k 为负":       seg(`{"from": "2026-10-30T07:00:00Z", "k": -0.5}`),
		"缺 k":        seg(`{"from": "2026-10-30T07:00:00Z"}`),
		"缺起点":        seg(`{"k": 0.5}`),
		"起点为 null":   seg(`{"from": null, "k": 0.5}`),
	} {
		_, err := kongPlanParseForecastAccounts(accounts)
		appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
		require.Equal(t, "accounts", appErr.Metadata["field"], name)
	}
	_, err = kongPlanParseForecastAccounts(seg(`{"from": "2026-10-30T07:00:00Z", "k": 0.5, "basis": "legacy_pro"}`))
	appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
	require.Equal(t, "basis", appErr.Metadata["field"], "分段里的未知字段同样拒收")
	_, err = kongPlanParseForecastAccounts(seg(`{"from": "0000-01-01T00:00:00+01:00", "k": 0.5}`))
	requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
}

func TestKongPlanStore_AccountK(t *testing.T) {
	repo := newKongPlanFakeRepo()
	s, _ := newKongPlanTestStore(t, repo)
	_, ok := s.AccountK(3, kongPlanKSwitch)
	require.False(t, ok, "没有慢速部分")
	kongPlanPutForecast(t, s, `[{"id": 3, "k": 1, "depth_pp_per_hour": 2, "k_timeline": [{"from": "2026-10-30T15:00:00+08:00", "k": 0.5}]}]`)
	_, ok = s.AccountK(4, kongPlanKSwitch)
	require.False(t, ok, "慢速部分里没有这个账号")

	// 慢速部分算于 10-02、周期 60 分钟，到切换时早已过期；越过切换照样按时间线取。
	st := s.state.Load()
	require.Equal(t, "stale", kongPoolSlowStateOf(st, st.forecast, kongPlanKSwitch))
	k, ok := s.AccountK(3, kongPlanKSwitch.Add(-time.Second))
	require.True(t, ok)
	require.Equal(t, 1.0, k)
	k, ok = s.AccountK(3, kongPlanKSwitch.Add(time.Hour))
	require.True(t, ok)
	require.Equal(t, 0.5, k)

	m, ok := s.AccountMultiple(3, kongPlanKSwitch.Add(time.Hour))
	require.True(t, ok)
	require.Equal(t, 10.0, m, "没给单位倍数按 20：0.5 × 20")

	restarted, _ := newKongPlanTestStore(t, repo)
	k, ok = restarted.AccountK(3, kongPlanKSwitch)
	require.True(t, ok)
	require.Equal(t, 0.5, k, "重启后从库里读回时间线")

	// 单位改成 x10：同一个新 pro 的 K 是 1，倍数仍是 10。
	kongPlanPutForecastUnit(t, s, 10, `[{"id": 3, "k": 1, "depth_pp_per_hour": 2}]`)
	k, _ = s.AccountK(3, kongPlanKSwitch)
	m, _ = s.AccountMultiple(3, kongPlanKSwitch)
	require.Equal(t, 1.0, k)
	require.Equal(t, 10.0, m)
}

func TestParseKongPlanForecast_UnitMultiple(t *testing.T) {
	f, err := kongPlanParseForecastAccounts(`[]`)
	require.NoError(t, err)
	require.Equal(t, 20.0, f.UnitMultiple, "旧边车不送：按 20")
	f, err = ParseKongPlanForecast(strings.NewReader(kongPlanForecastWith(`"unit_multiple": null,`, `[]`)))
	require.NoError(t, err)
	require.Equal(t, 20.0, f.UnitMultiple)
	f, err = ParseKongPlanForecast(strings.NewReader(kongPlanForecastWith(`"unit_multiple": 10,`, `[]`)))
	require.NoError(t, err)
	require.Equal(t, 10.0, f.UnitMultiple)
	for _, bad := range []string{"0", "-10"} {
		_, err := ParseKongPlanForecast(strings.NewReader(kongPlanForecastWith(`"unit_multiple": `+bad+`,`, `[]`)))
		appErr := requireKongPlanErr(t, err, 400, KongPlanReasonInvalid)
		require.Equal(t, "unit_multiple", appErr.Metadata["field"], bad)
	}

	// 更早保存、没有这一项的慢速部分按 20。
	var stored KongPlanForecast
	require.NoError(t, json.Unmarshal([]byte(`{"cycle_id": "c", "accounts": []}`), &stored))
	require.Equal(t, 20.0, stored.unitMultiple())
}

// 同一次余额下降（625 credits = 2 点）在各规格上折成的本账号百分点；K 取不到时记未知。
func TestKongPlanApplyObservation_ConvertsByK(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	perPoint := 312.5
	for _, c := range []struct {
		name    string
		k, want float64
	}{{"老 pro", 1, 2}, {"新 pro", 0.5, 4}, {"promax", 1.25, 1.6}, {"plus", 0.05, 40}} {
		st := &kongPaceAccountState{}
		kongPlanApplyObservation(st, kongPlanObservation{At: t0, Balance: kongPlanF(1000)}, &perPoint, c.k, false)
		kongPlanApplyObservation(st, kongPlanObservation{At: t0.Add(time.Hour), Balance: kongPlanF(375)}, &perPoint, c.k, false)
		require.Len(t, st.Rises, 1, c.name)
		require.InDelta(t, c.want, st.Rises[0].Points, 1e-9, c.name)
		require.True(t, st.UnknownUntil.IsZero(), c.name)
	}

	st := &kongPaceAccountState{}
	kongPlanApplyObservation(st, kongPlanObservation{At: t0, Balance: kongPlanF(1000)}, &perPoint, 0, false)
	kongPlanApplyObservation(st, kongPlanObservation{At: t0.Add(time.Hour), Balance: kongPlanF(375)}, &perPoint, 0, false)
	require.Empty(t, st.Rises)
	require.Equal(t, t0.Add(time.Hour).Add(kongPlanUnknownHold), st.UnknownUntil, "套餐系数未知按未知记")
	require.Equal(t, 375.0, st.Balance.Value)
}

// 推进者按观测时刻的 K 换算；慢速部分里没有这个账号时记未知。
func TestKongPlanLedger_ConvertsByAccountK(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	switchAt := kongPaceT0.Add(30 * time.Minute)
	rows := []KongPaceAccountRow{kongPaceRow(1, "pro", 99, reset, kongPaceT0.Add(-time.Minute)),
		kongPaceRow(2, "pro", 99, reset, kongPaceT0.Add(-time.Minute))}
	h := newKongPlanLedgerHarness(t, rows, "2")
	kongPlanPutForecast(t, h.plan, fmt.Sprintf(`[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5,
		"k_timeline": [{"from": %q, "k": 0.5}]}]`, switchAt.Format(time.RFC3339)))
	h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.observe(t, 2, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.pace.factsTick()

	h.observe(t, 1, kongPaceT0.Add(time.Minute), kongPlanF(980))
	h.observe(t, 2, kongPaceT0.Add(time.Minute), kongPlanF(980))
	h.now = kongPaceT0.Add(2 * time.Minute)
	h.pace.factsTick()
	require.Equal(t, 10.0, kongPlanCreditPoints(h.state(t, 1)), "切换前：20 ÷ (2 × 1)")
	st2 := h.state(t, 2)
	require.Zero(t, kongPlanCreditPoints(st2))
	require.Equal(t, kongPaceT0.Add(time.Minute).Add(kongPlanUnknownHold), st2.UnknownUntil, "慢速部分里没有账号 2：记未知")

	h.observe(t, 1, switchAt.Add(10*time.Minute), kongPlanF(970))
	h.now = switchAt.Add(11 * time.Minute)
	h.pace.factsTick()
	// 切换前记的 10 折成新规格的 20；切换后的下降 10 ÷ (2 × 0.5) = 10。
	require.Equal(t, 30.0, kongPlanCreditPoints(h.state(t, 1)))
}

func TestKongPoolCapacity_SubscriptionUsesKTimeline(t *testing.T) {
	h := newKongPlanTierHarness(t)
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	capacity := func() float64 {
		view, err := kongPoolService(h.account(50, 10)).KongPoolCapacity(context.Background())
		require.NoError(t, err)
		return view.CapacityNow.ByLayer.Subscription.PPPerHour
	}
	h.forecastAccounts(t, "c", h.now, fmt.Sprintf(`[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5, "k_timeline": [{"from": %q, "k": 0.5}]}]`,
		ts(h.now.Add(-time.Hour))))
	require.Equal(t, 1.25, capacity(), "已生效的分段：min(阈值 − 用量, 深度线) × 0.5")
	h.forecastAccounts(t, "c", h.now, fmt.Sprintf(`[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5, "k_timeline": [{"from": %q, "k": 0.5}]}]`,
		ts(h.now.Add(time.Hour))))
	require.Equal(t, 2.5, capacity(), "未来的分段还不生效")
}

// credits 层：余额 ÷ (per_point × K) 是本账号百分点，与吞吐上限取小后再乘 K。余额不够时点数与 K 无关。
func TestKongPoolCapacity_CreditsConvertByK(t *testing.T) {
	h := newKongPlanTierHarness(t)
	ctl, err := ParseKongPlanControl(strings.NewReader(`{"revision": 2, "credits_mode": "on", "allow": [1], "floor": 0,
		"per_point": 1000, "overflow_margin": {"default": 1}}`))
	require.NoError(t, err)
	_, err = h.store.PutControl(context.Background(), ctl)
	require.NoError(t, err)
	for _, c := range []struct {
		k, want float64
	}{
		{1, 0.5},    // 500 credits = 0.5 点 < 吞吐上限 2.5 × 1
		{0.5, 0.5},  // 500 ÷ (1000 × 0.5) = 1 个本账号百分点 < 2.5，× 0.5 = 0.5 点
		{0.1, 0.25}, // 500 ÷ 100 = 5 个本账号百分点 > 2.5，吞吐上限起作用：2.5 × 0.1
	} {
		h.forecastAccounts(t, "c", h.now, fmt.Sprintf(`[{"id": 1, "k": %g, "depth_pp_per_hour": 2.5}]`, c.k))
		view, err := kongPoolService(h.account(99, 10)).KongPoolCapacity(context.Background())
		require.NoError(t, err)
		require.Len(t, view.CapacityNow.ByLayer.Credits, 1)
		lv := view.CapacityNow.ByLayer.Credits[0]
		require.True(t, lv.RateKnown)
		require.Equal(t, c.want, lv.PPPerHour, "k = %g", c.k)
	}
}

func TestKongPaceView_CapacityFromForecastK(t *testing.T) {
	plan, _ := newKongPlanTestStore(t, newKongPlanFakeRepo())
	kongPlanPutForecast(t, plan, `[{"id": 1, "k": 0.5, "depth_pp_per_hour": 2.5}, {"id": 3, "k": 1000, "depth_pp_per_hour": 2.5}]`)
	p, _, _ := kongPaceTestComponent(t, nil, kongPaceTestYAML)
	p.SetKongPlanLedger(NewKongPlanLedger(newKongPlanFakeLedgerStore(), plan))
	cfg := kongPaceTestConfig(t)
	view := func(id int64, plan string) kongPaceAccountView {
		return p.view(cfg, id, plan, 8, nil, 0, nil, nil, kongPaceT0)
	}

	newPro, oldPro := view(1, "pro"), view(2, "pro")
	require.Equal(t, 0.5, newPro.Capacity, "预测里的 K")
	require.Equal(t, 1.0, oldPro.Capacity, "预测里没有：按套餐表")
	require.InDelta(t, 0.5, newPro.Weight/oldPro.Weight, 1e-9, "其余条件相同，权重按 K 之比")
	lite := view(1, "prolite")
	require.Equal(t, 0.5, lite.Capacity)
	require.Equal(t, 10.0, lite.Yield, "让位量仍按套餐名")
	require.Equal(t, 0.25, view(2, "prolite").Capacity)
	require.Equal(t, 1.0, view(3, "pro").Capacity, "超出容量系数上限的 K 不用")

	// 单位改成 x10：K 一起翻倍，容量系数按套餐表的刻度（x20 为 1）不变，与按套餐表估的账号仍可比。
	kongPlanPutForecastUnit(t, plan, 10, `[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5}, {"id": 4, "k": 2, "depth_pp_per_hour": 2.5}]`)
	require.Equal(t, 0.5, view(1, "pro").Capacity, "新 pro：x10 ÷ 20")
	require.Equal(t, 1.0, view(4, "pro").Capacity, "老 pro：x20 ÷ 20")

	p.SetKongPlanLedger(nil)
	require.Equal(t, 1.0, view(1, "pro").Capacity, "没有计划组件：按套餐表")
}

func TestKongPaceApplyMultiple(t *testing.T) {
	st := &kongPaceAccountState{MaxUsed: 60, Rises: []kongPaceRise{{Points: 10}, {Points: 4, Credits: true}}}
	points := func() []float64 { return []float64{st.Rises[0].Points, st.Rises[1].Points} }
	apply := func(m float64) bool {
		_, rescaled := kongPaceApplyMultiple(st, m)
		return rescaled
	}
	require.False(t, apply(20), "第一次只记下")
	require.Equal(t, []float64{10, 4}, points())
	require.False(t, apply(20))
	require.False(t, apply(20*(1+1e-12)), "舍入误差不算变化")
	require.True(t, apply(10))
	require.Equal(t, []float64{20, 8}, points(), "含 credits 补记，都乘 旧倍数/新倍数")
	require.Equal(t, 60.0, st.MaxUsed, "读数基点照旧")
	require.Equal(t, 10.0, st.Multiple)
	require.False(t, apply(0), "取不到的倍数不记")
	require.Equal(t, 10.0, st.Multiple)
	require.True(t, apply(25))
	require.InDeltaSlice(t, []float64{8, 3.2}, points(), 1e-9)

	// 此前的版本记下的 k 按单位 20 理解，记下倍数时清掉。
	legacy := &kongPaceAccountState{LegacyK: 1, Rises: []kongPaceRise{{Points: 10}}}
	old, rescaled := kongPaceApplyMultiple(legacy, 20)
	require.Equal(t, 20.0, old)
	require.False(t, rescaled, "k = 1 即 x20，规格没变")
	require.Zero(t, legacy.LegacyK)
	legacy = &kongPaceAccountState{LegacyK: 1, Rises: []kongPaceRise{{Points: 10}}}
	_, rescaled = kongPaceApplyMultiple(legacy, 10)
	require.True(t, rescaled)
	require.Equal(t, 20.0, legacy.Rises[0].Points)
}

// kongPaceSwitchHarness：账号 1 是 pro，倍数在 switchAt 从 x20 降到 x10。第一拍以 40 建起点，第二拍读数涨到 50（旧规格的
// 10 个百分点）。
func kongPaceSwitchHarness(t *testing.T) (h *kongPlanLedgerHarness, reset, switchAt time.Time) {
	t.Helper()
	reset = kongPaceT0.Add(72 * time.Hour)
	switchAt = kongPaceT0.Add(30 * time.Minute)
	h = newKongPlanLedgerHarness(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 40, reset, kongPaceT0.Add(-time.Minute))}, "2")
	kongPlanPutForecast(t, h.plan, fmt.Sprintf(`[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5,
		"k_timeline": [{"from": %q, "k": 0.5}]}]`, switchAt.Format(time.RFC3339)))
	h.pace.factsTick()
	require.Equal(t, 20.0, h.state(t, 1).Multiple, "第一次只记下")
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 50, reset, kongPaceT0.Add(time.Minute))}
	h.now = kongPaceT0.Add(2 * time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, kongPaceIncrease(&st, h.now, 6*time.Hour))
	return h, reset, switchAt
}

// 规格倍数变化（套餐名不变）：已记的上涨（含 credits 补记）按 旧倍数/新倍数 折算，读数基点照旧；之后的读数照常按差额记，
// 重新载入不再折算。
func TestKongPaceFactsTick_MultipleChangeRescalesRises(t *testing.T) {
	h, reset, switchAt := kongPaceSwitchHarness(t)
	h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.observe(t, 1, kongPaceT0.Add(time.Minute), kongPlanF(980))
	h.now = kongPaceT0.Add(3 * time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, kongPlanCreditPoints(st), "首个观测只建基点，下降 20 ÷ (2 × 1)")

	h.now = switchAt.Add(time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, 10.0, st.Multiple)
	require.Equal(t, 40.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "同样的点数占新额度的两倍")
	require.Equal(t, 20.0, kongPlanCreditPoints(st))
	require.Equal(t, 50.0, st.MaxUsed, "读数基点照旧")
	require.Equal(t, int64(1), st.WindowSeq)

	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 53, reset, switchAt.Add(2*time.Minute))}
	h.now = switchAt.Add(3 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, 43.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "之后的读数按差额记")

	h.pace.statesReady = false // 下一拍从 Redis 重新载入
	h.now = switchAt.Add(4 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, 43.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "重新载入不二次折算")
}

// 切换后的第一拍同时收到新阶段的上涨：只折算切换前已记的，新上涨已是新规格的百分点。10 × 2 + 3 = 23。
func TestKongPaceFactsTick_SwitchTickNewRiseNotRescaled(t *testing.T) {
	h, reset, switchAt := kongPaceSwitchHarness(t)
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 53, reset, switchAt.Add(30*time.Second))}
	h.now = switchAt.Add(time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 23.0, kongPaceIncrease(&st, h.now, 6*time.Hour))
	require.Equal(t, 53.0, st.MaxUsed)

	h.pace.statesReady = false
	h.now = switchAt.Add(2 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, 23.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "重新载入不二次折算")
}

// 切换后的第一拍同时遇到自然重置：新窗口的读数是新规格的上涨，不折算。
func TestKongPaceFactsTick_SwitchTickWithReset(t *testing.T) {
	h, reset, switchAt := kongPaceSwitchHarness(t)
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 3, reset.Add(7*24*time.Hour), switchAt.Add(30*time.Second))}
	h.now = switchAt.Add(time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 23.0, kongPaceIncrease(&st, h.now, 6*time.Hour))
	require.Equal(t, 3.0, st.MaxUsed)
	require.Equal(t, int64(2), st.WindowSeq)
	require.Equal(t, 10.0, st.Multiple)
}

// 切换后的第一拍套餐名也变了：仍走换套餐处理——清掉已记的上涨、等新读数，记下当时的倍数；之后的上涨按新规格记、不再折算。
func TestKongPaceFactsTick_SwitchTickWithPlanChange(t *testing.T) {
	h, reset, switchAt := kongPaceSwitchHarness(t)
	kongPlanPutForecast(t, h.plan, `[{"id": 1, "k": 0.25, "depth_pp_per_hour": 2.5}]`)
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "prolite", 50, reset, kongPaceT0.Add(time.Minute))}
	h.now = switchAt.Add(time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, "prolite", st.Plan)
	require.Empty(t, st.Rises, "换套餐清掉已记的上涨")
	require.False(t, st.HasWindow, "等新套餐的读数")
	require.Equal(t, 5.0, st.Multiple)

	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "prolite", 12, reset, switchAt.Add(2*time.Minute))}
	h.now = switchAt.Add(3 * time.Minute)
	h.pace.factsTick()
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "prolite", 15, reset, switchAt.Add(4*time.Minute))}
	h.now = switchAt.Add(5 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.True(t, st.HasWindow)
	require.Equal(t, 3.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "新读数只建起点，之后按差额记")
}

// 内部单位从 x20 改成 x10：K 与 per_point 一起变，规格没变，已记的上涨不折算、余额补记的百分点不变；之后在新单位下降档
// 照常折算。
func TestKongPaceFactsTick_UnitChangeKeepsHistory(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	h := newKongPlanLedgerHarness(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 40, reset, kongPaceT0.Add(-time.Minute))}, "2")
	h.observe(t, 1, kongPaceT0.Add(-time.Hour), kongPlanF(1000))
	h.pace.factsTick()
	h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 50, reset, kongPaceT0.Add(time.Minute))}
	h.now = kongPaceT0.Add(2 * time.Minute)
	h.pace.factsTick()

	kongPlanPutForecastUnit(t, h.plan, 10, `[{"id": 1, "k": 2, "depth_pp_per_hour": 2.5}]`)
	ctl, err := ParseKongPlanControl(strings.NewReader(`{"revision": 2, "credits_mode": "on", "allow": [1], "floor": 0,
		"per_point": 1, "overflow_margin": {"default": 0}}`))
	require.NoError(t, err)
	_, err = h.plan.PutControl(context.Background(), ctl)
	require.NoError(t, err)
	h.observe(t, 1, kongPaceT0.Add(3*time.Minute), kongPlanF(980))
	h.now = kongPaceT0.Add(4 * time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 20.0, st.Multiple, "x20 账号在 x10 单位下 K = 2，倍数不变")
	require.Equal(t, 20.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "读数上涨 10 不折算；下降 20 ÷ (1 × 2) = 10")

	kongPlanPutForecastUnit(t, h.plan, 10, `[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5}]`)
	h.now = kongPaceT0.Add(5 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, 10.0, st.Multiple)
	require.Equal(t, 40.0, kongPaceIncrease(&st, h.now, 6*time.Hour), "单位不变、规格 x20 → x10：照常折算")
}

// 更早写下的节奏状态：没有倍数的第一次只记下，不折算；带 k 的按单位 20 理解。
func TestKongPaceState_LegacyData(t *testing.T) {
	reset := kongPaceT0.Add(72 * time.Hour)
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	read := kongPaceT0.Add(-time.Minute)
	legacy := func(extra string) []byte {
		return []byte(fmt.Sprintf(`{"plan": "pro", "has_window": true, "window_minutes": 10080, "reset_at": %q, "max_used": 50,
			"reading_at": %q, "seen_reading_at": %q, "processed_at": %q, "tracked_since": %q, %s
			"rises": [{"at": %q, "points": 10, "reading_at": %q}, {"at": %q, "points": 5, "reading_at": %q, "credits": true}],
			"window_seq": 3}`, ts(reset), ts(read), ts(read), ts(read), ts(kongPaceT0.Add(-48*time.Hour)), extra,
			ts(kongPaceT0.Add(-time.Hour)), ts(kongPaceT0.Add(-time.Hour)), ts(kongPaceT0.Add(-time.Hour)), ts(kongPaceT0.Add(-time.Hour))))
	}
	for _, c := range []struct {
		name  string
		extra string
		want  float64
	}{
		{"没有倍数与 k：只记下", "", 15},
		{"k = 0.5（x10）：规格没变", `"k": 0.5,`, 15},
		{"k = 1（x20）：降到 x10，折算", `"k": 1,`, 30},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newKongPlanLedgerHarness(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 50, reset, read)}, "2")
			kongPlanPutForecast(t, h.plan, `[{"id": 1, "k": 0.5, "depth_pp_per_hour": 2.5}]`)
			h.store.saved = map[int64][]byte{1: legacy(c.extra)}
			h.pace.factsTick()
			st := h.state(t, 1)
			require.Equal(t, 10.0, st.Multiple)
			require.Zero(t, st.LegacyK)
			require.Equal(t, c.want, kongPaceIncrease(&st, kongPaceT0, 6*time.Hour))
			require.Equal(t, int64(3), st.WindowSeq)
			require.Equal(t, 50.0, st.MaxUsed)
			saved := string(h.store.saved[1])
			require.Contains(t, saved, `"multiple":10`)
			require.NotContains(t, saved, `"k":`, "记下倍数后不再写 k")
		})
	}

	b, err := json.Marshal(kongPaceAccountState{Plan: "pro"})
	require.NoError(t, err)
	require.NotContains(t, string(b), `"multiple"`, "还没记过倍数的状态不写这个字段")
}

// kongPaceHookRepo 在查账号之前先跑一次 onList：模拟查询期间时间流逝、读数或余额观测到达。
type kongPaceHookRepo struct {
	*kongPaceFakeRepo
	onList func()
}

func (r *kongPaceHookRepo) ListOpenAIOAuthAccounts(ctx context.Context) ([]KongPaceAccountRow, error) {
	if f := r.onList; f != nil {
		r.onList = nil
		f()
	}
	return r.kongPaceFakeRepo.ListOpenAIOAuthAccounts(ctx)
}

// kongPlanHookLedgerStore 在读待处理观测之前先跑一次 onList：模拟推进者取了本拍时刻之后才登记的观测。
type kongPlanHookLedgerStore struct {
	*kongPlanFakeLedgerStore
	onList func()
}

func (s *kongPlanHookLedgerStore) ListObservations(ctx context.Context) ([][]byte, error) {
	if f := s.onList; f != nil {
		s.onList = nil
		f()
	}
	return s.kongPlanFakeLedgerStore.ListObservations(ctx)
}

// kongPaceCrossHarness：账号 1 是 pro，倍数在 switchAt 从 x20 降到 x10；第一拍以用量 40、余额 1000 建起点，per_point 为 2。
func kongPaceCrossHarness(t *testing.T) (h *kongPlanLedgerHarness, reset, switchAt time.Time) {
	t.Helper()
	reset = kongPaceT0.Add(72 * time.Hour)
	switchAt = kongPaceT0.Add(30 * time.Minute)
	h = newKongPlanLedgerHarness(t, []KongPaceAccountRow{kongPaceRow(1, "pro", 40, reset, kongPaceT0.Add(-time.Minute))}, "2")
	kongPlanPutForecast(t, h.plan, fmt.Sprintf(`[{"id": 1, "k": 1, "depth_pp_per_hour": 2.5,
		"k_timeline": [{"from": %q, "k": 0.5}]}]`, switchAt.Format(time.RFC3339)))
	h.observe(t, 1, kongPaceT0.Add(-time.Minute), kongPlanF(1000))
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 20.0, st.Multiple)
	require.Equal(t, 1000.0, st.Balance.Value)
	return h, reset, switchAt
}

// kongPaceRequireSettled 再跑一拍、再从 Redis 重新载入跑一拍，上涨都保持 want：新阶段的上涨不会在之后被再折算一次。
func kongPaceRequireSettled(t *testing.T, h *kongPlanLedgerHarness, switchAt time.Time, want float64) {
	t.Helper()
	h.pace.repo = h.repo
	h.now = switchAt.Add(time.Minute)
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, st.Multiple)
	require.Equal(t, want, kongPaceIncrease(&st, h.now, 6*time.Hour), "下一拍")
	h.pace.statesReady = false
	h.now = switchAt.Add(2 * time.Minute)
	h.pace.factsTick()
	st = h.state(t, 1)
	require.Equal(t, want, kongPaceIncrease(&st, h.now, 6*time.Hour), "重新载入")
}

// 一拍从切换前开始、查账号期间跨过切换，其间到达切换后的余额观测：本拍时刻在查完账号之后取，倍数与补记同在新规格。
// 下降 20 ÷ (2 × 0.5) = 20。
func TestKongPaceFactsTick_QueryCrossesSwitchBalance(t *testing.T) {
	h, _, switchAt := kongPaceCrossHarness(t)
	h.now = switchAt.Add(-time.Second)
	h.pace.repo = &kongPaceHookRepo{kongPaceFakeRepo: h.repo, onList: func() {
		h.now = switchAt.Add(time.Second)
		h.observe(t, 1, h.now, kongPlanF(980))
	}}
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, st.Multiple, "本拍时刻已过切换")
	require.Equal(t, 20.0, kongPlanCreditPoints(st))
	kongPaceRequireSettled(t, h, switchAt, 20)
}

// 一拍从切换前开始、查账号期间跨过切换，读到切换后的读数：40 → 43 是新规格的 3 个百分点。
func TestKongPaceFactsTick_QueryCrossesSwitchSubscription(t *testing.T) {
	h, reset, switchAt := kongPaceCrossHarness(t)
	h.now = switchAt.Add(-time.Second)
	h.pace.repo = &kongPaceHookRepo{kongPaceFakeRepo: h.repo, onList: func() {
		h.now = switchAt.Add(time.Second)
		h.repo.rows = []KongPaceAccountRow{kongPaceRow(1, "pro", 43, reset, h.now)}
	}}
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 10.0, st.Multiple)
	require.Equal(t, 3.0, kongPaceIncrease(&st, h.now, 6*time.Hour))
	kongPaceRequireSettled(t, h, switchAt, 3)
}

// 余额观测在查完账号、取了本拍时刻之后才登记，源时刻已过切换：这一拍不处理、留在待处理列表里，下一拍按新规格记。
func TestKongPaceFactsTick_ObservationAfterTickTimeDeferred(t *testing.T) {
	h, _, switchAt := kongPaceCrossHarness(t)
	hs := &kongPlanHookLedgerStore{kongPlanFakeLedgerStore: h.ls}
	h.ledger.store = hs
	h.now = switchAt.Add(-time.Second)
	hs.onList = func() { h.observe(t, 1, switchAt.Add(time.Second), kongPlanF(980)) }
	h.pace.factsTick()
	st := h.state(t, 1)
	require.Equal(t, 20.0, st.Multiple)
	require.Zero(t, kongPlanCreditPoints(st))
	require.Equal(t, 1000.0, st.Balance.Value, "基点不动")
	require.Len(t, h.ls.obs, 1, "留到下一拍")
	kongPaceRequireSettled(t, h, switchAt, 20)
	require.Empty(t, h.ls.obs)
}

// 观测的源时刻正好等于本拍时刻：这一拍就处理。等于切换时刻时已是新规格（分段起点含在内）；切换前一秒按旧规格记 10，
// 下一拍随切换折成 20。
func TestKongPaceFactsTick_ObservationAtTickTime(t *testing.T) {
	for _, c := range []struct {
		name           string
		offset         time.Duration
		mult, credited float64
	}{
		{"等于切换时刻", 0, 10, 20},
		{"切换前一秒", -time.Second, 20, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, switchAt := kongPaceCrossHarness(t)
			h.now = switchAt.Add(c.offset)
			h.observe(t, 1, h.now, kongPlanF(980))
			h.pace.factsTick()
			st := h.state(t, 1)
			require.Equal(t, c.mult, st.Multiple)
			require.Equal(t, c.credited, kongPlanCreditPoints(st))
			require.Empty(t, h.ls.obs)
			kongPaceRequireSettled(t, h, switchAt, 20)
		})
	}
}
