package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// 选号侧的接入：旧版选号第 2 层的几个一行挂钩都落在这里（设计文档第 3、4.4 节）。
// 所有方法对 nil 接收者安全：功能未装配、配置文件不存在时一律退化为空操作、保持原顺序。

// SetKongOpenAIAccountPace 注入账号消耗节奏组件（可选依赖）。装配后调用一次；传 nil 等于不启用。
func (s *OpenAIGatewayService) SetKongOpenAIAccountPace(p *KongOpenAIAccountPace) {
	s.kongPace = p
}

// StopKongOpenAIAccountPace 在进程退出时停止节奏组件的协程。
func (s *OpenAIGatewayService) StopKongOpenAIAccountPace() {
	if s != nil && s.kongPace != nil {
		s.kongPace.Stop()
	}
}

// kongPaceRound 是第 2 层的一轮尝试：第 2 层可能用刷新后的负载再试一轮。
type kongPaceRound struct {
	Candidates []kongPaceAccountView `json:"candidates"`
	PaceOrder  []int64               `json:"pace_order"`
	// FinalOrder 是经倍率排序与 compact 分层之后的实际尝试顺序。
	FinalOrder []int64 `json:"final_order,omitempty"`
	// AcquiredIndex 是抢到的账号在 FinalOrder 中的位置；排在它前面的是复核未通过或抢槽失败的。
	AcquiredIndex int `json:"acquired_index"`
	// SessionFull 是会话数已满、不参与本轮的账号；SessionOverflow 是有余量的账号一个都没有、本轮用的就是
	// 满额账号（见 kong_openai_session_limit.go）。
	SessionFull     []int64 `json:"session_full,omitempty"`
	SessionOverflow bool    `json:"session_overflow,omitempty"`
}

// kongPaceDecision 是一次进入第 2 层的决策，从进入到离开第 2 层。
type kongPaceDecision struct {
	p      *KongOpenAIAccountPace
	loaded *kongPaceLoadedConfig
	snap   *kongPaceSnapshot
	now    time.Time
	rec    KongPaceDecisionRecord
	// applied 是这次允许重排（启用且快照新鲜）；paced 是至少有一个段按节奏排了序。两者都成立
	// 才算实际重排。
	applied    bool
	paced      bool
	notApplied string
	rounds     []kongPaceRound
	rates      openAILegacyUpstreamRateOrder
	acquiredID int64
	loadError  bool
	// sessions 是同一次第 2 层的会话分段。每一轮按它当时的状态取会话占用与满额账号：确认登记没通过的
	// 账号在之后的轮次里已是满额。
	sessions *kongSessionGate
}

// kongPaceBegin 在进入第 2 层时调用。功能未装配或配置文件不存在时返回 nil。
func (s *OpenAIGatewayService) kongPaceBegin(ctx context.Context, groupID *int64, sessionHash, model string, spillover bool) *kongPaceDecision {
	if s == nil || s.kongPace == nil {
		return nil
	}
	p := s.kongPace
	loaded := p.active.Load()
	if loaded == nil {
		return nil
	}
	now := p.now()
	d := &kongPaceDecision{p: p, loaded: loaded, snap: p.snapshot.Load(), now: now}
	d.rec = KongPaceDecisionRecord{
		CreatedAt:     now,
		GroupID:       derefGroupID(groupID),
		Model:         model,
		SessionHash:   sessionHash,
		ConfigVersion: loaded.version,
		Config:        loaded.json,
		SnapshotAgeMs: -1,
		AttemptIndex:  -1,
	}
	switch {
	case spillover:
		d.rec.Reason = "sticky_spillover"
	case sessionHash == "":
		d.rec.Reason = "no_session"
	default:
		d.rec.Reason = "new_session"
	}
	if v, _ := ctx.Value(ctxkey.RequestID).(string); v != "" {
		d.rec.RequestID = strings.TrimSpace(v)
	}
	if v, _ := ctx.Value(ctxkey.ClientRequestID).(string); v != "" {
		d.rec.ClientRequestID = strings.TrimSpace(v)
	}
	if v, _ := ctx.Value(ctxkey.UserID).(int64); v > 0 {
		d.rec.UserID = v
	}
	if d.snap != nil {
		d.rec.SnapshotAgeMs = now.Sub(d.snap.at).Milliseconds()
	}
	switch {
	case !loaded.cfg.Enabled:
		d.notApplied = "disabled"
	case d.snap == nil:
		d.notApplied = "snapshot_missing"
	case now.Sub(d.snap.at) > kongPaceSnapshotMaxAge:
		d.notApplied = "snapshot_stale"
	default:
		d.applied = true
	}
	return d
}

