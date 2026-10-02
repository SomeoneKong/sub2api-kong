package service

import (
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"
)

// 热账号限速（fork 专有，设计见 DESIGN-openai-plan-dispatch.md 第 13 节）：节奏组件每拍按发布里的规则判断每个 OpenAI
// OAuth 账号该在哪一档（轻度、重度、暂停），名单与滞回存进账号状态；生效时在此刻所用条目的平时值上取小、宽限归零，
// 暂停的账号排进计划暂停段。用量是节奏组件的近 6h、近 24h 消耗（本账号百分点，含 credits 补记）。

const (
	kongHotTierLight = 1
	kongHotTierHeavy = 2
	kongHotTierPause = 3

	// kongHotJudgeMaxAge：判断结果（节奏快照）超过这么久没更新，选号时不再按它暂停，限速按最后一次判断继续。
	kongHotJudgeMaxAge = 5 * time.Minute

	kongHotTransitionsKeep = 1000
	kongHotSqueezeKeep     = 48
	kongHotEps             = 1e-9
)

var kongHotTierNames = map[int]string{kongHotTierLight: "light", kongHotTierHeavy: "heavy", kongHotTierPause: "pause"}

// kongHotState 是一个账号的热度名单，存在节奏组件的账号状态里、与消耗记录同一次写入。三份名单各自记进入时刻；
// SafetyPaused 是只按默认规则判断的暂停身份（总暂停名单的子集，只供报告区分来源）；Segment 是上次判断时所在时段的序号
// （从 1 起，0 为不在时段里），SegmentLabel 是它的标签（离开时段的记录用）；JudgedAt 是最近一次真正判断的时刻（计划状态
// 未知、跳过判断的那几拍不更新），暂停是否过期按它算。一份名单建好之后不再修改，每次判断换一份新的。
type kongHotState struct {
	Light        *time.Time `json:"light,omitempty"`
	Heavy        *time.Time `json:"heavy,omitempty"`
	Paused       *time.Time `json:"paused,omitempty"`
	SafetyPaused bool       `json:"safety_paused,omitempty"`
	Segment      int        `json:"segment,omitempty"`
	SegmentLabel string     `json:"segment_label,omitempty"`
	JudgedAt     time.Time  `json:"judged_at,omitempty"`
}

func (h *kongHotState) member(tier int) *time.Time {
	switch tier {
	case kongHotTierLight:
		return h.Light
	case kongHotTierHeavy:
		return h.Heavy
	}
	return h.Paused
}

func (h *kongHotState) set(tier int, t *time.Time) {
	switch tier {
	case kongHotTierLight:
		h.Light = t
	case kongHotTierHeavy:
		h.Heavy = t
	default:
		h.Paused = t
	}
}

// level 是限速档：2 重度、1 轻度、0 不限速。
func (h *kongHotState) level() int {
	switch {
	case h == nil:
		return 0
	case h.Heavy != nil:
		return 2
	case h.Light != nil:
		return 1
	}
	return 0
}

// kongHotRule 是此刻生效的一条规则。Source 为 0 是默认规则，否则是时段在这个账号列表里的序号。
type kongHotRule struct {
	Tier   int
	Window string
	Line   KongHotLine
	Source int
}

// forced 是 [0, 0]：不看读数，在所在时段里一直成立。
func (r kongHotRule) forced() bool { return r.Line.enter() <= 0 }

func kongHotTiersRules(t *KongHotTiers, window string, source int, enabled map[int]bool, out []kongHotRule) []kongHotRule {
	if t == nil {
		return out
	}
	for tier, line := range map[int]*KongHotLine{kongHotTierLight: t.Light, kongHotTierHeavy: t.Heavy, kongHotTierPause: t.Pause} {
		if line != nil && enabled[tier] {
			out = append(out, kongHotRule{Tier: tier, Window: window, Line: *line, Source: source})
		}
	}
	return out
}

