package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// 两轮排序（fork 专有，设计见 DESIGN-account-selection.md 第 4.2 节第 3、4 步）：计划输入（快照或兜底）生效时，
// 旧版选号的四个分支都按这里给出的尝试序列逐个试；输入为现状调度时这里的挂点全部是空操作，代码路径与上游相同。
//
// 序列由桶组成，一个桶是四个键都相同的一组账号：轮次与段、第一层的档位组、组内的健康状态、健康子集里的临期。
// 桶的先后不可跨越。桶内沿用旧版选号这一轮的顺序（原优先级、负载、最近使用，节奏重排、低倍率优先、compact 分层都只在
// 桶内起作用）；credits 层的桶严格按（级别，作废时刻，账号编号）。会话数已经由段分开，组内的健康只看其余几项：
// 未到软线 full、负载已知且并发未满、不视同到线。

type kongPlanSeg int

const (
	kongPlanSegRoom            kongPlanSeg = iota + 1 // 余量轮：第一层，会话数低于上限
	kongPlanSegGrace                                  // 余量轮：第一层，会话数到上限、低于上限 + 宽限
	kongPlanSegCreditsRoom                            // 余量轮：credits 层，会话数低于上限
	kongPlanSegOverflow                               // 溢出轮：第一层
	kongPlanSegCreditsOverflow                        // 溢出轮：credits 层
	kongPlanSegHeld                                   // 计划暂停：快照条目 paused 的第一层与 credits 层账号，别的都接不住才用
	kongPlanSegPaused                                 // 定层为暂停却仍在候选里：复核时由现状的暂停判定拦下
	kongPlanSegRelay                                  // 非 OAuth 中转
)

func (s kongPlanSeg) String() string {
	switch s {
	case kongPlanSegRoom:
		return "room"
	case kongPlanSegGrace:
		return "grace"
	case kongPlanSegCreditsRoom:
		return "credits_room"
	case kongPlanSegOverflow:
		return "overflow"
	case kongPlanSegCreditsOverflow:
		return "credits_overflow"
	case kongPlanSegHeld:
		return "held"
	case kongPlanSegPaused:
		return "paused"
	case kongPlanSegRelay:
		return "relay"
	}
	return ""
}

func (s kongPlanSeg) credits() bool {
	return s == kongPlanSegCreditsRoom || s == kongPlanSegCreditsOverflow
}

func (s kongPlanSeg) subscription() bool {
	return s == kongPlanSegRoom || s == kongPlanSegGrace || s == kongPlanSegOverflow
}

type kongPlanGroup int

const (
	kongPlanGroupNone kongPlanGroup = iota
	kongPlanGroupAsap
	kongPlanGroupNormal
	kongPlanGroupInactive
	kongPlanGroupStandby
)

func (g kongPlanGroup) String() string {
	switch g {
	case kongPlanGroupAsap:
		return "asap"
	case kongPlanGroupNormal:
		return "normal"
	case kongPlanGroupInactive:
		return "inactive"
	case kongPlanGroupStandby:
		return "standby"
	}
	return ""
}

// kongPlanFacts 是排序用到的一个候选账号的事实。
type kongPlanFacts struct {
	ID   int64
	Tier kongPlanTierResult // Layer 为 0 表示非 OpenAI OAuth（中转）
	// Sessions 是本次选号开始时查得的活跃会话数；SessionsKnown 为假时（没有计数）按有余量处理，确认登记照旧失败开放。
	Sessions      int
	SessionsKnown bool
	// SessionLimit 是生效会话上限，0 表示不限。Grace 是第一层的会话宽限。
	SessionLimit int
	Grace        int
	// LoadKnown 为假时（负载读取失败、第 3 层）按忙处理。
	LoadKnown       bool
	ConcurrencyFull bool
	// Inc6、Inc24 是近 6h、24h 的消耗（本账号百分点，含 credits 补记）；PaceFull 是已到软线 full。
	Inc6, Inc24 float64
	PaceFull    bool
	// Priority 是账号的原优先级（小的先），同一桶里两个降段重试位置之间按它排。
	Priority int
}

func (f *kongPlanFacts) limited() bool { return f.SessionLimit > 0 && f.SessionsKnown }

// kongPlanBucketKey 是桶的四个键。credits、暂停与中转的桶只有段。
type kongPlanBucketKey struct {
	Seg      kongPlanSeg
	Group    kongPlanGroup
	Healthy  bool
	Imminent bool
}

