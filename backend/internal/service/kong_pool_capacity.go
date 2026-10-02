package service

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 容量视图（fork 专有，设计见 DESIGN-account-selection.md 第 4.5 节、实施计划第 2.7 节）：当前能力由网关按此刻实际
// 所用的输入与实时读数现算，慢速部分（预报、续航、需求估计）取边车最近一次写入的那份并标明新旧。端点不返回 503。

// kongPoolDefaultPerSession 是没有慢速部分时的每会话速度（安全线缺省值）。
const kongPoolDefaultPerSession = 1.5

// KongPoolCapacity 是 GET /api/v1/pool/capacity 的响应。
type KongPoolCapacity struct {
	AsOf                    time.Time           `json:"as_of"`
	PlanVersion             *int64              `json:"plan_version"`
	CycleID                 *string             `json:"cycle_id"`
	Inputs                  KongPoolInputs      `json:"inputs"`
	CapacityNow             KongPoolCapacityNow `json:"capacity_now"`
	OnCredits               bool                `json:"on_credits"`
	CreditsRunwayH          json.RawMessage     `json:"credits_runway_h"`
	Forecast                json.RawMessage     `json:"forecast"`
	RunwayH                 json.RawMessage     `json:"runway_h"`
	NextResetAt             *time.Time          `json:"next_reset_at"`
	DemandEstimatePPPerHour *float64            `json:"demand_estimate_pp_per_hour"`
	Slow                    KongPoolSlowState   `json:"slow"`
	Advice                  KongPoolAdvice      `json:"advice"`
}

// KongPoolInputs 是网关此刻实际所用的输入。
type KongPoolInputs struct {
	InUse       string `json:"in_use"`
	Generation  int64  `json:"generation"`
	Seq         *int64 `json:"seq"`
	CreditsMode string `json:"credits_mode"`
}

// KongPoolCapacityNow 是当前能力。
type KongPoolCapacityNow struct {
	PPPerHour           float64         `json:"pp_per_hour"`
	Sessions            int             `json:"sessions"`
	SessionsActive      int             `json:"sessions_active"`
	AccountsServiceable int             `json:"accounts_serviceable"`
	PerSessionPPPerHour float64         `json:"per_session_pp_per_hour"`
	AvgSessionPP        *float64        `json:"avg_session_pp"`
	ByLayer             KongPoolByLayer `json:"by_layer"`
}

// KongPoolByLayer 是第一层与 credits 各级分开的可承接速度。
type KongPoolByLayer struct {
	Subscription KongPoolLayerCap     `json:"subscription"`
	Credits      []KongPoolCreditsCap `json:"credits"`
}

// KongPoolLayerCap 是一层的可承接速度与账号数。
type KongPoolLayerCap struct {
	PPPerHour float64 `json:"pp_per_hour"`
	Accounts  int     `json:"accounts"`
}

// KongPoolCreditsCap 是 credits 一级的可承接速度；BalanceCredits 在有账号没有数值观测时为 null。
type KongPoolCreditsCap struct {
	Level          int      `json:"level"`
	PPPerHour      float64  `json:"pp_per_hour"`
	Accounts       int      `json:"accounts"`
	BalanceCredits *float64 `json:"balance_credits"`
	RateKnown      bool     `json:"rate_known"`
}

// KongPoolSlowState 是慢速部分的来源与新旧：fresh / degraded / stale。
type KongPoolSlowState struct {
	ComputedAt *time.Time `json:"computed_at"`
	CycleID    *string    `json:"cycle_id"`
	State      string     `json:"state"`
}

// KongPoolAdvice 是限流建议：normal / throttle / pause。
type KongPoolAdvice struct {
	Level     string     `json:"level"`
	Sessions  int        `json:"sessions"`
	PPPerHour float64    `json:"pp_per_hour"`
	Until     *time.Time `json:"until"`
	Reason    string     `json:"reason,omitempty"`
}

type kongPoolForecastPoint struct {
	From      time.Time `json:"from"`
	PPPerHour float64   `json:"pp_per_hour"`
}