// reorder 在第 2 层每一轮 shuffleWithinSortGroups 之后调用，rates 是随后倍率排序用的那一份：同一 accounts.priority 连续段内，
// 段里全是 OpenAI OAuth 账号时，未到线的在前、到线的在后，各自按权重不放回加权随机；混有其他
// 类型账号的段保持原顺序。未应用（关闭、快照缺失或过期）时照样算出节奏顺序记入决策，但返回原顺序。
func (d *kongPaceDecision) reorder(available []accountWithLoad, rates openAILegacyUpstreamRateOrder) []accountWithLoad {
	if d == nil || len(available) == 0 {
		return available
	}
	d.rates = rates
	cfg := &d.loaded.cfg
	views := make([]kongPaceAccountView, len(available))
	for i, item := range available {
		acc := item.account
		current := 0
		if item.loadInfo != nil {
			current = item.loadInfo.CurrentConcurrency + item.loadInfo.WaitingCount
			// 第 2 层本就读到的负载也计入峰值。
			d.p.peaks.observe(acc.ID, current, d.now)
		}
		var facts *kongPaceFacts
		plan := acc.GetCredential("plan_type")
		if d.snap != nil {
			if f, ok := d.snap.accounts[acc.ID]; ok {
				facts, plan = &f, f.Plan
			}
		}
		views[i] = d.p.view(cfg, acc.ID, plan, acc.Concurrency, facts, current, acc.LastUsedAt, d.sessions.paceUse(acc), d.now)
		views[i].Priority = acc.Priority
		views[i].CompactTier = openAICompactSupportTier(acc)
		if rate, ok := rates.rates[acc.ID]; ok && rates.enabled {
			views[i].Rate = &rate
		}
	}

	order := make([]int, 0, len(available))
	for start := 0; start < len(available); {
		end := start + 1
		for end < len(available) && available[end].account.Priority == available[start].account.Priority {
			end++
		}
		run := make([]int, 0, end-start)
		paceable := true
		for i := start; i < end; i++ {
			run = append(run, i)
			acc := available[i].account
			if acc.Platform != PlatformOpenAI || acc.Type != AccountTypeOAuth {
				paceable = false
			}
		}
		if paceable {
			for _, i := range run {
				views[i].Paced = true
			}
			run = kongPaceOrderRun(run, views, d.p.rand)
			d.paced = true
		}
		order = append(order, run...)
		start = end
	}

	round := kongPaceRound{Candidates: views, PaceOrder: make([]int64, len(order)), AcquiredIndex: -1,
		SessionFull: d.sessions.fullIDs(), SessionOverflow: d.sessions.overflow()}
	reordered := make([]accountWithLoad, len(order))
	for pos, i := range order {
		round.PaceOrder[pos] = available[i].account.ID
		reordered[pos] = available[i]
	}
	d.rounds = append(d.rounds, round)
	if !d.applied {
		return available
	}
	return reordered
}

// kongPaceOrderRun 对一个优先级段排序：未到线组在前，组内按权重不放回加权随机；同时写入各账号的
// 首选概率。
func kongPaceOrderRun(run []int, views []kongPaceAccountView, rnd func() float64) []int {
	var open, atLine []int
	for _, i := range run {
		if views[i].AtLine {
			atLine = append(atLine, i)
		} else {
			open = append(open, i)
		}
	}
	first := open
	if len(first) == 0 {
		first = atLine
	}
	var total float64
	for _, i := range first {
		total += views[i].Weight
	}
	for _, i := range first {
		if total > 0 {
			views[i].FirstChoice = views[i].Weight / total
		} else {
			views[i].FirstChoice = 1 / float64(len(first))
		}
	}
	return append(kongPaceWeightedShuffle(open, views, rnd), kongPaceWeightedShuffle(atLine, views, rnd)...)
}

// kongPaceWeightedShuffle 按权重不放回抽样。权重全为 0（下溢）时退化为均匀抽样。
func kongPaceWeightedShuffle(idx []int, views []kongPaceAccountView, rnd func() float64) []int {
	rest := append([]int(nil), idx...)
	out := make([]int, 0, len(rest))
	for len(rest) > 0 {
		var total float64
		for _, i := range rest {
			total += views[i].Weight
		}
		pick := len(rest) - 1
		if total > 0 {
			target := rnd() * total
			for k, i := range rest {
				target -= views[i].Weight
				if target < 0 {
					pick = k
					break
				}
			}
		} else {
			pick = int(rnd() * float64(len(rest)))
			if pick >= len(rest) {
				pick = len(rest) - 1
			}
		}
		out = append(out, rest[pick])
		rest = append(rest[:pick], rest[pick+1:]...)
	}
	return out
}

