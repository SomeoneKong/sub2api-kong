//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 热账号限速由网关执行（DESIGN-openai-plan-dispatch.md 第 13 节）：契约、规则集合、名单的进出、生效与余额刷新。

const kongHotTestRules = `{"hot": {"mode": "on",
	"default": {"24h": {"light": [60, 50], "heavy": [70, 60], "pause": [90, 80]}},
	"light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {}},
	"balance_refresh": {"active_min": 30, "recent_traffic_min": 30, "ready_min": 110, "near_pp": 15, "concurrency": 2}}`

func kongHotDispatchBody(gen, seq int64, expires time.Time, rules string) string {
	extra := ""
	if rules != "" {
		extra = `, "gateway_rules": ` + rules
	}
	return fmt.Sprintf(`{"generation": %d, "seq": %d, "plan_version": 1, "cycle_id": "c", "fallback": [{"id": 1, "tier": "normal"}],
		"snapshot": {"expires_at": %q, "accounts": [{"id": 1, "tier": "normal"}]}%s}`, gen, seq, expires.UTC().Format(time.RFC3339), extra)
}

func TestKongHotContract_FingerprintUnchangedWithoutNewFields(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	req, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, "")))
	require.NoError(t, err)
	require.Nil(t, req.GatewayRules)
	old := kongPlanSHA256(struct {
		Generation  int64            `json:"generation"`
		Seq         int64            `json:"seq"`
		PlanVersion int64            `json:"plan_version"`
		CycleID     string           `json:"cycle_id"`
		Fallback    []KongPlanEntry  `json:"fallback"`
		Snapshot    KongPlanSnapshot `json:"snapshot"`
	}{req.Generation, req.Seq, req.PlanVersion, req.CycleID, req.Fallback, req.Snapshot})
	require.Equal(t, old, req.SHA256, "不带 gateway_rules 的发布与不认它的版本同一个指纹")

	ctl, err := ParseKongPlanControl(strings.NewReader(`{"revision": 2, "credits_mode": "on", "allow": [3, 1], "floor": 0, "per_point": 312.5,
		"overflow_margin": {"default": 1}, "file_hold": null}`))
	require.NoError(t, err)
	pp := 312.5
	oldCtl := kongPlanSHA256(struct {
		Revision       int64             `json:"revision"`
		CreditsMode    string            `json:"credits_mode"`
		Allow          []int64           `json:"allow"`
		Floor          float64           `json:"floor"`
		PerPoint       *float64          `json:"per_point"`
		OverflowMargin map[string]int    `json:"overflow_margin"`
		FileHold       *KongPlanFileHold `json:"file_hold"`
	}{2, "on", []int64{1, 3}, 0, &pp, map[string]int{"default": 1}, nil})
	require.Equal(t, oldCtl, ctl.SHA256, "不带热度开关的人工约束与旧版本同一个指纹：旧边车同步同一修订号不会冲突")

	ctl, err = ParseKongPlanControl(strings.NewReader(`{"revision": 3, "credits_mode": "on", "allow": [], "floor": 0, "per_point": null,
		"overflow_margin": {"default": 1}, "hot_off": true, "hot_exempt": [8, 2]}`))
	require.NoError(t, err)
	require.True(t, ctl.HotOff)
	require.Equal(t, []int64{2, 8}, ctl.HotExempt)
	_, err = ParseKongPlanControl(strings.NewReader(`{"revision": 3, "credits_mode": "on", "allow": [], "floor": 0, "per_point": null,
		"overflow_margin": {"default": 1}, "hot_exempt": [2, 2]}`))
	require.Error(t, err)
}

func TestKongHotContract_ParseRules(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	rules := `{"hot": {"mode": "observe",
		"default": {"24h": {"light": [60, 50], "heavy": [70, 60], "pause": [90, 80]}},
		"light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3,
		"schedule": {"8": [{"from": "2026-10-15T19:00:00+08:00", "to": "2026-10-16T19:00:00+08:00",
			"24h": {"light": [0, 0], "heavy": [40, 35], "pause": [48, 45]}, "6h": {"pause": [25, 20]}, "label": "新开通第 1 天"},
			{"from": "2026-10-16T19:00:00+08:00", "to": null, "24h": {"heavy": [50, 40]}}]}}}`
	req, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, rules)))
	require.NoError(t, err)
	h := req.GatewayRules.Hot
	require.Equal(t, KongHotModeObserve, h.Mode)
	require.Nil(t, req.GatewayRules.BalanceRefresh)
	segs := h.Schedule[8]
	require.Len(t, segs, 2)
	require.Equal(t, KongHotLine{48, 45}, *segs[0].H24.Pause)
	require.Equal(t, KongHotLine{25, 20}, *segs[0].H6.Pause)
	require.Nil(t, segs[1].To)
	require.Equal(t, "新开通第 1 天", segs[0].Label)
	at := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	seg, idx := h.segmentAt(8, at)
	require.Equal(t, 1, idx)
	require.Same(t, &segs[0], seg)
	seg, idx = h.segmentAt(8, at.Add(30*24*time.Hour))
	require.Equal(t, 2, idx)
	_, idx = h.segmentAt(9, at)
	require.Equal(t, 0, idx)

	base := `{"hot": {"mode": "on", "default": {"24h": {"light": [60, 50]}}, "light_fraction": 0.5, "heavy_sessions": 1,
		"heavy_concurrency": 3, "schedule": %s}}`
	for name, bad := range map[string]string{
		"退出高于进入":          fmt.Sprintf(base, `{"1": [{"24h": {"light": [40, 50]}}]}`),
		"进入超过 1000":       fmt.Sprintf(base, `{"1": [{"24h": {"light": [1001, 50]}}]}`),
		"不是二元":            fmt.Sprintf(base, `{"1": [{"24h": {"light": [40]}}]}`),
		"键不是编号":           fmt.Sprintf(base, `{"x": []}`),
		"起止反向":            fmt.Sprintf(base, `{"1": [{"from": "2026-10-16T00:00:00Z", "to": "2026-10-15T00:00:00Z", "24h": {"light": [40, 30]}}]}`),
		"没写窗口":            fmt.Sprintf(base, `{"1": [{"label": "x"}]}`),
		"窗口没写档":           fmt.Sprintf(base, `{"1": [{"6h": {}}]}`),
		"窗口名不认识":          fmt.Sprintf(base, `{"1": [{"12h": {"light": [40, 30]}}]}`),
		"模式不认识":           `{"hot": {"mode": "maybe", "default": {}, "light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {}}}`,
		"比例越界":            `{"hot": {"mode": "on", "default": {}, "light_fraction": 0, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {}}}`,
		"刷新间隔为 0":         `{"balance_refresh": {"active_min": 0, "recent_traffic_min": 30, "ready_min": 110, "near_pp": 15, "concurrency": 2}}`,
		"刷新并发越界":          `{"balance_refresh": {"active_min": 30, "recent_traffic_min": 30, "ready_min": 110, "near_pp": 15, "concurrency": 9}}`,
		"gateway_rules 空": `{}`,
	} {
		_, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, bad)))
		require.Error(t, err, name)
	}
	many := make([]string, kongHotMaxSegments+1)
	for i := range many {
		many[i] = `{"24h": {"light": [40, 30]}}`
	}
	_, err = ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, fmt.Sprintf(base, `{"1": [`+strings.Join(many, ",")+`]}`))))
	require.Error(t, err, "一个账号最多 8 段")
}