// kongHotRuleSet 给出账号此刻的规则：默认规则加此刻所在时段的规则。默认规则里没有的档是运维关掉的，时段也加不上。
func kongHotRuleSet(r *KongHotRules, seg *KongHotSegment, segIdx int) []kongHotRule {
	if r == nil {
		return nil
	}
	enabled := map[int]bool{}
	for _, t := range []*KongHotTiers{r.Default.H24, r.Default.H6} {
		if t == nil {
			continue
		}
		enabled[kongHotTierLight] = enabled[kongHotTierLight] || t.Light != nil
		enabled[kongHotTierHeavy] = enabled[kongHotTierHeavy] || t.Heavy != nil
		enabled[kongHotTierPause] = enabled[kongHotTierPause] || t.Pause != nil
	}
	var out []kongHotRule
	out = kongHotTiersRules(r.Default.H24, "24h", 0, enabled, out)
	out = kongHotTiersRules(r.Default.H6, "6h", 0, enabled, out)
	if seg != nil {
		out = kongHotTiersRules(seg.H24, "24h", segIdx, enabled, out)
		out = kongHotTiersRules(seg.H6, "6h", segIdx, enabled, out)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Window == "24h" && b.Window != "24h" // 24h 在 6h 之前
	})
	return out
}

func kongHotReached(x, line float64) bool { return x >= line-kongHotEps }

// kongHotTransition 是一次进出：进入或退出一档、进入或离开时段。Source 为触发的规则来源（0 默认、n 时段序号）。
type kongHotTransition struct {
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
	AccountID int64     `json:"account_id"`
	Kind      string    `json:"kind"` // enter / exit / segment_enter / segment_exit
	Tier      string    `json:"tier,omitempty"`
	Window    string    `json:"window,omitempty"`
	Source    int       `json:"source,omitempty"`
	Value     float64   `json:"value"`
	Line      float64   `json:"line,omitempty"`
	Pct24h    float64   `json:"pct_24h"`
	Pct6h     float64   `json:"pct_6h"`
	Label     string    `json:"label,omitempty"`
}

// kongHotJudge 按此刻的规则推进一个账号的名单（设计文档第 13.3 节）：某一档任一条规则到了进入线就进；名单上的账号，这一档
// 所有规则都在各自退出线以下才退出。[0, 0] 不看读数，在所在时段里一直成立。rules 为空（没有规则）时清空名单，各档与所在
// 时段照常记退出。
func kongHotJudge(prev *kongHotState, rules []kongHotRule, inc6, inc24 float64, segIdx int, label string, now time.Time) (*kongHotState, []kongHotTransition) {
	next := &kongHotState{}
	if prev != nil {
		*next = *prev
	}
	next.JudgedAt = now
	var out []kongHotTransition
	note := func(kind string, tier int, r *kongHotRule, value, line float64, label string) {
		t := kongHotTransition{At: now, Kind: kind, Tier: kongHotTierNames[tier], Value: value, Line: line, Pct24h: inc24, Pct6h: inc6, Label: label}
		if r != nil {
			t.Window, t.Source = r.Window, r.Source
		}
		out = append(out, t)
	}
	if len(rules) == 0 {
		segIdx, label = 0, ""
	}
	if next.Segment != segIdx {
		if next.Segment != 0 {
			note("segment_exit", 0, &kongHotRule{Source: next.Segment}, inc24, 0, next.SegmentLabel)
		}
		if segIdx != 0 {
			note("segment_enter", 0, &kongHotRule{Source: segIdx}, inc24, 0, label)
		}
		next.Segment = segIdx
	}
	next.SegmentLabel = label
	if len(rules) == 0 {
		for _, tier := range []int{kongHotTierLight, kongHotTierHeavy, kongHotTierPause} {
			if next.member(tier) != nil {
				note("exit", tier, nil, inc24, 0, label)
			}
		}
		return nil, out
	}
	value := func(r kongHotRule) float64 {
		if r.Window == "6h" {
			return inc6
		}
		return inc24
	}
	step := func(tier int, safetyOnly bool) (in bool, trigger *kongHotRule) {
		var member bool
		if safetyOnly {
			member = next.SafetyPaused
		} else {
			member = next.member(tier) != nil
		}
		var tierRules []kongHotRule
		for _, r := range rules {
			if r.Tier == tier && (!safetyOnly || r.Source == 0) {
				tierRules = append(tierRules, r)
			}
		}
		if !member {
			for i := range tierRules {
				r := tierRules[i]
				if r.forced() || kongHotReached(value(r), r.Line.enter()) {
					return true, &r
				}
			}
			return false, nil
		}
		for i := range tierRules {
			r := tierRules[i]
			if r.forced() || kongHotReached(value(r), r.Line.exit()) {
				return true, nil
			}
		}
		return false, nil
	}
	for _, tier := range []int{kongHotTierLight, kongHotTierHeavy, kongHotTierPause} {
		was := next.member(tier) != nil
		in, trigger := step(tier, false)
		switch {
		case in && !was:
			at := now
			next.set(tier, &at)
			note("enter", tier, trigger, value(*trigger), trigger.Line.enter(), label)
		case !in && was:
			next.set(tier, nil)
			note("exit", tier, nil, inc24, 0, label)
		}
	}
	safety, _ := step(kongHotTierPause, true)
	next.SafetyPaused = safety && next.Paused != nil
	return next, out
}

