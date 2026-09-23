package service

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// 发布里网关要执行的规则（fork 专有，设计见 DESIGN-openai-plan-dispatch.md 第 13 节）：热账号限速的默认档与按账号
// 时段、credits 余额刷新的参数。随整份发布替换，快照过期不影响它，清空全部时删除。

const (
	KongHotModeObserve = "observe"
	KongHotModeOn      = "on"

	kongHotMaxSegments    = 8
	kongHotMaxRefreshMin  = 24 * 60
	kongHotMaxRefreshConc = 8
	// kongHotLineMax 是线的上限：近 24 / 6 小时用了一周额度的百分比可以超过 100（用 credits、用卡、碰上全局 reset），
	// 上限只防手误，不代表合理取值
	kongHotLineMax = 1000
)

// KongHotLine 是一档的线：[进入, 退出]，本账号一周额度的百分比。到进入线进这一档，降到退出线以下才可能退出；
// [0, 0] 表示所在时段内一直在这一档。
type KongHotLine [2]float64

func (l KongHotLine) enter() float64 { return l[0] }
func (l KongHotLine) exit() float64  { return l[1] }

// KongHotTiers 是一个窗口三档的线，没写的档为 nil。
type KongHotTiers struct {
	Light *KongHotLine `json:"light,omitempty"`
	Heavy *KongHotLine `json:"heavy,omitempty"`
	Pause *KongHotLine `json:"pause,omitempty"`
}

// KongHotWindows 是按近 24h、近 6h 用量各自的线。
type KongHotWindows struct {
	H24 *KongHotTiers `json:"24h,omitempty"`
	H6  *KongHotTiers `json:"6h,omitempty"`
}

// KongHotSegment 是一个账号的一个时段：[From, To)，nil 表示这一端不限。Label 只给人看。
type KongHotSegment struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
	KongHotWindows
	Label string `json:"label,omitempty"`
}

func (s *KongHotSegment) covers(now time.Time) bool {
	return (s.From == nil || !now.Before(*s.From)) && (s.To == nil || now.Before(*s.To))
}

// KongHotRules 是热账号限速的规则：账号的默认规则一直有效（Base 里有这个账号就用它的，否则用 Default），此刻所在时段的
// 规则另加在上面；手段沿用安全线。Base 由人在安全线里按账号设，计划改不动。
type KongHotRules struct {
	Mode             string                     `json:"mode"`
	Default          KongHotWindows             `json:"default"`
	Base             map[int64]KongHotWindows   `json:"base,omitempty"`
	LightFraction    float64                    `json:"light_fraction"`
	HeavySessions    int                        `json:"heavy_sessions"`
	HeavyConcurrency int                        `json:"heavy_concurrency"`
	Schedule         map[int64][]KongHotSegment `json:"schedule"`
}

// defaultFor 返回账号的默认规则：Base 里有就用 Base 的（三档都取它的，不看全池默认缺哪档），否则用全池默认。
func (r *KongHotRules) defaultFor(id int64) KongHotWindows {
	if w, ok := r.Base[id]; ok {
		return w
	}
	return r.Default
}

// segmentAt 返回账号此刻所在的时段与它在列表里的序号（从 1 起）；不在任何段里返回 nil, 0。时段不重叠由边车校验，
// 万一重叠取第一个。
func (r *KongHotRules) segmentAt(id int64, now time.Time) (*KongHotSegment, int) {
	if r == nil {
		return nil, 0
	}
	segs := r.Schedule[id]
	for i := range segs {
		if segs[i].covers(now) {
			return &segs[i], i + 1
		}
	}
	return nil, 0
}

// KongBalanceRefresh 是网关查 credits 余额的参数（分钟、百分点）。
type KongBalanceRefresh struct {
	ActiveMin        int     `json:"active_min"`
	RecentTrafficMin int     `json:"recent_traffic_min"`
	ReadyMin         int     `json:"ready_min"`
	NearPP           float64 `json:"near_pp"`
	Concurrency      int     `json:"concurrency"`
}