func kongPlanKeyLess(a, b kongPlanBucketKey) bool {
	if a.Seg != b.Seg {
		return a.Seg < b.Seg
	}
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	if a.Healthy != b.Healthy {
		return a.Healthy
	}
	if a.Imminent != b.Imminent {
		return a.Imminent
	}
	return false
}

// kongPlanSlot 是尝试序列里的一个位置。
type kongPlanSlot struct {
	ID  int64
	Key kongPlanBucketKey
	// Limit 是在这个位置确认会话登记时允许的上限：余量段为生效上限，宽限段为上限 + 宽限，溢出轮为 kongSessionUnlimited
	// （放行登记）；0 表示账号不受会话上限约束、不登记。
	Limit int
	// Ghost 是降段重试的位置：账号在更前的段确认登记失败、降到这一段时才尝试，否则跳过。
	Ghost bool
}

// kongPlanNaturalSeg 按本次选号开始时的会话数给出账号所在的段。计划暂停的第一层与 credits 层账号不论会话数都在计划暂停段：
// 排在所有正常候选之后，不降段、不受会话上限约束；第一层的复核时改判为 credits 层的，改判位置也在这一段。
func kongPlanNaturalSeg(f *kongPlanFacts) kongPlanSeg {
	if f.Tier.Entry.Paused && (f.Tier.Layer == kongPlanLayerSubscription || f.Tier.Layer == kongPlanLayerCredits) {
		return kongPlanSegHeld
	}
	switch f.Tier.Layer {
	case kongPlanLayerSubscription:
		switch {
		case !f.limited() || f.Sessions < f.SessionLimit:
			return kongPlanSegRoom
		case f.Sessions < f.SessionLimit+f.Grace:
			return kongPlanSegGrace
		}
		return kongPlanSegOverflow
	case kongPlanLayerCredits:
		if !f.limited() || f.Sessions < f.SessionLimit {
			return kongPlanSegCreditsRoom
		}
		return kongPlanSegCreditsOverflow
	case kongPlanLayerPaused:
		return kongPlanSegPaused
	}
	return kongPlanSegRelay
}

// kongPlanSegWith 是账号此刻所在的段：按会话数的段与本次选号里已降到的段中靠后的那个（同一层之内）。
func kongPlanSegWith(f *kongPlanFacts, minSeg map[int64]kongPlanSeg) kongPlanSeg {
	seg := kongPlanNaturalSeg(f)
	if m, ok := minSeg[f.ID]; ok && m > seg && m.credits() == seg.credits() && m.subscription() == seg.subscription() {
		seg = m
	}
	return seg
}

// kongPlanDemote 给出确认登记失败后向后降到的段：余量段 → 宽限段（宽限为 0 时跳过）→ 溢出轮。
func kongPlanDemote(f *kongPlanFacts, seg kongPlanSeg) (kongPlanSeg, bool) {
	switch seg {
	case kongPlanSegRoom:
		if f.Grace > 0 {
			return kongPlanSegGrace, true
		}
		return kongPlanSegOverflow, true
	case kongPlanSegGrace:
		return kongPlanSegOverflow, true
	case kongPlanSegCreditsRoom:
		return kongPlanSegCreditsOverflow, true
	}
	return 0, false
}

// kongPlanAdmitted 是尽快档的准入：会话有余量（调用方只在余量段问）、负载已知且并发未满、近 6h 与 24h 消耗都低于
// 条目的准入线，且不视同到线。
func kongPlanAdmitted(f *kongPlanFacts) bool {
	a := f.Tier.Entry.AdmitBelow
	return a != nil && f.LoadKnown && !f.ConcurrencyFull && !f.Tier.AtLine && f.Inc6 < a.H6 && f.Inc24 < a.H24
}

func kongPlanGroupOf(f *kongPlanFacts, seg kongPlanSeg) kongPlanGroup {
	e := f.Tier.Entry
	switch {
	case e.Tier == KongPlanTierStandby:
		return kongPlanGroupStandby
	case !e.Active:
		return kongPlanGroupInactive
	case e.Tier == KongPlanTierAsap && seg == kongPlanSegRoom && kongPlanAdmitted(f):
		return kongPlanGroupAsap
	}
	return kongPlanGroupNormal
}