// kongHotCap 是一个上限在热度下的值：base ≤ 0 表示不限（会话），这时两档都取重度的值；重度与平时取小；轻度按比例向上取整、
// 不低于重度的值，再与平时取小。
func kongHotCap(base, heavy int, frac float64, level int) int {
	if level <= 0 {
		return base
	}
	if base <= 0 {
		return heavy
	}
	if level >= 2 {
		return min(base, heavy)
	}
	return min(base, max(heavy, int(math.Ceil(float64(base)*frac-kongHotEps))))
}

// kongHotEffect 是一个账号此刻生效的热度：限速档与是否暂停。不生效时为零值。
type kongHotEffect struct {
	Level  int
	Paused bool
	Rules  *KongHotRules
}

// kongHotNotInEffect 返回规则此刻对这个账号不生效的原因；生效时返回空串。
func kongHotNotInEffect(st *kongPlanState, id int64, now time.Time) string {
	if st == nil || st.dispatch == nil || st.dispatch.GatewayRules == nil || st.dispatch.GatewayRules.Hot == nil {
		return "no_rules"
	}
	if st.inUse(now) == KongPlanInUseLegacy {
		return "legacy"
	}
	if st.dispatch.GatewayRules.Hot.Mode != KongHotModeOn {
		return "observe"
	}
	if st.control.HotOff {
		return "hot_off"
	}
	i := sort.Search(len(st.control.HotExempt), func(i int) bool { return st.control.HotExempt[i] >= id })
	if i < len(st.control.HotExempt) && st.control.HotExempt[i] == id {
		return "exempt"
	}
	return ""
}

// kongHotEffectNow 是选号与上限读取用的热度：只看内存里的计划状态与节奏快照，不访问 Redis 与数据库。
func kongHotEffectNow(id int64) kongHotEffect {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return kongHotEffect{}
	}
	return rt.hotEffect(rt.store.current(), id, rt.now())
}

// hotEffect 按给定的计划状态求账号此刻生效的热度（同一次计算的条目与热度要取自同一份状态）。
func (rt *kongPlanRuntime) hotEffect(st *kongPlanState, id int64, now time.Time) kongHotEffect {
	if rt == nil || rt.pace == nil {
		return kongHotEffect{}
	}
	return kongHotEffectOf(st, rt.pace.hotState(id), id, now)
}