// 按账号的默认线（base）与超过 100 的线：解析、按账号取线、空 base 不改指纹、非法的 base 拒收。
func TestKongHotContract_Base(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	body := func(base string) string {
		return fmt.Sprintf(`{"hot": {"mode": "on", "default": {"24h": {"light": [60, 50], "heavy": [70, 60], "pause": [90, 80]}},
			%s"light_fraction": 0.75, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {"3": [{"24h": {"pause": [0, 0]}}]}}}`, base)
	}
	req, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp,
		body(`"base": {"3": {"24h": {"light": [90, 80], "heavy": [120, 105]}}, "4": {"24h": {"light": [90, 80], "heavy": [120, 105], "pause": [150, 135]}}}, `))))
	require.NoError(t, err)
	h := req.GatewayRules.Hot
	require.Equal(t, KongHotLine{150, 135}, *h.defaultFor(4).H24.Pause, "线可以超过 100")
	require.Equal(t, KongHotLine{90, 80}, *h.defaultFor(1).H24.Pause, "base 里没有的账号用全池默认")
	tiers := func(id int64, seg *KongHotSegment, idx int) map[string]KongHotLine {
		out := map[string]KongHotLine{}
		for _, r := range kongHotRuleSet(h, id, seg, idx) {
			out[fmt.Sprintf("%s/%d", kongHotTierNames[r.Tier], r.Source)] = r.Line
		}
		return out
	}
	forced := &h.Schedule[3][0]
	require.Equal(t, map[string]KongHotLine{"light/0": {90, 80}, "heavy/0": {120, 105}}, tiers(3, forced, 1),
		"账号的默认线缺暂停一档：时段的暂停也加不上")
	require.Equal(t, map[string]KongHotLine{"light/0": {60, 50}, "heavy/0": {70, 60}, "pause/0": {90, 80}, "pause/1": {0, 0}}, tiers(1, forced, 1))

	plain, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, body(""))))
	require.NoError(t, err)
	empty, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, body(`"base": {}, `))))
	require.NoError(t, err)
	require.Equal(t, plain.SHA256, empty.SHA256, "空的 base 不改指纹")

	for name, bad := range map[string]string{
		"键不是编号":   `"base": {"x": {"24h": {"light": [90, 80]}}}, `,
		"没写窗口":    `"base": {"3": {}}, `,
		"窗口没写档":   `"base": {"3": {"24h": {}}}, `,
		"超过 1000": `"base": {"3": {"24h": {"pause": [1001, 900]}}}, `,
		"退出高于进入":  `"base": {"3": {"24h": {"pause": [150, 160]}}}, `,
	} {
		_, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, body(bad))))
		require.Error(t, err, name)
	}
}

func kongHotTestRulesValue(t *testing.T, schedule string) *KongHotRules {
	t.Helper()
	body := fmt.Sprintf(`{"hot": {"mode": "on", "default": {"24h": {"light": [60, 50], "heavy": [70, 60], "pause": [90, 80]}},
		"light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": %s}}`, schedule)
	req, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, time.Now().Add(time.Hour), body)))
	require.NoError(t, err)
	return req.GatewayRules.Hot
}

func TestKongHotRuleSet(t *testing.T) {
	r := kongHotTestRulesValue(t, `{"8": [{"24h": {"heavy": [40, 35]}, "6h": {"pause": [25, 20]}}]}`)
	require.Len(t, kongHotRuleSet(r, 1, nil, 0), 3, "不在时段里只有默认的三条")
	seg, idx := r.segmentAt(8, time.Now())
	rules := kongHotRuleSet(r, 1, seg, idx)
	require.Len(t, rules, 5)
	require.Equal(t, kongHotRule{Tier: kongHotTierHeavy, Window: "24h", Line: KongHotLine{40, 35}, Source: 1}, rules[2])

	noPause := kongHotTestRulesValue(t, `{"8": [{"6h": {"pause": [25, 20]}}]}`)
	noPause.Default.H24.Pause = nil
	seg, idx = noPause.segmentAt(8, time.Now())
	for _, rr := range kongHotRuleSet(noPause, 1, seg, idx) {
		require.NotEqual(t, kongHotTierPause, rr.Tier, "默认里没有暂停一档，时段也加不上")
	}
	require.Nil(t, kongHotRuleSet(nil, 1, nil, 0))
}