// KongPoolCapacity 按此刻的输入与读数算容量视图。
func (s *OpenAIGatewayService) KongPoolCapacity(ctx context.Context) (*KongPoolCapacity, error) {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil || s.accountRepo == nil {
		return nil, infraerrors.ServiceUnavailable(KongPlanReasonUnavailable, "计划组件未启用")
	}
	now := rt.now()
	st := rt.store.current()
	if st == nil {
		st = kongPlanDefaultState()
	}
	out := &KongPoolCapacity{AsOf: now.UTC(), Inputs: KongPoolInputs{InUse: st.inUse(now), Generation: st.publish.Generation,
		CreditsMode: st.control.CreditsMode}, CreditsRunwayH: json.RawMessage("null"), Forecast: json.RawMessage("null"),
		RunwayH: json.RawMessage("null")}
	if d := st.dispatch; d != nil {
		seq, ver, cycle := d.Seq, d.PlanVersion, d.CycleID
		out.Inputs.Seq, out.PlanVersion, out.CycleID = &seq, &ver, &cycle
	}
	fc := st.forecast
	perSession := kongPoolDefaultPerSession
	depth := map[int64]KongPlanForecastAccount{}
	if fc != nil {
		if fc.PerSessionPPPerHour > 0 {
			perSession = fc.PerSessionPPPerHour
		}
		for _, a := range fc.Accounts {
			depth[a.ID] = a
		}
	}

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	cn := &out.CapacityNow
	cn.PerSessionPPPerHour = perSession
	cn.ByLayer.Credits = []KongPoolCreditsCap{}
	levels := map[int]*KongPoolCreditsCap{}
	perPoint := st.control.PerPoint
	var limited []*Account
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAIOAuth() {
			continue
		}
		if kongSessionLimited(acc) {
			limited = append(limited, acc)
		}
		p, ok := depth[acc.ID]
		if !ok || !rt.serviceable(ctx, acc, now) {
			continue
		}
		res := rt.tier(ctx, acc, now)
		capPP := p.DepthPPPerHour
		if limit := kongPlanEffectiveMaxSessions(res.Entry, acc); limit > 0 {
			capPP = math.Min(capPP, float64(limit)*perSession)
		}
		switch res.Layer {
		case kongPlanLayerSubscription:
			used := 0.0 // 与 A 路径同一种读法：重置时刻已过、或读数陈旧又没有待到的重置时，旧用量不再算数
			if u, ok := resolveOpenAIQuotaUtilization(acc.Extra, "7d", now); ok {
				used = u * 100
			}
			room := rt.windowThresholdPercent(ctx, acc, "7d", now) - used
			v := math.Max(0, math.Min(room, capPP)) * p.K
			if v <= 0 {
				continue
			}
			cn.ByLayer.Subscription.PPPerHour += v
			cn.ByLayer.Subscription.Accounts++
		case kongPlanLayerCredits:
			lv := levels[res.Entry.Credits.Level]
			if lv == nil {
				zero := 0.0
				lv = &KongPoolCreditsCap{Level: res.Entry.Credits.Level, BalanceCredits: &zero, RateKnown: perPoint != nil}
				levels[lv.Level] = lv
			}
			ps, _ := rt.paceState(acc.ID)
			var committed *kongPlanBalancePoint
			if ps != nil {
				committed = ps.Balance
			}
			v := capPP
			if bal := rt.ledger.LatestBalance(acc.ID, committed); bal != nil {
				if lv.BalanceCredits != nil {
					sum := *lv.BalanceCredits + bal.Value
					lv.BalanceCredits = &sum
				}
				if perPoint != nil && *perPoint > 0 {
					v = math.Min(v, math.Max(0, bal.Value-st.control.Floor) / *perPoint)
				}
			} else {
				lv.BalanceCredits = nil
			}
			v *= p.K
			lv.PPPerHour += v
			lv.Accounts++
		default:
			continue
		}
		cn.AccountsServiceable++
	}
	for _, lv := range levels {
		cn.ByLayer.Credits = append(cn.ByLayer.Credits, *lv)
	}
	sort.Slice(cn.ByLayer.Credits, func(i, j int) bool { return cn.ByLayer.Credits[i].Level < cn.ByLayer.Credits[j].Level })
	creditsPP := 0.0
	for _, lv := range cn.ByLayer.Credits {
		creditsPP += lv.PPPerHour
	}
	cn.PPPerHour = kongPoolRound(cn.ByLayer.Subscription.PPPerHour + creditsPP)
	cn.ByLayer.Subscription.PPPerHour = kongPoolRound(cn.ByLayer.Subscription.PPPerHour)
	for i := range cn.ByLayer.Credits {
		cn.ByLayer.Credits[i].PPPerHour = kongPoolRound(cn.ByLayer.Credits[i].PPPerHour)
	}
	cn.Sessions = int(math.Floor(cn.PPPerHour/perSession + 1e-9))
	cn.SessionsActive = s.kongPoolActiveSessions(ctx, limited)
	out.OnCredits = cn.ByLayer.Subscription.Accounts == 0 && creditsPP > 0

	var points []kongPoolForecastPoint
	if fc != nil {
		avg := fc.AvgSessionPP
		cn.AvgSessionPP = &avg
		out.CreditsRunwayH, out.Forecast, out.RunwayH = kongPoolRaw(fc.CreditsRunwayH), kongPoolRaw(fc.Forecast), kongPoolRaw(fc.RunwayH)
		out.NextResetAt = fc.NextResetAt
		demand := fc.DemandEstimatePPPerHour
		out.DemandEstimatePPPerHour = &demand
		computed, cycle := fc.ComputedAt.UTC(), fc.CycleID
		out.Slow.ComputedAt, out.Slow.CycleID = &computed, &cycle
		_ = json.Unmarshal(fc.Forecast, &points)
	}
	out.Slow.State = kongPoolSlowStateOf(st, fc, now)
	out.Advice = kongPoolAdviceOf(out, points)
	return out, nil
}