// kongHotEffectOf 由计划状态与账号的节奏状态（hotState 取出的同一份）求此刻生效的热度：判断结果过期时不暂停，限速按最后
// 一次判断继续。
func kongHotEffectOf(st *kongPlanState, ps *kongPaceAccountState, id int64, now time.Time) kongHotEffect {
	if ps == nil || ps.Hot == nil || kongHotNotInEffect(st, id, now) != "" {
		return kongHotEffect{}
	}
	hot := ps.Hot
	e := kongHotEffect{Level: hot.level(), Rules: st.dispatch.GatewayRules.Hot}
	e.Paused = hot.Paused != nil && !hot.JudgedAt.IsZero() && now.Sub(hot.JudgedAt) <= kongHotJudgeMaxAge
	return e
}

// hotState 是求热度用的一个账号的节奏状态：取节奏快照；启动后还没成功推进过（没有快照）时取从 Redis 读回的那份。
// 只读，没有这个账号时为 nil。
func (p *KongOpenAIAccountPace) hotState(id int64) *kongPaceAccountState {
	if p == nil {
		return nil
	}
	if snap := p.snapshot.Load(); snap != nil {
		if f, ok := snap.accounts[id]; ok {
			return &f.State
		}
		return nil
	}
	if restored := p.restored.Load(); restored != nil {
		return (*restored)[id]
	}
	return nil
}

// kongHotRestoredFrom 复制一份读回的账号状态（推进时会原地修改 p.states，这份要保持读回时的样子）。
func kongHotRestoredFrom(states map[int64]*kongPaceAccountState) *map[int64]*kongPaceAccountState {
	out := make(map[int64]*kongPaceAccountState, len(states))
	for id, st := range states {
		if st != nil {
			c := kongPaceStateCopy(st)
			out[id] = &c
		}
	}
	return &out
}

func (e kongHotEffect) concurrency(base int) int {
	if e.Rules == nil {
		return base
	}
	return kongHotCap(base, e.Rules.HeavyConcurrency, e.Rules.LightFraction, e.Level)
}

func (e kongHotEffect) sessions(base int) int {
	if e.Rules == nil {
		return base
	}
	return kongHotCap(base, e.Rules.HeavySessions, e.Rules.LightFraction, e.Level)
}

// kongHotOverlayEntry 把热度 h 叠加到定层用的条目上：平时的值来自条目的覆盖或账号自身的设置；限速时上限取小、宽限归零，
// 暂停与条目里别处来的 paused 取"或"。h 与条目要取自同一份计划状态。
func kongHotOverlayEntry(e *KongPlanEntry, account *Account, h kongHotEffect) {
	if e == nil || account == nil {
		return
	}
	if h.Level > 0 {
		baseS := account.GetMaxSessions()
		if e.MaxSessions != nil {
			baseS = *e.MaxSessions
		}
		baseC := account.Concurrency
		if e.MaxConcurrency != nil {
			baseC = *e.MaxConcurrency
		}
		s, c, g := h.sessions(baseS), h.concurrency(baseC), 0
		e.MaxSessions, e.MaxConcurrency, e.Grace = &s, &c, &g
	}
	if h.Paused {
		e.Paused = true
	}
}

// kongHotOwnConcurrency 返回节奏快照里账号自身的并发设置（0 为不限）；快照里没有这个账号时 found 为假。
func kongHotOwnConcurrency(id int64) (own int, found bool) {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.pace == nil {
		return 0, false
	}
	snap := rt.pace.snapshot.Load()
	if snap == nil {
		return 0, false
	}
	f, ok := snap.accounts[id]
	return f.Concurrency, ok
}

// KongWaitConcurrency 是抢槽用的并发上限：计划输入生效时读此刻的生效值（含热度），选号或入队时记下的值可能已经过时；
// 未生效、或节奏快照里没有这个账号时用记下的值。
func KongWaitConcurrency(id int64, queued int) int {
	if !kongPlanInEffect() {
		return queued
	}
	own, ok := kongHotOwnConcurrency(id)
	if !ok {
		return queued
	}
	return kongEffectiveConcurrencyByID(id, own)
}