func TestKongHotJudge_EnterExitAndHysteresis(t *testing.T) {
	now := time.Now()
	r := kongHotTestRulesValue(t, `{}`)
	rules := kongHotRuleSet(r, 1, nil, 0)
	st, ts := kongHotJudge(nil, rules, 10, 61, 0, "", now)
	require.Equal(t, 1, st.level())
	require.Len(t, ts, 1)
	require.Equal(t, "enter", ts[0].Kind)
	require.Equal(t, "light", ts[0].Tier)
	require.Equal(t, 60.0, ts[0].Line)
	st, ts = kongHotJudge(st, rules, 10, 55, 0, "", now.Add(time.Minute))
	require.Equal(t, 1, st.level(), "落在 50–60 之间留在轻度")
	require.Empty(t, ts)
	st, ts = kongHotJudge(st, rules, 10, 49, 0, "", now.Add(2*time.Minute))
	require.Equal(t, 0, st.level())
	require.Equal(t, "exit", ts[0].Kind)

	st, _ = kongHotJudge(nil, rules, 10, 95, 0, "", now)
	require.NotNil(t, st.Paused)
	require.True(t, st.SafetyPaused)
	require.Equal(t, 2, st.level())
	st, _ = kongHotJudge(st, rules, 10, 85, 0, "", now)
	require.NotNil(t, st.Paused, "暂停在 80–90 之间保持")
	require.True(t, st.SafetyPaused)

	st, ts = kongHotJudge(st, nil, 10, 85, 0, "", now)
	require.Nil(t, st, "没有规则时清空名单")
	require.Len(t, ts, 3, "三档各记一次退出")
}

func TestKongHotJudge_SegmentsAndForced(t *testing.T) {
	now := time.Now()
	r := kongHotTestRulesValue(t, `{"8": [{"24h": {"light": [0, 0], "heavy": [40, 35], "pause": [48, 45]}, "6h": {"pause": [25, 20]}, "label": "新开通第 1 天"}]}`)
	seg, idx := r.segmentAt(8, now)
	rules := kongHotRuleSet(r, 1, seg, idx)

	st, ts := kongHotJudge(nil, rules, 0, 0, idx, seg.Label, now)
	require.Equal(t, 1, st.level(), "[0, 0] 一进段就是轻度")
	require.Equal(t, "segment_enter", ts[0].Kind)
	require.Equal(t, "新开通第 1 天", ts[0].Label)
	require.Equal(t, 1, ts[1].Source)

	st, ts = kongHotJudge(st, rules, 10, 41, idx, seg.Label, now)
	require.Equal(t, 2, st.level())
	require.Equal(t, "heavy", ts[0].Tier)
	require.Equal(t, 1, ts[0].Source)

	st, _ = kongHotJudge(st, rules, 26, 41, idx, seg.Label, now)
	require.NotNil(t, st.Paused, "6h 的暂停线")
	require.False(t, st.SafetyPaused, "时段的暂停不算默认线的暂停")
	st, ts = kongHotJudge(st, rules, 19, 41, idx, seg.Label, now)
	require.Nil(t, st.Paused, "6h 降到退出线以下、24h 没到 48：解除")
	require.Equal(t, "pause", ts[0].Tier)
	require.Equal(t, "exit", ts[0].Kind)
	require.Equal(t, 2, st.level(), "限速照常按读数")

	forced := kongHotRuleSet(kongHotTestRulesValue(t, `{"8": [{"24h": {"pause": [0, 0]}}]}`), 8, &KongHotSegment{KongHotWindows: KongHotWindows{H24: &KongHotTiers{Pause: &KongHotLine{0, 0}}}}, 1)
	st, _ = kongHotJudge(nil, forced, 0, 0, 1, "", now)
	require.NotNil(t, st.Paused, "[0, 0] 的暂停不看读数")

	// 段结束：[0, 0] 消失，默认规则下用量 30 不在任何档。
	st, _ = kongHotJudge(nil, rules, 0, 30, idx, seg.Label, now)
	require.Equal(t, 1, st.level())
	st, ts = kongHotJudge(st, kongHotRuleSet(r, 1, nil, 0), 0, 30, 0, "", now)
	require.Equal(t, 0, st.level())
	require.Equal(t, "segment_exit", ts[0].Kind)
}

func TestKongHotContract_NullInLineRejected(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	for _, line := range []string{"[null, null]", "[60, null]", "[null, 0]"} {
		for _, where := range []string{
			`{"hot": {"mode": "on", "default": {"24h": {"pause": %s}}, "light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {}}}`,
			`{"hot": {"mode": "on", "default": {}, "light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {"1": [{"24h": {"pause": %s}}]}}}`,
		} {
			_, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp, fmt.Sprintf(where, line))))
			require.Error(t, err, line)
		}
	}
	_, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, 1, exp,
		`{"hot": {"mode": "on", "default": {}, "light_fraction": 0.5, "heavy_sessions": 1, "heavy_concurrency": 3, "schedule": {"1": [{"24h": {"pause": [0, 0]}}]}}}`)))
	require.NoError(t, err, "[0, 0] 照常接受")
}

func TestKongHotCap(t *testing.T) {
	for _, c := range []struct{ base, heavy, level, want int }{
		{8, 3, 1, 4}, {10, 3, 1, 5}, {5, 3, 1, 3}, {4, 1, 1, 2}, {2, 1, 1, 1}, {1, 1, 1, 1},
		{8, 3, 2, 3}, {2, 3, 2, 2}, {0, 1, 1, 1}, {0, 1, 2, 1}, {8, 3, 0, 8},
	} {
		require.Equal(t, c.want, kongHotCap(c.base, c.heavy, 0.5, c.level), "%+v", c)
	}
}

func (h *kongPlanTierHarness) publishRules(t *testing.T, rules string) {
	t.Helper()
	kongPlanTierSeq++
	req, err := ParseKongPlanDispatch(strings.NewReader(kongHotDispatchBody(1, kongPlanTierSeq, h.now.Add(time.Hour), rules)))
	require.NoError(t, err)
	h.store.now = func() time.Time { return h.now.UTC() }
	_, err = h.store.PutDispatch(context.Background(), req)
	require.NoError(t, err)
}