// serviceable 是"此刻可服务"：状态与开关、临时停调、限流、过载，以及 A、B 两条暂停路径（credits 层只豁免 7d）。
// WS 逐轮复核与容量视图共用。
func (rt *kongPlanRuntime) serviceable(ctx context.Context, account *Account, now time.Time) bool {
	if !account.IsSchedulable() {
		return false
	}
	ctx = rt.withAutoPause(ctx)
	if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		return false
	}
	var thresholds map[string]int
	if rt.thresholds != nil {
		thresholds = rt.thresholds(ctx)
	}
	return !EvaluateAccountSchedulingThreshold(account, thresholds, now).ShouldPause
}

// kongPoolActiveSessions 是受会话上限约束的账号此刻的活动会话数之和；读不到时为 0。
func (s *OpenAIGatewayService) kongPoolActiveSessions(ctx context.Context, accounts []*Account) int {
	if s.kongSessionLimit == nil || len(accounts) == 0 {
		return 0
	}
	ids := make([]int64, 0, len(accounts))
	timeouts := make(map[int64]time.Duration, len(accounts))
	for _, acc := range accounts {
		ids = append(ids, acc.ID)
		timeouts[acc.ID] = kongSessionIdleTimeout(acc)
	}
	counts, err := s.kongSessionLimit.cache.GetActiveSessionCountBatch(ctx, ids, timeouts)
	if err != nil {
		return 0
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// kongPoolSlowStateOf：fresh 与最近一次接受的发布同一周期、快照未过期，且距源事实时刻不超过 2 个周期；stale 超过
// 2 个周期或从未收到；其余 degraded（周期不一致，含主模式 shadow 时没有发布；或快照已过期）。
func kongPoolSlowStateOf(st *kongPlanState, fc *KongPlanForecast, now time.Time) string {
	if fc == nil {
		return "stale"
	}
	interval := time.Duration(fc.IntervalMin) * time.Minute
	if interval <= 0 {
		interval = time.Hour
	}
	if now.Sub(fc.ComputedAt) > 2*interval {
		return "stale"
	}
	if st.dispatch == nil || st.dispatch.CycleID != fc.CycleID || st.inUse(now) != KongPlanInUseSnapshot {
		return "degraded"
	}
	return "fresh"
}

// kongPoolAdviceOf 按当前能力给限流建议：能力为 0 是 pause，低于需求估计是 throttle；until 取预报里第一个回升的点。
func kongPoolAdviceOf(c *KongPoolCapacity, points []kongPoolForecastPoint) KongPoolAdvice {
	cn := c.CapacityNow
	a := KongPoolAdvice{Level: "normal", Sessions: cn.Sessions, PPPerHour: cn.PPPerHour}
	var reasons []string
	switch {
	case c.DemandEstimatePPPerHour == nil:
		reasons = append(reasons, "还没有慢速部分，深度线与需求估计未知，当前能力只计有参数的账号")
	case cn.PPPerHour <= 0:
		a.Level = "pause"
		a.Until = kongPoolFirstPoint(points, c.AsOf, func(v float64) bool { return v > 0 })
	case cn.PPPerHour < *c.DemandEstimatePPPerHour:
		a.Level = "throttle"
		demand := *c.DemandEstimatePPPerHour
		a.Until = kongPoolFirstPoint(points, c.AsOf, func(v float64) bool { return v >= demand })
	}
	if c.OnCredits {
		reasons = append(reasons, "只剩 credits 层可服务")
		for _, lv := range cn.ByLayer.Credits {
			if !lv.RateKnown {
				reasons = append(reasons, "credits 续航无法估计")
				break
			}
		}
	}
	a.Reason = strings.Join(reasons, "；")
	return a
}

func kongPoolFirstPoint(points []kongPoolForecastPoint, after time.Time, ok func(float64) bool) *time.Time {
	for _, p := range points {
		if p.From.After(after) && ok(p.PPPerHour) {
			t := p.From.UTC()
			return &t
		}
	}
	return nil
}

func kongPoolRaw(m json.RawMessage) json.RawMessage {
	if len(m) == 0 {
		return json.RawMessage("null")
	}
	return m
}

func kongPoolRound(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 2, 64), 64)
	return r
}