// judgeHot 按当前发布里的规则推进每个账号的热度名单（只在刷新协程里调用），返回这一拍的转换记录；记录在状态写入成功后
// 才进转换记录。发布确实没有规则时清空名单（记退出）；计划状态还没载入（不知道有没有规则）时名单原样保留、这一拍不判断，
// 判断时刻也不更新。
func (p *KongOpenAIAccountPace) judgeHot(rows []KongPaceAccountRow, tracked map[int64]bool, now time.Time) []kongHotTransition {
	var rules *KongHotRules
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return nil
	}
	st := rt.store.current()
	if st == nil {
		return nil
	}
	if st.dispatch != nil && st.dispatch.GatewayRules != nil {
		rules = st.dispatch.GatewayRules.Hot
	}
	var out []kongHotTransition
	for _, row := range rows {
		st := p.states[row.ID]
		if st == nil {
			continue
		}
		inc6, inc24 := kongPaceIncrease(st, now, 6*time.Hour), kongPaceIncrease(st, now, 24*time.Hour)
		var (
			next *kongHotState
			ts   []kongHotTransition
		)
		if rules == nil || !tracked[row.ID] {
			next, ts = kongHotJudge(st.Hot, nil, inc6, inc24, 0, "", now)
		} else {
			seg, idx := rules.segmentAt(row.ID, now)
			label := ""
			if seg != nil {
				label = seg.Label
			}
			next, ts = kongHotJudge(st.Hot, kongHotRuleSet(rules, seg, idx), inc6, inc24, idx, label, now)
		}
		st.Hot = next
		for i := range ts {
			ts[i].AccountID = row.ID
		}
		out = append(out, ts...)
	}
	return out
}

// kongHotLog 是转换记录与限速挤压计数，只在内存里（网关重启后从空开始，边车按 Boot 发现重启）。
type kongHotLogT struct {
	mu          sync.Mutex
	boot        time.Time
	seq         int64
	transitions []kongHotTransition
	squeeze     []KongHotSqueezeBucket
}

// KongHotSqueezeBucket 是一小时里热度生效的账号上的排队结果：等到槽、队列满被拒、等到超时、WS 后续轮次抢不到槽被关。
type KongHotSqueezeBucket struct {
	Hour     time.Time `json:"hour"`
	Waited   int64     `json:"waited"`
	Rejected int64     `json:"rejected"`
	Timeout  int64     `json:"timeout"`
	WSBusy   int64     `json:"ws_busy"`
}

var kongHotLog = &kongHotLogT{boot: time.Now().UTC()}

// append 在这一拍的状态写入成功之后记下转换（同时写日志）：写入失败的那一拍不记，重新载入后再判断出的才记。
func (l *kongHotLogT) append(ts []kongHotTransition) {
	if len(ts) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range ts {
		l.seq++
		t.Seq = l.seq
		l.transitions = append(l.transitions, t)
		slog.Info("kong plan: 热度转换", "seq", t.Seq, "account_id", t.AccountID, "kind", t.Kind, "tier", t.Tier, "window", t.Window,
			"source", t.Source, "value", t.Value, "line", t.Line, "pct_24h", t.Pct24h, "pct_6h", t.Pct6h, "label", t.Label)
	}
	if over := len(l.transitions) - kongHotTransitionsKeep; over > 0 {
		l.transitions = append([]kongHotTransition(nil), l.transitions[over:]...)
	}
}

// kongHotSqueezeSince 是近 kongHotSqueezeKeep 小时的小时桶（调用方持锁）：按桶的时刻裁，没有新事件时旧桶照样过期。
func (l *kongHotLogT) kongHotSqueezeSince(now time.Time) []KongHotSqueezeBucket {
	cutoff := now.UTC().Truncate(time.Hour).Add(-(kongHotSqueezeKeep - 1) * time.Hour)
	i := sort.Search(len(l.squeeze), func(i int) bool { return !l.squeeze[i].Hour.Before(cutoff) })
	return l.squeeze[i:]
}