func (h *kongPlanTierHarness) setPaceHot(id int64, conc int, hot *kongHotState, at time.Time) {
	if hot != nil && hot.JudgedAt.IsZero() {
		hot.JudgedAt = at
	}
	st := kongPaceAccountState{Plan: "pro", HasWindow: true, WindowMinutes: 10080, ResetAt: h.reset, WindowSeq: 4, Hot: hot}
	h.pace.snapshot.Store(&kongPaceSnapshot{at: at, accounts: map[int64]kongPaceFacts{id: {Plan: "pro", Concurrency: conc, State: st}}})
}

func TestKongHotEffect_OverlayAndSwitches(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	acc := h.account(40, 0)
	acc.Concurrency = 8
	acc.Extra["max_sessions"] = 4
	since := h.now.Add(-time.Hour)

	h.setPaceHot(1, 8, &kongHotState{Light: &since}, h.now)
	require.Equal(t, 4, kongEffectiveConcurrencyByID(1, 8))
	require.Equal(t, 2, kongEffectiveMaxSessions(acc))
	res := h.rt.tier(ctx, acc, h.now)
	require.Equal(t, 2, *res.Entry.MaxSessions)
	require.Equal(t, 0, *res.Entry.Grace, "限速时宽限归零")
	require.False(t, res.Entry.Paused)
	require.Equal(t, 4, KongWaitConcurrency(1, 8), "排队重试读此刻的生效值")

	h.setPaceHot(1, 8, &kongHotState{Light: &since, Heavy: &since, Paused: &since}, h.now)
	require.Equal(t, 3, kongEffectiveConcurrencyByID(1, 8))
	require.Equal(t, 1, kongEffectiveMaxSessions(acc))
	res = h.rt.tier(ctx, acc, h.now)
	require.True(t, res.Entry.Paused)
	require.Equal(t, kongPlanSegHeld, kongPlanNaturalSeg(&kongPlanFacts{Tier: res}))

	h.setPaceHot(1, 8, &kongHotState{Heavy: &since, Paused: &since}, h.now.Add(-10*time.Minute))
	res = h.rt.tier(ctx, acc, h.now)
	require.False(t, res.Entry.Paused, "判断过期：不再暂停")
	require.Equal(t, 1, *res.Entry.MaxSessions, "限速按最后一次判断继续")

	h.setPaceHot(1, 8, &kongHotState{Heavy: &since, Paused: &since}, h.now)
	h.controlBody(t, `{"revision": 2, "credits_mode": "on", "allow": [1], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}, "hot_exempt": [1]}`)
	require.Equal(t, 8, kongEffectiveConcurrencyByID(1, 8), "豁免的账号不做")
	h.controlBody(t, `{"revision": 3, "credits_mode": "on", "allow": [1], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}, "hot_off": true}`)
	require.Equal(t, 8, kongEffectiveConcurrencyByID(1, 8), "全池关掉")
	h.controlBody(t, `{"revision": 4, "credits_mode": "on", "allow": [1], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}}`)
	require.Equal(t, 3, kongEffectiveConcurrencyByID(1, 8))

	h.publishRules(t, strings.Replace(kongHotTestRules, `"mode": "on"`, `"mode": "observe"`, 1))
	require.Equal(t, 8, kongEffectiveConcurrencyByID(1, 8), "观察模式只判断不生效")
	h.publishRules(t, "")
	require.Equal(t, 8, kongEffectiveConcurrencyByID(1, 8), "发布不带规则：由边车执行")

	h.publishRules(t, kongHotTestRules)
	_, err := h.store.PutPublish(ctx, &KongPlanPublishRequest{Action: "disable", Clear: "all", Note: "回到现状"})
	require.NoError(t, err)
	require.Equal(t, 8, kongEffectiveConcurrencyByID(1, 8), "回到现状调度时不做")
}

func TestKongHotStore_GatewayRulesLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	v, err := h.store.GetDispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, v.GatewayRules)
	require.Equal(t, KongHotModeOn, v.GatewayRules.Hot.Mode)

	_, err = h.store.PutPublish(ctx, &KongPlanPublishRequest{Action: "disable", Clear: "snapshot", Note: "清快照"})
	require.NoError(t, err)
	v, _ = h.store.GetDispatch(ctx)
	require.NotNil(t, v.GatewayRules, "清快照保留规则")
	require.Equal(t, KongPlanInUseFallback, v.InUse)

	// 重新载入（相当于网关重启）规则仍在。
	h.store.stale.Store(true)
	v, _ = h.store.GetDispatch(ctx)
	require.NotNil(t, v.GatewayRules)

	_, err = h.store.PutPublish(ctx, &KongPlanPublishRequest{Action: "disable", Clear: "all", Note: "清全部"})
	require.NoError(t, err)
	v, _ = h.store.GetDispatch(ctx)
	require.Nil(t, v.GatewayRules, "清全部删除规则")
}

func TestKongHotWaitConcurrency_OwnZeroIsUnlimited(t *testing.T) {
	// 账号自身并发 0（不限）、条目覆盖 12、重度：排队重试读 3，不沿用入队时的值
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	kongPlanTierSeq++
	body := strings.Replace(kongHotDispatchBody(1, kongPlanTierSeq, h.now.Add(time.Hour), kongHotTestRules),
		`"accounts": [{"id": 1, "tier": "normal"}]`, `"accounts": [{"id": 1, "tier": "normal", "max_concurrency": 12}]`, 1)
	req, err := ParseKongPlanDispatch(strings.NewReader(body))
	require.NoError(t, err)
	_, err = h.store.PutDispatch(context.Background(), req)
	require.NoError(t, err)
	since := h.now.Add(-time.Hour)
	h.setPaceHot(1, 0, &kongHotState{Heavy: &since}, h.now)
	require.Equal(t, 3, KongWaitConcurrency(1, 6))
	h.setPaceHot(1, 0, &kongHotState{Light: &since}, h.now)
	require.Equal(t, 6, KongWaitConcurrency(1, 99))
	h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: map[int64]kongPaceFacts{}})
	require.Equal(t, 7, KongWaitConcurrency(1, 7), "快照里没有这个账号：用记下的值")
}