// KongGatewayRules 是发布里网关要执行的规则。没有 Hot 时热度由边车经快照条目执行；没有 BalanceRefresh 时网关不查余额。
type KongGatewayRules struct {
	Hot            *KongHotRules       `json:"hot,omitempty"`
	BalanceRefresh *KongBalanceRefresh `json:"balance_refresh,omitempty"`
}

type kongHotTiersJSON struct {
	Light *[]*float64 `json:"light"`
	Heavy *[]*float64 `json:"heavy"`
	Pause *[]*float64 `json:"pause"`
}

type kongHotSegmentJSON struct {
	From  *string           `json:"from"`
	To    *string           `json:"to"`
	H24   *kongHotTiersJSON `json:"24h"`
	H6    *kongHotTiersJSON `json:"6h"`
	Label *string           `json:"label"`
}

type kongHotWindowsJSON struct {
	H24 *kongHotTiersJSON `json:"24h"`
	H6  *kongHotTiersJSON `json:"6h"`
}

type kongHotRulesJSON struct {
	Mode             *string                          `json:"mode"`
	Default          *kongHotWindowsJSON              `json:"default"`
	Base             *map[string]kongHotWindowsJSON   `json:"base"`
	LightFraction    *float64                         `json:"light_fraction"`
	HeavySessions    *int                             `json:"heavy_sessions"`
	HeavyConcurrency *int                             `json:"heavy_concurrency"`
	Schedule         *map[string][]kongHotSegmentJSON `json:"schedule"`
}

type kongBalanceRefreshJSON struct {
	ActiveMin        *int     `json:"active_min"`
	RecentTrafficMin *int     `json:"recent_traffic_min"`
	ReadyMin         *int     `json:"ready_min"`
	NearPP           *float64 `json:"near_pp"`
	Concurrency      *int     `json:"concurrency"`
}

type kongPlanGatewayRulesJSON struct {
	Hot            *kongHotRulesJSON       `json:"hot"`
	BalanceRefresh *kongBalanceRefreshJSON `json:"balance_refresh"`
}

func kongHotParseLine(raw *[]*float64, field string) (*KongHotLine, error) {
	if raw == nil {
		return nil, nil
	}
	if len(*raw) != 2 {
		return nil, kongPlanInvalid(field, field+" 必须是 [进入, 退出]")
	}
	var v [2]float64
	for i, x := range *raw {
		if x == nil || math.IsNaN(*x) || math.IsInf(*x, 0) {
			return nil, kongPlanInvalid(field, field+" 必须是两个有限数")
		}
		v[i] = *x
	}
	if !(0 <= v[1] && v[1] <= v[0] && v[0] <= kongHotLineMax) {
		return nil, kongPlanInvalid(field, fmt.Sprintf("%s 必须满足 0 ≤ 退出 ≤ 进入 ≤ %d", field, kongHotLineMax))
	}
	l := KongHotLine{kongPlanZero(v[0]), kongPlanZero(v[1])}
	return &l, nil
}

func kongHotParseTiers(raw *kongHotTiersJSON, field string) (*KongHotTiers, error) {
	if raw == nil {
		return nil, nil
	}
	var t KongHotTiers
	var err error
	if t.Light, err = kongHotParseLine(raw.Light, field+".light"); err != nil {
		return nil, err
	}
	if t.Heavy, err = kongHotParseLine(raw.Heavy, field+".heavy"); err != nil {
		return nil, err
	}
	if t.Pause, err = kongHotParseLine(raw.Pause, field+".pause"); err != nil {
		return nil, err
	}
	if t.Light == nil && t.Heavy == nil && t.Pause == nil {
		return nil, kongPlanInvalid(field, field+" 至少写一档")
	}
	return &t, nil
}