func (l *kongHotLogT) note(outcome string, now time.Time) {
	hour := now.UTC().Truncate(time.Hour)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.squeeze = append([]KongHotSqueezeBucket(nil), l.kongHotSqueezeSince(now)...)
	n := len(l.squeeze)
	if n == 0 || !l.squeeze[n-1].Hour.Equal(hour) {
		l.squeeze = append(l.squeeze, KongHotSqueezeBucket{Hour: hour})
		n = len(l.squeeze)
	}
	b := &l.squeeze[n-1]
	switch outcome {
	case KongHotWaitAcquired:
		b.Waited++
	case KongHotWaitRejected:
		b.Rejected++
	case KongHotWaitTimeout:
		b.Timeout++
	case KongHotWaitWSBusy:
		b.WSBusy++
	}
}

// 排队结果，KongHotNoteWait 的 outcome。
const (
	KongHotWaitAcquired = "acquired"
	KongHotWaitRejected = "rejected"
	KongHotWaitTimeout  = "timeout"
	KongHotWaitWSBusy   = "ws_busy"
)

// KongHotNoteWait 记一次排队结果（handler 在等待结束处调用）：只记热度此刻生效（限速或暂停）的账号，记了返回 true
// （HTTP 请求据此保证同一请求只记一次）。
func KongHotNoteWait(accountID int64, outcome string) bool {
	if outcome == "" {
		return false
	}
	h := kongHotEffectNow(accountID)
	if h.Level == 0 && !h.Paused {
		return false
	}
	kongHotLog.note(outcome, time.Now())
	return true
}

// KongHotLogView 是 GET /api/v1/admin/kong-plan/hot 的响应：Boot 变了说明网关重启过、序号从头算。
type KongHotLogView struct {
	Boot        time.Time                `json:"boot"`
	LastSeq     int64                    `json:"last_seq"`
	Transitions []kongHotTransition      `json:"transitions"`
	Squeeze     []KongHotSqueezeBucket   `json:"squeeze"`
	Refresh     []KongBalanceRefreshView `json:"balance_refresh"`
}

// KongHotLog 返回序号大于 since 的转换记录、近 48 小时的限速挤压与余额刷新状态。
func KongHotLog(since int64) *KongHotLogView {
	l := kongHotLog
	l.mu.Lock()
	v := &KongHotLogView{Boot: l.boot, LastSeq: l.seq, Transitions: []kongHotTransition{}, Squeeze: append([]KongHotSqueezeBucket{}, l.kongHotSqueezeSince(time.Now())...)}
	for _, t := range l.transitions {
		if t.Seq > since {
			v.Transitions = append(v.Transitions, t)
		}
	}
	l.mu.Unlock()
	v.Refresh = kongBalanceRefreshStatus()
	return v
}

// KongHotSegmentView 是读回里此刻所在的时段：Index 是它在这个账号列表里的序号（从 1 起）。
type KongHotSegmentView struct {
	Index int        `json:"index"`
	From  *time.Time `json:"from"`
	To    *time.Time `json:"to"`
	Label string     `json:"label,omitempty"`
}

// KongHotRuleView 是读回里此刻生效的一条规则。Source 为 0 是默认规则，否则是时段序号。
type KongHotRuleView struct {
	Tier   string  `json:"tier"`
	Window string  `json:"window"`
	Enter  float64 `json:"enter"`
	Exit   float64 `json:"exit"`
	Source int     `json:"source"`
}