func TestKongHotEffect_RestoredBeforeFirstTick(t *testing.T) {
	// 启动时读回了名单、第一次推进还没成功：限速照样按读回的名单，读回也给出同一份（不是 null）；暂停只在那份判断不超过
	// 5 分钟时生效
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	h.pace.snapshot.Store(nil)
	since := h.now.Add(-time.Hour)
	states := map[int64]*kongPaceAccountState{1: {Hot: &kongHotState{Heavy: &since, Paused: &since, JudgedAt: h.now.Add(-2 * time.Minute)}}}
	h.pace.restored.Store(kongHotRestoredFrom(states))
	e := kongHotEffectNow(1)
	require.Equal(t, 2, e.Level)
	require.True(t, e.Paused)
	st := h.store.current()
	ps := h.pace.hotState(1)
	v := h.rt.hotView(st, ps, kongHotEffectOf(st, ps, 1, h.now), h.account(40, 0), h.now)
	require.NotNil(t, v, "读回不能是 null：网关此刻按它暂停")
	require.True(t, v.EffectivePaused)
	require.Equal(t, "heavy", v.EffectiveLevel)
	states[1].Hot = &kongHotState{Heavy: &since, Paused: &since, JudgedAt: h.now.Add(-10 * time.Minute)}
	h.pace.restored.Store(kongHotRestoredFrom(states))
	e = kongHotEffectNow(1)
	require.Equal(t, 2, e.Level, "限速按最后一次判断继续")
	require.False(t, e.Paused)
	require.Nil(t, h.pace.hotState(2), "没有读回的账号：网关还没跟踪")
	h.setPaceHot(1, 0, nil, h.now)
	require.Equal(t, 0, kongHotEffectNow(1).Level, "有了快照就只看快照")
}

func TestKongHotRestore_LedgerFailureKeepsRestoredHot(t *testing.T) {
	// 账号状态读到了、入层记录读失败：这一拍不推进，读到的名单照样生效；旧版本没记判断时刻的，按上次处理的时刻算
	h := newKongPlanLedgerHarness(t, []KongPaceAccountRow{{ID: 1}}, "1")
	since := h.now.Add(-time.Hour)
	raw, err := json.Marshal(&kongPaceAccountState{ProcessedAt: h.now.Add(-2 * time.Minute), Hot: &kongHotState{Heavy: &since, Paused: &since}})
	require.NoError(t, err)
	h.store.saved = map[int64][]byte{1: raw}
	h.ls.loadErr = errors.New("redis down")
	require.False(t, h.pace.restoreStates(context.Background()))
	require.False(t, h.pace.statesReady)
	ps := h.pace.hotState(1)
	require.NotNil(t, ps)
	require.Equal(t, 2, ps.Hot.level())
	require.Equal(t, h.now.Add(-2*time.Minute), ps.Hot.JudgedAt)
	h.ls.loadErr = nil
	require.True(t, h.pace.restoreStates(context.Background()))
	require.True(t, h.pace.statesReady)
}

func TestKongHotPace_JudgeKeepsListWhilePlanUnknown(t *testing.T) {
	// 计划状态还没载入：不知道有没有规则，名单原样保留
	h := newKongPlanTierHarness(t)
	since := h.now.Add(-time.Hour)
	p := h.pace
	p.states = map[int64]*kongPaceAccountState{1: {Hot: &kongHotState{Light: &since}}}
	p.states[1].Hot.Paused, p.states[1].Hot.JudgedAt = &since, h.now.Add(-10*time.Minute)
	h.rt.store = NewKongPlanStore(newKongPlanFakeRepo()) // 没有 Start：没有载入
	p.judgeHot([]KongPaceAccountRow{{ID: 1}}, map[int64]bool{1: true}, h.now)
	require.NotNil(t, p.states[1].Hot)
	require.Equal(t, &since, p.states[1].Hot.Light)
	require.Equal(t, h.now.Add(-10*time.Minute), p.states[1].Hot.JudgedAt, "没有判断：判断时刻不更新，旧暂停不因此续期")
}

func TestKongHotJudge_ClearAndSegmentLabels(t *testing.T) {
	now := time.Now()
	tiers := KongHotTiers{Light: &KongHotLine{60, 50}}
	seg1 := &KongHotSegment{Label: "第 1 天", KongHotWindows: KongHotWindows{H24: &KongHotTiers{Light: &KongHotLine{30, 20}}}}
	rules := &KongHotRules{Default: KongHotWindows{H24: &tiers}}
	st, ts := kongHotJudge(nil, kongHotRuleSet(rules, 1, seg1, 1), 0, 40, 1, "第 1 天", now)
	require.NotNil(t, st.Light)
	require.Equal(t, "第 1 天", ts[0].Label)
	// 换到第 2 段：离开第 1 段的记录用第 1 段的标签
	_, ts = kongHotJudge(st, kongHotRuleSet(rules, 1, nil, 2), 0, 40, 2, "第 2 天", now.Add(time.Minute))
	require.Equal(t, "segment_exit", ts[0].Kind)
	require.Equal(t, 1, ts[0].Source)
	require.Equal(t, "第 1 天", ts[0].Label)
	// 规则清空：各档与所在时段都记退出，名单清空
	next, ts := kongHotJudge(st, nil, 0, 40, 0, "", now.Add(time.Minute))
	require.Nil(t, next)
	kinds := map[string]int{}
	for _, t := range ts {
		kinds[t.Kind+"/"+t.Tier]++
	}
	require.Equal(t, map[string]int{"segment_exit/": 1, "exit/light": 1}, kinds)
}