// kongPlanSlotFor 给出账号在某一段的位置。
func kongPlanSlotFor(f *kongPlanFacts, seg kongPlanSeg) kongPlanSlot {
	s := kongPlanSlot{ID: f.ID, Key: kongPlanBucketKey{Seg: seg}}
	if seg.subscription() {
		healthy := !f.PaceFull && f.LoadKnown && !f.ConcurrencyFull && !f.Tier.AtLine
		s.Key.Group = kongPlanGroupOf(f, seg)
		s.Key.Healthy = healthy
		s.Key.Imminent = healthy && f.Tier.Entry.Imminent
	}
	if !f.limited() {
		return s
	}
	switch seg {
	case kongPlanSegRoom, kongPlanSegCreditsRoom:
		s.Limit = f.SessionLimit
	case kongPlanSegGrace:
		s.Limit = f.SessionLimit + f.Grace
	default:
		s.Limit = kongSessionUnlimited
	}
	return s
}

// kongPlanOrder 给出一轮的尝试序列。facts 按旧版选号这一轮的顺序给出，桶内沿用它；minSeg 是本次选号里确认登记
// 失败后降到的段，账号不再回到更前的段。每个受会话上限约束的账号在它之后的各段各有一个降段重试的位置，排在目标桶
// 本来的成员之后；重试位置之间按原优先级排（它们来自不同的桶，输入里的先后是来源桶的先后）。第一层的账号若条目给了
// credits，另在 credits 段预留改判的位置：确认时复核读数已到阈值、改判为 credits 层（confirm），就在这些位置上试，
// 仍在中转之前。
func kongPlanOrder(facts []kongPlanFacts, minSeg map[int64]kongPlanSeg) []kongPlanSlot {
	slots := make([]kongPlanSlot, 0, len(facts)*2)
	credits := make(map[int64]*KongPlanCredits, len(facts))
	priority := make(map[int64]int, len(facts))
	for i := range facts {
		f := &facts[i]
		priority[f.ID] = f.Priority
		seg := kongPlanSegWith(f, minSeg)
		if seg.credits() {
			credits[f.ID] = f.Tier.Entry.Credits
		}
		slots = append(slots, kongPlanSlotFor(f, seg))
		if !f.limited() {
			continue
		}
		for next, ok := kongPlanDemote(f, seg); ok; next, ok = kongPlanDemote(f, next) {
			g := kongPlanSlotFor(f, next)
			g.Ghost = true
			slots = append(slots, g)
		}
	}
	for i := range facts {
		f := facts[i]
		if f.Tier.Layer != kongPlanLayerSubscription || f.Tier.Entry.Credits == nil {
			continue
		}
		credits[f.ID] = f.Tier.Entry.Credits
		f.Tier.Layer = kongPlanLayerCredits
		for seg, ok := kongPlanNaturalSeg(&f), true; ok; seg, ok = kongPlanDemote(&f, seg) {
			g := kongPlanSlotFor(&f, seg)
			g.Ghost = true
			slots = append(slots, g)
			if !f.limited() {
				break
			}
		}
	}
	sort.SliceStable(slots, func(i, j int) bool {
		a, b := slots[i], slots[j]
		if a.Key != b.Key {
			return kongPlanKeyLess(a.Key, b.Key)
		}
		if a.Ghost != b.Ghost {
			return !a.Ghost // 降段来的账号已经在更前的段试过一次，排在这个桶本来的成员之后
		}
		if !a.Key.Seg.credits() {
			return a.Ghost && priority[a.ID] < priority[b.ID]
		}
		ca, cb := credits[a.ID], credits[b.ID]
		if ca == nil || cb == nil {
			return ca != nil && cb == nil
		}
		if ca.Level != cb.Level {
			return ca.Level < cb.Level
		}
		if !ca.ExpiresAt.Equal(cb.ExpiresAt) {
			return ca.ExpiresAt.Before(cb.ExpiresAt)
		}
		return a.ID < b.ID
	})
	return slots
}

// kongPlanEffectiveMaxSessions 是生效会话上限：条目有覆盖值就用它，否则用账号自身的设置；0 表示不限。
func kongPlanEffectiveMaxSessions(entry KongPlanEntry, account *Account) int {
	if entry.MaxSessions != nil {
		return *entry.MaxSessions
	}
	return account.GetMaxSessions()
}

// kongPlanGrace 是账号的会话宽限：人工约束里按账号的值，没有就用缺省值。
func kongPlanGrace(c KongPlanControl, id int64) int {
	if v, ok := c.OverflowMargin[strconv.FormatInt(id, 10)]; ok {
		return v
	}
	return c.OverflowMargin["default"]
}