// KongHotAccountView 是 GET /accounts 里一个账号的热度状态。Level / Paused / SafetyPaused 与各名单的进入时刻是判断出的
// 名单，JudgedAt 是那次判断的时刻；EffectiveLevel / EffectivePaused 是选号此刻实际用的（不生效时为零，判断过期时只有
// EffectivePaused 为假）；InEffect 为假时 NotInEffect 是原因（no_rules / legacy / observe / hot_off / exempt）。Hourly
// 第 i 项是 (now − (i+1)h, now − ih] 的消耗（本账号百分点，含 credits 补记）。
type KongHotAccountView struct {
	Level           string              `json:"level"`
	Paused          bool                `json:"paused"`
	SafetyPaused    bool                `json:"safety_paused"`
	LightSince      *time.Time          `json:"light_since"`
	HeavySince      *time.Time          `json:"heavy_since"`
	PausedSince     *time.Time          `json:"paused_since"`
	EffectiveLevel  string              `json:"effective_level"`
	EffectivePaused bool                `json:"effective_paused"`
	InEffect        bool                `json:"in_effect"`
	NotInEffect     string              `json:"not_in_effect,omitempty"`
	JudgedAt        *time.Time          `json:"judged_at"`
	Pct24h          float64             `json:"pct_24h"`
	Pct6h           float64             `json:"pct_6h"`
	Segment         *KongHotSegmentView `json:"segment"`
	Rules           []KongHotRuleView   `json:"rules"`
	Hourly          []float64           `json:"hourly"`
}

var kongHotLevelNames = map[int]string{0: "none", 1: "light", 2: "heavy"}

func kongHotTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// hotView 给出一个账号的热度读回。st、ps、e 是同一行读回共用的计划状态、节奏状态（hotState）与由它们求出的生效热度。
// ps 为 nil（网关还没跟踪这个账号）时为 nil。
func (rt *kongPlanRuntime) hotView(st *kongPlanState, ps *kongPaceAccountState, e kongHotEffect, account *Account, now time.Time) *KongHotAccountView {
	if rt == nil || ps == nil || account == nil {
		return nil
	}
	var rules *KongHotRules
	if st != nil && st.dispatch != nil && st.dispatch.GatewayRules != nil {
		rules = st.dispatch.GatewayRules.Hot
	}
	v := &KongHotAccountView{
		Pct24h: kongPaceIncrease(ps, now, 24*time.Hour),
		Pct6h:  kongPaceIncrease(ps, now, 6*time.Hour),
		Rules:  []KongHotRuleView{},
		Hourly: make([]float64, 24),
	}
	for _, r := range ps.Rises {
		if r.At.After(now) {
			continue
		}
		if i := int(now.Sub(r.At) / time.Hour); i >= 0 && i < 24 && now.Sub(r.At) < 24*time.Hour {
			v.Hourly[i] += r.Points
		}
	}
	if h := ps.Hot; h != nil {
		v.Level, v.Paused, v.SafetyPaused = kongHotLevelNames[h.level()], h.Paused != nil, h.SafetyPaused
		v.LightSince, v.HeavySince, v.PausedSince = h.Light, h.Heavy, h.Paused
		v.JudgedAt = kongHotTimePtr(h.JudgedAt)
	} else {
		v.Level = kongHotLevelNames[0]
	}
	v.NotInEffect = kongHotNotInEffect(st, account.ID, now)
	v.InEffect = v.NotInEffect == ""
	v.EffectiveLevel, v.EffectivePaused = kongHotLevelNames[e.Level], e.Paused
	if seg, idx := rules.segmentAt(account.ID, now); seg != nil {
		v.Segment = &KongHotSegmentView{Index: idx, From: seg.From, To: seg.To, Label: seg.Label}
		for _, r := range kongHotRuleSet(rules, seg, idx) {
			v.Rules = append(v.Rules, KongHotRuleView{Tier: kongHotTierNames[r.Tier], Window: r.Window, Enter: r.Line.enter(), Exit: r.Line.exit(), Source: r.Source})
		}
	} else {
		for _, r := range kongHotRuleSet(rules, nil, 0) {
			v.Rules = append(v.Rules, KongHotRuleView{Tier: kongHotTierNames[r.Tier], Window: r.Window, Enter: r.Line.enter(), Exit: r.Line.exit(), Source: r.Source})
		}
	}
	return v
}