func TestKongHotLog_SqueezeExpiresByTime(t *testing.T) {
	l := &kongHotLogT{}
	now := time.Now()
	l.note(KongHotWaitRejected, now.Add(-72*time.Hour))
	l.note(KongHotWaitRejected, now.Add(-47*time.Hour))
	l.mu.Lock()
	got := l.kongHotSqueezeSince(now)
	l.mu.Unlock()
	require.Len(t, got, 1, "72 小时前的桶不再给出（没有新事件也过期）")
	l.note(KongHotWaitTimeout, now)
	require.Len(t, l.squeeze, 2, "追加时也按时刻裁")
}

func TestKongHotPace_JudgeInTick(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	p := h.pace
	now := h.now
	p.states = map[int64]*kongPaceAccountState{
		1: {Rises: []kongPaceRise{{At: now.Add(-time.Hour), Points: 75}}},
		2: {Rises: []kongPaceRise{{At: now.Add(-time.Hour), Points: 75}}},
		3: {TrackedSince: now.Add(-time.Minute), Rises: []kongPaceRise{{At: now.Add(-time.Minute), Points: 95}}},
	}
	ts := p.judgeHot([]KongPaceAccountRow{{ID: 1}, {ID: 2}, {ID: 3}}, map[int64]bool{1: true, 2: false, 3: true}, now)
	require.Equal(t, 2, p.states[1].Hot.level())
	require.Nil(t, p.states[2].Hot, "影子账号不判断")
	require.NotNil(t, p.states[3].Hot.Paused, "刚开始跟踪的账号到线就暂停，不等记满一个窗口")
	var got []string
	for _, tr := range ts {
		got = append(got, fmt.Sprintf("%d/%s/%s", tr.AccountID, tr.Tier, tr.Kind))
	}
	require.ElementsMatch(t, []string{"1/light/enter", "1/heavy/enter", "3/light/enter", "3/heavy/enter", "3/pause/enter"}, got)

	h.publishRules(t, "")
	p.judgeHot([]KongPaceAccountRow{{ID: 1}}, map[int64]bool{1: true}, now)
	require.Nil(t, p.states[1].Hot, "发布不再带规则：清空名单")
}

func TestKongHotNoteWait_CountsOnlyHotAccounts(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	before := len(kongHotLog.squeeze)
	h.setPaceHot(1, 8, nil, h.now)
	KongHotNoteWait(1, KongHotWaitTimeout)
	require.Equal(t, before, len(kongHotLog.squeeze), "没有限速的账号不计")
	since := h.now.Add(-time.Hour)
	h.setPaceHot(1, 8, &kongHotState{Heavy: &since}, h.now)
	KongHotNoteWait(1, KongHotWaitTimeout)
	KongHotNoteWait(1, "")
	l := KongHotLog(0)
	require.NotEmpty(t, l.Squeeze)
	last := l.Squeeze[len(l.Squeeze)-1]
	require.GreaterOrEqual(t, last.Timeout, int64(1))
}

func TestKongQuotaAccountLock_Serializes(t *testing.T) {
	ctx := context.Background()
	unlock := KongQuotaAccountLock(ctx, 77)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer KongQuotaAccountLock(ctx, 77)()
	}()
	select {
	case <-done:
		t.Fatal("同一账号的第二次加锁应当等第一次解锁")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-done
	other := KongQuotaAccountLock(ctx, 78)
	other()

	held := KongQuotaAccountLock(ctx, 79)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, ok := kongQuotaLock(short, 79)
	require.False(t, ok, "等锁时 ctx 结束：不再等")
	held()
	again, ok := kongQuotaLock(ctx, 79)
	require.True(t, ok, "等不到的那次没有占住锁")
	again()
}

type kongHotFakeRecoverer struct{ recovered chan struct{} }

func (r *kongHotFakeRecoverer) RecoverAccountState(context.Context, int64, AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error) {
	close(r.recovered)
	return &SuccessfulTestRecoveryResult{}, nil
}

type kongHotFakeResetQuota struct{ queries, writes int }

func (q *kongHotFakeResetQuota) QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error) {
	q.queries++
	return &OpenAIQuotaUsage{}, nil
}

func (q *kongHotFakeResetQuota) CachePostResetSnapshot(context.Context, int64, *OpenAIQuotaUsage) error {
	q.writes++
	return nil
}

func TestKongResetPostProcess_RecoversOutsideQuotaLock(t *testing.T) {
	// 后台刷新占着账号锁：用卡后照样先解除限流，额度查询等锁、期限到了按刷新失败返回；锁放开后查询与写入照常
	ctx := context.Background()
	held := KongQuotaAccountLock(ctx, 91)
	rec := &kongHotFakeRecoverer{recovered: make(chan struct{})}
	q := &kongHotFakeResetQuota{}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	res := RunOpenAIQuotaResetPostProcess(short, 91, q, rec, nil)
	require.True(t, res.AccountStateRecovered)
	require.Equal(t, OpenAIQuotaResetWarningCacheRefreshFailed, res.WarningCode)
	require.Zero(t, q.queries, "等不到锁不查")
	held()
	rec = &kongHotFakeRecoverer{recovered: make(chan struct{})}
	res = RunOpenAIQuotaResetPostProcess(ctx, 91, q, rec, nil)
	require.True(t, res.CacheRefreshed)
	require.Equal(t, 1, q.writes)
	free, cancelFree := context.WithTimeout(ctx, time.Second)
	defer cancelFree()
	again, ok := kongQuotaLock(free, 91)
	require.True(t, ok, "写完缓存锁已放")
	again()
}