func kongHotParseOptTime(raw *string, field string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := kongPlanParseTime(raw, field)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func kongHotParseRules(raw *kongHotRulesJSON) (*KongHotRules, error) {
	if raw == nil {
		return nil, nil
	}
	r := &KongHotRules{Schedule: map[int64][]KongHotSegment{}}
	if raw.Mode == nil || (*raw.Mode != KongHotModeObserve && *raw.Mode != KongHotModeOn) {
		return nil, kongPlanInvalid("gateway_rules.hot.mode", "mode 只能是 observe / on")
	}
	r.Mode = *raw.Mode
	if raw.Default == nil {
		return nil, kongPlanInvalid("gateway_rules.hot.default", "default 必填")
	}
	var err error
	if r.Default.H24, err = kongHotParseTiers(raw.Default.H24, "gateway_rules.hot.default.24h"); err != nil {
		return nil, err
	}
	if r.Default.H6, err = kongHotParseTiers(raw.Default.H6, "gateway_rules.hot.default.6h"); err != nil {
		return nil, err
	}
	if raw.Base != nil {
		r.Base = map[int64]KongHotWindows{}
		for k, w := range *raw.Base {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil || id < 1 || strconv.FormatInt(id, 10) != k {
				return nil, kongPlanInvalid("gateway_rules.hot.base", "base 的键必须是账号编号")
			}
			f := "gateway_rules.hot.base." + k
			var b KongHotWindows
			if b.H24, err = kongHotParseTiers(w.H24, f+".24h"); err != nil {
				return nil, err
			}
			if b.H6, err = kongHotParseTiers(w.H6, f+".6h"); err != nil {
				return nil, err
			}
			if b.H24 == nil && b.H6 == nil {
				return nil, kongPlanInvalid(f, "至少写 24h 或 6h 之一")
			}
			r.Base[id] = b
		}
	}
	if raw.LightFraction == nil || !(*raw.LightFraction > 0 && *raw.LightFraction <= 1) {
		return nil, kongPlanInvalid("gateway_rules.hot.light_fraction", "light_fraction 必须在 (0, 1] 内")
	}
	r.LightFraction = *raw.LightFraction
	if raw.HeavySessions == nil || *raw.HeavySessions < 1 || *raw.HeavySessions > kongPlanMaxLimit {
		return nil, kongPlanInvalid("gateway_rules.hot.heavy_sessions", fmt.Sprintf("heavy_sessions 的取值为 1..%d", kongPlanMaxLimit))
	}
	r.HeavySessions = *raw.HeavySessions
	if raw.HeavyConcurrency == nil || *raw.HeavyConcurrency < 1 || *raw.HeavyConcurrency > kongPlanMaxLimit {
		return nil, kongPlanInvalid("gateway_rules.hot.heavy_concurrency", fmt.Sprintf("heavy_concurrency 的取值为 1..%d", kongPlanMaxLimit))
	}
	r.HeavyConcurrency = *raw.HeavyConcurrency
	if raw.Schedule == nil {
		return nil, kongPlanInvalid("gateway_rules.hot.schedule", "schedule 必填（可以是空对象）")
	}
	for k, segs := range *raw.Schedule {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != k {
			return nil, kongPlanInvalid("gateway_rules.hot.schedule", "schedule 的键必须是账号编号")
		}
		if len(segs) > kongHotMaxSegments {
			return nil, kongPlanInvalid("gateway_rules.hot.schedule."+k, fmt.Sprintf("一个账号最多 %d 段", kongHotMaxSegments))
		}
		out := make([]KongHotSegment, 0, len(segs))
		for i, s := range segs {
			f := fmt.Sprintf("gateway_rules.hot.schedule.%s[%d]", k, i)
			var seg KongHotSegment
			if seg.From, err = kongHotParseOptTime(s.From, f+".from"); err != nil {
				return nil, err
			}
			if seg.To, err = kongHotParseOptTime(s.To, f+".to"); err != nil {
				return nil, err
			}
			if seg.From != nil && seg.To != nil && !seg.From.Before(*seg.To) {
				return nil, kongPlanInvalid(f, "from 必须早于 to")
			}
			if seg.H24, err = kongHotParseTiers(s.H24, f+".24h"); err != nil {
				return nil, err
			}
			if seg.H6, err = kongHotParseTiers(s.H6, f+".6h"); err != nil {
				return nil, err
			}
			if seg.H24 == nil && seg.H6 == nil {
				return nil, kongPlanInvalid(f, "至少写 24h 或 6h 之一")
			}
			if s.Label != nil {
				if seg.Label, err = kongPlanCheckNote(s.Label, f+".label", false); err != nil {
					return nil, err
				}
			}
			out = append(out, seg)
		}
		r.Schedule[id] = out
	}
	return r, nil
}

func kongHotParseRefreshMin(v *int, field string) (int, error) {
	if v == nil || *v < 1 || *v > kongHotMaxRefreshMin {
		return 0, kongPlanInvalid(field, fmt.Sprintf("%s 的取值为 1..%d（分钟）", field, kongHotMaxRefreshMin))
	}
	return *v, nil
}

func kongHotParseBalanceRefresh(raw *kongBalanceRefreshJSON) (*KongBalanceRefresh, error) {
	if raw == nil {
		return nil, nil
	}
	var b KongBalanceRefresh
	var err error
	if b.ActiveMin, err = kongHotParseRefreshMin(raw.ActiveMin, "gateway_rules.balance_refresh.active_min"); err != nil {
		return nil, err
	}
	if b.RecentTrafficMin, err = kongHotParseRefreshMin(raw.RecentTrafficMin, "gateway_rules.balance_refresh.recent_traffic_min"); err != nil {
		return nil, err
	}
	if b.ReadyMin, err = kongHotParseRefreshMin(raw.ReadyMin, "gateway_rules.balance_refresh.ready_min"); err != nil {
		return nil, err
	}
	if raw.NearPP == nil || !(*raw.NearPP >= 0 && *raw.NearPP <= 100) {
		return nil, kongPlanInvalid("gateway_rules.balance_refresh.near_pp", "near_pp 的取值为 0..100")
	}
	b.NearPP = kongPlanZero(*raw.NearPP)
	if raw.Concurrency == nil || *raw.Concurrency < 1 || *raw.Concurrency > kongHotMaxRefreshConc {
		return nil, kongPlanInvalid("gateway_rules.balance_refresh.concurrency", fmt.Sprintf("concurrency 的取值为 1..%d", kongHotMaxRefreshConc))
	}
	b.Concurrency = *raw.Concurrency
	return &b, nil
}

// kongPlanParseGatewayRules 解析发布里的 gateway_rules；没写时返回 nil。
func kongPlanParseGatewayRules(raw *kongPlanGatewayRulesJSON) (*KongGatewayRules, error) {
	if raw == nil {
		return nil, nil
	}
	hot, err := kongHotParseRules(raw.Hot)
	if err != nil {
		return nil, err
	}
	refresh, err := kongHotParseBalanceRefresh(raw.BalanceRefresh)
	if err != nil {
		return nil, err
	}
	if hot == nil && refresh == nil {
		return nil, kongPlanInvalid("gateway_rules", "gateway_rules 至少写 hot 或 balance_refresh 之一")
	}
	return &KongGatewayRules{Hot: hot, BalanceRefresh: refresh}, nil
}

// kongPlanParseExempt 校验人工约束里不做热度限速的账号编号：正整数、不重复，升序返回。
func kongPlanParseExempt(raw *[]int64) ([]int64, error) {
	if raw == nil {
		return nil, nil
	}
	out := append(make([]int64, 0, len(*raw)), (*raw)...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	for i, id := range out {
		if id < 1 {
			return nil, kongPlanInvalid("hot_exempt", "hot_exempt 里的账号编号必须是正整数")
		}
		if i > 0 && out[i-1] == id {
			return nil, kongPlanInvalid("hot_exempt", fmt.Sprintf("hot_exempt 里账号 %d 重复", id))
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