// kongPlanSelection 是一次进入第 2 层的计划排序：事实在进入时取一次，各轮（刷新负载后的重试、负载读取失败、第 3 层）
// 共用，降段结果跨轮保留。nil 表示计划输入未生效，全部挂点照旧。
type kongPlanSelection struct {
	now    time.Time
	facts  map[int64]kongPlanFacts
	minSeg map[int64]kongPlanSeg
	// keys 是本轮各账号的位置（未降段时的那一个），节奏重排按它分桶。
	keys   map[int64]kongPlanBucketKey
	slots  []kongPlanSlot
	cursor int
	cur    *kongPlanSlot
	// 决策记录：所用输入与发布、本轮的事实（含负载）、是否在第 3 层（选中即排队等槽）。
	stats            *KongPlanStats
	inUse            string
	planVersion, seq int64
	round            map[int64]kongPlanFacts
	waiting          bool
}

// kongPlanInEffect 表示计划输入（快照或兜底）此刻生效。
func kongPlanInEffect() bool {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return false
	}
	return rt.store.current().inUse(rt.now()) != KongPlanInUseLegacy
}

// kongPlanBegin 在第 2 层建好候选与会话分段之后调用：给每个候选定层、取会话数与短时消耗，并接到节奏决策上。
// 最新读数显示账号回到第一层、而它还有没离层的入层时，在这里登记离层。
func (s *OpenAIGatewayService) kongPlanBegin(ctx context.Context, candidates []*Account, g *kongSessionGate, pace *kongPaceDecision) *kongPlanSelection {
	rt := kongPlanRuntimeNow()
	if rt == nil || rt.store == nil {
		return nil
	}
	now := rt.now()
	st := rt.store.current()
	if st.inUse(now) == KongPlanInUseLegacy {
		return nil
	}
	var cfg *KongPaceConfig
	if rt.pace != nil {
		if loaded := rt.pace.active.Load(); loaded != nil {
			cfg = &loaded.cfg
		}
	}
	p := &kongPlanSelection{now: now, facts: make(map[int64]kongPlanFacts, len(candidates)), minSeg: map[int64]kongPlanSeg{}}
	p.attachStats(rt, st, now)
	for _, acc := range candidates {
		f := kongPlanFacts{ID: acc.ID, Tier: rt.tier(ctx, acc, now), Priority: acc.Priority}
		if f.Tier.Layer != 0 {
			f.SessionLimit = kongPlanEffectiveMaxSessions(f.Tier.Entry, acc)
			f.Grace = kongPlanGrace(st.control, acc.ID)
			if g != nil {
				f.Sessions, f.SessionsKnown = g.counts[acc.ID]
			}
			ps, _ := rt.paceState(acc.ID)
			f.Inc6 = kongPaceIncrease(ps, now, 6*time.Hour)
			f.Inc24 = kongPaceIncrease(ps, now, 24*time.Hour)
			if cfg != nil {
				_, _, _, f.PaceFull = kongPaceShortPenalty(f.Inc6, f.Inc24, cfg.plan(rt.planType(acc)), cfg.MaxPenaltyPP)
			}
			if f.Tier.Layer == kongPlanLayerSubscription {
				rt.markLeft(ctx, acc, ps, now)
			}
		}
		p.facts[acc.ID] = f
	}
	pace.attachPlan(p)
	return p
}

// planType 取账号的套餐：节奏快照里有就用它，否则用凭据里的。
func (rt *kongPlanRuntime) planType(acc *Account) string {
	if snap := rt.pace.snapshot.Load(); snap != nil {
		if f, ok := snap.accounts[acc.ID]; ok {
			return f.Plan
		}
	}
	return acc.GetCredential("plan_type")
}