func TestKongPlanBalanceRefresh_Select(t *testing.T) {
	ctx := context.Background()
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	h.controlBody(t, `{"revision": 2, "credits_mode": "on", "allow": [1, 2], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}}`)
	st := h.store.current()
	cfg := *st.dispatch.GatewayRules.BalanceRefresh
	recent := h.now.Add(-5 * time.Minute)
	mk := func(id int64, used float64, lastUsed *time.Time) Account {
		a := *h.account(used, 0)
		a.ID, a.LastUsedAt = id, lastUsed
		return a
	}
	setBalance := func(ids map[int64]time.Time) {
		accounts := map[int64]kongPaceFacts{}
		for id, at := range ids {
			accounts[id] = kongPaceFacts{Plan: "pro", State: kongPaceAccountState{Balance: &kongPlanBalancePoint{At: at, Value: 100}}}
		}
		h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: accounts})
	}
	classes := func(accts ...Account) map[int64]string {
		out := map[int64]string{}
		for _, d := range kongRefreshSelect(h.rt, h.store.current(), cfg, accts, h.now) {
			out[d.ID] = d.Class
		}
		return out
	}

	require.NoError(t, h.ledger.MarkEntered(ctx, 1, 4, h.now.Add(-2*time.Hour)))
	setBalance(map[int64]time.Time{1: h.now.Add(-40 * time.Minute), 2: h.now.Add(-40 * time.Minute)})
	require.Equal(t, map[int64]string{1: KongRefreshClassActive}, classes(mk(1, 99, &recent), mk(2, 40, &recent)),
		"在 credits 层且有流量：满 30 分钟就查；离阈值远的不查")

	setBalance(map[int64]time.Time{1: h.now.Add(-10 * time.Minute), 2: h.now.Add(-40 * time.Minute)})
	require.Empty(t, classes(mk(1, 99, &recent)), "刚查过的不查")

	idle := h.now.Add(-3 * time.Hour)
	setBalance(map[int64]time.Time{2: h.now.Add(-2 * time.Hour)})
	require.Equal(t, map[int64]string{2: KongRefreshClassReady}, classes(mk(2, 90, &idle)), "接近阈值、余额超过 110 分钟：待用")
	setBalance(map[int64]time.Time{})
	require.Equal(t, map[int64]string{2: KongRefreshClassReady}, classes(mk(2, 90, &idle)), "从没观测过：立即查")

	left := h.now.Add(-20 * time.Minute)
	require.NoError(t, h.ledger.MarkLeft(ctx, 1, 4, left))
	h.controlBody(t, `{"revision": 3, "credits_mode": "on", "allow": [2], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}}`)
	setBalance(map[int64]time.Time{1: h.now.Add(-40 * time.Minute)})
	require.Equal(t, map[int64]string{1: KongRefreshClassSettle}, classes(mk(1, 99, &recent)), "离层未结算：移出白名单也照样查")

	// 不在 credits 层、但最近一次余额观测比前一次低（换算率未知也一样）且有流量：在用
	accounts := map[int64]kongPaceFacts{2: {Plan: "pro", State: kongPaceAccountState{
		Balance: &kongPlanBalancePoint{At: h.now.Add(-40 * time.Minute), Value: 90}, BalanceDropAt: h.now.Add(-40 * time.Minute)}}}
	h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: accounts})
	require.Equal(t, map[int64]string{2: KongRefreshClassActive}, classes(mk(2, 40, &recent)), "余额在降：在用")

	r := NewKongPlanBalanceRefresher(nil, nil)
	r.Start() // 没有额度服务：不启动
}

func TestKongPlanBalanceObservation_DropFlag(t *testing.T) {
	now := time.Now()
	st := &kongPaceAccountState{}
	v := func(x float64) *float64 { return &x }
	kongPlanApplyObservation(st, kongPlanObservation{At: now, Balance: v(100)}, nil, 0, false)
	require.True(t, st.BalanceDropAt.IsZero(), "第一个观测只建立基点")
	kongPlanApplyObservation(st, kongPlanObservation{At: now.Add(time.Minute), Balance: v(90)}, nil, 0, false)
	require.Equal(t, now.Add(time.Minute), st.BalanceDropAt, "换算率未知也记下余额在降")
	kongPlanApplyObservation(st, kongPlanObservation{At: now.Add(2 * time.Minute)}, nil, 0, false)
	require.False(t, st.BalanceDropAt.IsZero(), "没有数值的观测不改")
	kongPlanApplyObservation(st, kongPlanObservation{At: now.Add(3 * time.Minute), Balance: v(90)}, nil, 0, false)
	require.True(t, st.BalanceDropAt.IsZero(), "持平：清掉")
}

type kongHotSlowQuota struct {
	kongHotFakeQuota
	running, peak int32
}

func (q *kongHotSlowQuota) QueryUsage(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	q.mu.Lock()
	q.running++
	if q.running > q.peak {
		q.peak = q.running
	}
	q.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	q.mu.Lock()
	q.running--
	q.mu.Unlock()
	return q.kongHotFakeQuota.QueryUsage(ctx, id)
}

func TestKongPlanBalanceRefresh_TickDrainsQueueWithinConcurrency(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	h.controlBody(t, `{"revision": 2, "credits_mode": "on", "allow": [1, 2, 3, 4, 5], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}}`)
	idle := h.now.Add(-3 * time.Hour)
	var list []Account
	for id := int64(1); id <= 5; id++ {
		a := *h.account(95, 0)
		a.ID, a.LastUsedAt = id, &idle
		list = append(list, a)
	}
	h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: map[int64]kongPaceFacts{}})
	q := &kongHotSlowQuota{}
	r := NewKongPlanBalanceRefresher(q, &kongPlanListRepo{stubQuotaAccountRepo: &stubQuotaAccountRepo{accounts: map[int64]*Account{}}, list: list})
	r.now = func() time.Time { return h.now }
	r.tick()
	require.Equal(t, 5, q.queries, "到期的 5 个都查")
	require.LessOrEqual(t, q.peak, int32(2), "同时查询的不超过 2 个")
}