// finalOrder 记下这一轮经倍率排序与 compact 分层之后的实际尝试顺序。
func (d *kongPaceDecision) finalOrder(order []accountWithLoad) {
	if d == nil || len(d.rounds) == 0 {
		return
	}
	ids := make([]int64, len(order))
	for i, item := range order {
		ids[i] = item.account.ID
	}
	d.rounds[len(d.rounds)-1].FinalOrder = ids
}

// acquired 记下第 2 层抢到的账号。
func (d *kongPaceDecision) acquired(accountID int64) {
	if d == nil {
		return
	}
	d.acquiredID = accountID
	if len(d.rounds) == 0 {
		return
	}
	r := &d.rounds[len(d.rounds)-1]
	for i, id := range r.FinalOrder {
		if id == accountID {
			r.AcquiredIndex = i
			d.rec.AttemptIndex = i
			break
		}
	}
}

// attachSessions 接上会话上限的分段：之后每一轮从它取会话占用、满额账号与溢出标记。
func (d *kongPaceDecision) attachSessions(g *kongSessionGate) {
	if d == nil {
		return
	}
	d.sessions = g
}

// loadFailed 记下第 2 层因负载读取失败走了降级分支（不经过重排）。
func (d *kongPaceDecision) loadFailed() {
	if d != nil {
		d.loadError = true
	}
}

// finish 在离开第 2 层时调用（defer），写计数与决策记录。
func (d *kongPaceDecision) finish() {
	if d == nil {
		return
	}
	p := d.p
	applied, notApplied := d.applied, d.notApplied
	switch {
	case d.loadError:
		applied, notApplied = false, "load_error"
	case len(d.rounds) == 0 && applied:
		applied, notApplied = false, "no_available"
	case !d.paced && applied:
		applied, notApplied = false, "mixed_segment"
	}
	if applied {
		d.countAtLineChoice()
	} else {
		p.counters.notApplied.Add(1)
	}
	p.counters.decisions.Add(1)
	if !d.loaded.cfg.DecisionLog.Enabled {
		return
	}

	rec := d.rec
	rec.Applied, rec.NotAppliedReason = applied, notApplied
	rec.AccountID = d.acquiredID
	switch {
	case d.acquiredID > 0:
		rec.Outcome = "acquired"
	case len(d.rounds) == 0 && !d.loadError:
		rec.Outcome = "no_available"
	default:
		rec.Outcome = "not_acquired"
	}
	if len(d.rounds) > 0 {
		if data, err := json.Marshal(d.rounds); err == nil {
			rec.Rounds = data
		} else {
			slog.Warn("kong pace: 决策明细无法编码，本条只记结果", "error", err)
		}
	}
	p.enqueueDecision(rec)
}

// countAtLineChoice 计数"按节奏排序的同一优先级段、同一 compact 层与倍率里有未到线账号可选、
// 却落到到线账号"（设计文档第 7 节）。
func (d *kongPaceDecision) countAtLineChoice() {
	if d.acquiredID <= 0 || len(d.rounds) == 0 {
		return
	}
	views := d.rounds[len(d.rounds)-1].Candidates
	var chosen *kongPaceAccountView
	for i := range views {
		if views[i].AccountID == d.acquiredID {
			chosen = &views[i]
		}
	}
	if chosen == nil || !chosen.Paced || !chosen.AtLine {
		return
	}
	for _, v := range views {
		if v.Paced && v.Priority == chosen.Priority && v.CompactTier == chosen.CompactTier && kongPaceSameRate(d.rates, v.AccountID, chosen.AccountID) && !v.AtLine {
			d.p.counters.atLineChosen.Add(1)
			return
		}
	}
}

// kongPaceSameRate 判断两个账号在倍率排序里是否同档：未按倍率排序时都算同档；按倍率排序时，
// 已知倍率的按值比较，未知倍率的彼此同档（排在已知之后）。
func kongPaceSameRate(rates openAILegacyUpstreamRateOrder, a, b int64) bool {
	if !rates.enabled {
		return true
	}
	ra, aKnown := rates.rates[a]
	rb, bKnown := rates.rates[b]
	return aKnown == bKnown && (!aKnown || ra == rb)
}