// markLeft 为回到第一层的账号登记离层：每个有入层、还没离层的窗口各一次。旧窗口回报的读数（重置时刻比节奏状态里的
// 当前窗口早出容差）不算。写失败只记日志，下一次请求再试。
func (rt *kongPlanRuntime) markLeft(ctx context.Context, acc *Account, ps *kongPaceAccountState, now time.Time) {
	if rt.ledger == nil {
		return
	}
	var pending []int64
	for _, e := range rt.ledger.Entries(acc.ID) {
		if !e.Settled && e.LeftAt == nil {
			pending = append(pending, e.Seq)
		}
	}
	if len(pending) == 0 {
		return
	}
	if ps != nil && ps.HasWindow {
		if reset := parseExtraTime(acc.Extra["codex_7d_reset_at"]); !reset.IsZero() && ps.ResetAt.Sub(reset) > kongPaceWindowTolerance {
			return
		}
	}
	at := kongPlanReadingTime(acc, now)
	for _, seq := range pending {
		if err := rt.ledger.MarkLeft(ctx, acc.ID, seq, at); err != nil {
			kongPlanWarn("kong plan: 离层登记失败，下一次请求再试", "account_id", acc.ID, "window_seq", seq, "error", err)
			return
		}
	}
}

// kongPlanReadingTime 是账号读数的源查询时刻，缺失时取当前时刻。
func kongPlanReadingTime(acc *Account, now time.Time) time.Time {
	if at := parseExtraTime(acc.Extra["codex_usage_updated_at"]); !at.IsZero() {
		return at
	}
	return now
}

var kongPlanLastWarn atomic.Int64

// kongPlanWarn 是选号路径上的告警，每分钟最多一条：Redis 故障时每个请求都会撞上同一个错误。
func kongPlanWarn(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := kongPlanLastWarn.Load()
	if now-last < int64(time.Minute) || !kongPlanLastWarn.CompareAndSwap(last, now) {
		return
	}
	slog.Warn(msg, args...)
}

// roundFacts 是一个账号在本轮的事实：进入时取的那份加上本轮的负载。
func (p *kongPlanSelection) roundFacts(id int64, load *AccountLoadInfo, loadKnown bool) kongPlanFacts {
	f, ok := p.facts[id]
	if !ok {
		f = kongPlanFacts{ID: id}
	}
	f.LoadKnown = loadKnown
	f.ConcurrencyFull = loadKnown && load != nil && load.LoadRate >= 100
	return f
}

// preferRoomLoads 是第 2 层主循环里会话分段的挂点：计划生效时不按会话余量过滤（全部账号都在序列里），并按本轮的
// 负载给账号分桶，供节奏重排使用；否则照旧。
func (p *kongPlanSelection) preferRoomLoads(g *kongSessionGate, available []accountWithLoad) []accountWithLoad {
	if p == nil {
		return g.preferRoomLoads(available)
	}
	p.keys = make(map[int64]kongPlanBucketKey, len(available))
	for _, item := range available {
		f := p.roundFacts(item.account.ID, item.loadInfo, true)
		p.keys[f.ID] = kongPlanSlotFor(&f, kongPlanSegWith(&f, p.minSeg)).Key
	}
	return available
}

// groupByBucket 把列表按本轮的桶稳定分组，桶内保持原顺序。节奏重排在分组后的列表上按（桶，原优先级）分段。
func (p *kongPlanSelection) groupByBucket(available []accountWithLoad) []accountWithLoad {
	out := append([]accountWithLoad(nil), available...)
	sort.SliceStable(out, func(i, j int) bool {
		return kongPlanKeyLess(p.keys[out[i].account.ID], p.keys[out[j].account.ID])
	})
	return out
}

func (p *kongPlanSelection) sameBucket(a, b int64) bool {
	return p.keys[a] == p.keys[b]
}

// sequenceLoads 是第 2 层主循环里的挂点：计划生效时把这一轮的最终顺序按桶重排为尝试序列；否则原样返回。
func (p *kongPlanSelection) sequenceLoads(order []accountWithLoad) []accountWithLoad {
	if p == nil {
		return order
	}
	facts := make([]kongPlanFacts, 0, len(order))
	byID := make(map[int64]accountWithLoad, len(order))
	for _, item := range order {
		byID[item.account.ID] = item
		facts = append(facts, p.roundFacts(item.account.ID, item.loadInfo, true))
	}
	p.start(kongPlanOrder(facts, p.minSeg))
	out := make([]accountWithLoad, len(p.slots))
	for i, s := range p.slots {
		out[i] = byID[s.ID]
	}
	return out
}

// sequenceAccounts 用于负载读取失败的分支与第 3 层：负载未知，按忙处理，轮次、组序与层序照常。
func (p *kongPlanSelection) sequenceAccounts(accounts []*Account) []*Account {
	facts := make([]kongPlanFacts, 0, len(accounts))
	byID := make(map[int64]*Account, len(accounts))
	for _, acc := range accounts {
		byID[acc.ID] = acc
		facts = append(facts, p.roundFacts(acc.ID, nil, false))
	}
	p.keepRound(facts)
	p.start(kongPlanOrder(facts, p.minSeg))
	out := make([]*Account, len(p.slots))
	for i, s := range p.slots {
		out[i] = byID[s.ID]
	}
	return out
}