func TestKongPlanBalanceRefresh_StopsWhenWithdrawn(t *testing.T) {
	// 第一个查询进行中发布撤销了余额刷新：本轮剩下的不再查，也不记失败
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	h.controlBody(t, `{"revision": 2, "credits_mode": "on", "allow": [201, 202, 203], "floor": 0, "per_point": null, "overflow_margin": {"default": 1}}`)
	idle := h.now.Add(-3 * time.Hour)
	var list []Account
	for id := int64(201); id <= 203; id++ {
		a := *h.account(95, 0)
		a.ID, a.LastUsedAt = id, &idle
		list = append(list, a)
	}
	h.pace.snapshot.Store(&kongPaceSnapshot{at: h.now, accounts: map[int64]kongPaceFacts{}})
	q := &kongHotWithdrawQuota{withdraw: func() { h.publishRules(t, "") }}
	b := kongRefreshConfigNow(h.rt)
	require.NotNil(t, b)
	h.publishRules(t, strings.Replace(kongHotTestRules, `"concurrency": 2`, `"concurrency": 1`, 1))
	r := NewKongPlanBalanceRefresher(q, &kongPlanListRepo{stubQuotaAccountRepo: &stubQuotaAccountRepo{accounts: map[int64]*Account{}}, list: list})
	r.now = func() time.Time { return h.now }
	r.tick()
	require.Equal(t, 1, q.queries, "撤销之后不再查")
	for id := int64(202); id <= 203; id++ {
		require.True(t, kongRefreshStatusAll.lastAttempt(id).IsZero(), "没查的不记尝试")
	}
}

type kongHotWithdrawQuota struct {
	kongHotFakeQuota
	withdraw func()
}

func (q *kongHotWithdrawQuota) QueryUsage(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	if q.withdraw != nil {
		q.withdraw()
		q.withdraw = nil
	}
	return q.kongHotFakeQuota.QueryUsage(ctx, id)
}

func TestKongPlanBalanceRefresh_RefreshWritesUnderLock(t *testing.T) {
	h := newKongPlanTierHarness(t)
	h.publishRules(t, kongHotTestRules)
	q := &kongHotFakeQuota{}
	r := NewKongPlanBalanceRefresher(q, nil)
	unlock := KongQuotaAccountLock(context.Background(), 5)
	done := make(chan error, 1)
	go func() { done <- r.refresh(context.Background(), 5) }()
	time.Sleep(30 * time.Millisecond)
	q.mu.Lock()
	require.Zero(t, q.queries, "账号锁被占着时不查")
	q.mu.Unlock()
	unlock()
	require.NoError(t, <-done)
	require.Equal(t, 1, q.queries)
	require.Equal(t, 1, q.credits)
	require.Equal(t, 1, q.cards)

	// 等锁期间撤销了余额刷新：拿到锁后不查
	unlock = KongQuotaAccountLock(context.Background(), 5)
	go func() { done <- r.refresh(context.Background(), 5) }()
	time.Sleep(30 * time.Millisecond)
	h.publishRules(t, "")
	unlock()
	require.ErrorIs(t, <-done, errKongRefreshWithdrawn)
	require.Equal(t, 1, q.queries)
}

type kongHotFakeQuota struct {
	mu                      sync.Mutex
	queries, credits, cards int
}

func (q *kongHotFakeQuota) QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries++
	return &OpenAIQuotaUsage{}, nil
}

func (q *kongHotFakeQuota) CacheCreditsSnapshot(context.Context, int64, *OpenAIQuotaUsage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.credits++
	return nil
}

func (q *kongHotFakeQuota) CacheResetCreditsSnapshot(context.Context, int64, *OpenAIRateLimitResetCredits) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cards++
	return nil
}

// 共用夹具 testdata/kong_plan/hot_judge.json：推演内核（prototype/kernel/src/hot.rs）跑同一份。
func TestKongHotJudge_SharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/kong_plan/hot_judge.json")
	require.NoError(t, err)
	// base：按账号的默认线（账号编号 → 近 24 小时三档）；步骤的 account 缺省为 1，各账号各自一份状态
	var cases []struct {
		Name     string                  `json:"name"`
		Default  KongHotTiers            `json:"default"`
		Base     map[string]KongHotTiers `json:"base"`
		Segments []KongHotWindows        `json:"segments"`
		Steps    []struct {
			Account      int64   `json:"account"`
			Hour         int64   `json:"hour"`
			Inc6         float64 `json:"inc6"`
			Inc24        float64 `json:"inc24"`
			Segment      int     `json:"segment"`
			Light        bool    `json:"light"`
			Heavy        bool    `json:"heavy"`
			Paused       bool    `json:"paused"`
			SafetyPaused bool    `json:"safety_paused"`
		} `json:"steps"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range cases {
		d := c.Default
		rules := &KongHotRules{Default: KongHotWindows{H24: &d}}
		if d == (KongHotTiers{}) {
			rules.Default = KongHotWindows{}
		}
		for k, v := range c.Base {
			id, err := strconv.ParseInt(k, 10, 64)
			require.NoError(t, err)
			if rules.Base == nil {
				rules.Base = map[int64]KongHotWindows{}
			}
			tiers := v
			rules.Base[id] = KongHotWindows{H24: &tiers}
		}
		st := map[int64]*kongHotState{}
		for i, s := range c.Steps {
			id := s.Account
			if id == 0 {
				id = 1
			}
			var seg *KongHotSegment
			if s.Segment > 0 {
				seg = &KongHotSegment{KongHotWindows: c.Segments[s.Segment-1]}
			}
			st[id], _ = kongHotJudge(st[id], kongHotRuleSet(rules, id, seg, s.Segment), s.Inc6, s.Inc24, s.Segment, "",
				base.Add(time.Duration(s.Hour)*time.Hour))
			var got [4]bool
			if h := st[id]; h != nil {
				got = [4]bool{h.Light != nil, h.Heavy != nil, h.Paused != nil, h.SafetyPaused}
			}
			require.Equal(t, [4]bool{s.Light, s.Heavy, s.Paused, s.SafetyPaused}, got, "%s 第 %d 步", c.Name, i+1)
		}
	}
}