// preferRoomAccounts 是负载读取失败分支的挂点。
func (p *kongPlanSelection) preferRoomAccounts(g *kongSessionGate, accounts []*Account) []*Account {
	if p == nil {
		return g.preferRoomAccounts(accounts)
	}
	return p.sequenceAccounts(accounts)
}

// roomFirst 是第 3 层的挂点。
func (p *kongPlanSelection) roomFirst(g *kongSessionGate, accounts []*Account) []*Account {
	if p == nil {
		return g.roomFirst(accounts)
	}
	p.waiting = true
	return p.sequenceAccounts(accounts)
}

func (p *kongPlanSelection) start(slots []kongPlanSlot) {
	p.slots, p.cursor, p.cur = slots, 0, nil
}

// next 在尝试序列的每个位置开头调用：降段重试的位置只在账号降到这一段时尝试，其余跳过。
func (p *kongPlanSelection) next(id int64) bool {
	if p == nil {
		return true
	}
	p.cur = nil
	if p.cursor >= len(p.slots) {
		return true
	}
	s := &p.slots[p.cursor]
	p.cursor++
	if s.ID != id {
		return true
	}
	if s.Ghost && p.minSeg[id] != s.Key.Seg {
		return false
	}
	p.cur = s
	return true
}

// confirm 在选定账号之后确认：credits 层的账号先登记入层（写不进去就不用它），再按所在位置的上限登记会话。名额已被
// 别的请求占满时，账号按段向后降，返回 false，调用方换下一个位置。计划未生效时照旧。
func (p *kongPlanSelection) confirm(ctx context.Context, g *kongSessionGate, account *Account) bool {
	if p == nil {
		return g.confirm(ctx, account)
	}
	f := p.facts[account.ID]
	// 入层按复核后的最新读数判断：进入第 2 层时的读数可能还在第一层，复核时已到阈值、按 credits 层放行了。这样的账号
	// 不在第一层的位置上用（后面可能还有有周额度的账号）：记下新的定层、放弃这个位置，在本轮序列里为它预留的 credits
	// 段位置上再试；之后的轮次按 credits 层排它。
	if rt := kongPlanRuntimeNow(); rt != nil && account.IsOpenAIOAuth() {
		now := rt.now()
		if res := rt.tier(ctx, account, now); res.Layer == kongPlanLayerCredits {
			if f.Tier.Layer == kongPlanLayerSubscription {
				// 改判位置是按选号开始时的计划暂停排的：选号途中快照换了也沿用开始时的，降到的段才对得上预留的位置
				res.Entry.Paused = f.Tier.Entry.Paused
				f.Tier = res
				p.facts[account.ID] = f
				p.retierRound(account.ID, res)
				p.minSeg[account.ID] = kongPlanNaturalSeg(&f) // 激活序列里为它预留的 credits 段位置
				return false
			}
			if rt.enterCredits(ctx, account, res, now) != nil {
				return false
			}
		}
	}
	s := p.cur
	if s == nil || s.ID != account.ID {
		ok := g.confirm(ctx, account)
		if ok {
			p.recordSelected(account.ID, nil)
		}
		return ok
	}
	if g == nil || s.Limit == 0 || g.confirmLimit(ctx, account, s.Limit) {
		p.recordSelected(account.ID, s)
		return true
	}
	p.recordConfirmFailed(account.ID, s)
	if next, ok := kongPlanDemote(&f, s.Key.Seg); ok {
		p.minSeg[account.ID] = next
	}
	return false
}

// kongPlanLoadBatch 读取第 2 层的负载。计划生效、而旧版选号本该走非批量分支（没有并发服务或关闭了批量负载）时，
// 按负载读取失败处理，走完整的尝试循环。
func (s *OpenAIGatewayService) kongPlanLoadBatch(ctx context.Context, batchEnabled bool, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if s.concurrencyService == nil || !batchEnabled {
		return nil, errKongPlanNoLoadBatch
	}
	return s.concurrencyService.GetAccountsLoadBatch(ctx, accounts)
}

var errKongPlanNoLoadBatch = errors.New("load batch unavailable")
