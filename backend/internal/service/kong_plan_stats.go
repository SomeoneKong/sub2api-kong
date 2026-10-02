package service

import (
	"context"
	"sync"
	"time"
)

// 决策记录（fork 专有，设计见 DESIGN-account-selection.md 第 4.4 节）：计划输入生效时的每次选号在内存里按小时累计，
// 每分钟把增量累加写进 kong_dispatch_stats（ON CONFLICT 累加），保留 30 天。边车与人读它核对分派（第 6 节 V5）。

const (
	kongPlanStatsFlushEvery = time.Minute
	kongPlanStatsRetention  = 30 * 24 * time.Hour
	kongPlanStatsMaxRows    = 20000 // 写库持续失败时内存里最多攒这么多行，超出的丢弃
	kongPlanStatsDwellSeg   = "credits_reuse"
)

// KongPlanStatKey 是一行的维度。Group 只在第一层的段里有值；Seg 是选中时的段，复用 credits 层会话绑定的停留时长记在
// kongPlanStatsDwellSeg。
type KongPlanStatKey struct {
	Hour        time.Time `json:"hour"`
	AccountID   int64     `json:"account_id"`
	Layer       string    `json:"layer"`
	Group       string    `json:"group"`
	Seg         string    `json:"seg"`
	InUse       string    `json:"in_use"`
	PlanVersion int64     `json:"plan_version"`
	PublishSeq  int64     `json:"publish_seq"`
}

// KongPlanStatRow 是一行的计数。
type KongPlanStatRow struct {
	KongPlanStatKey
	// Selected 选中次数；ConfirmFailed 确认登记时名额已被占、降段的次数；Waited 第 3 层选中、排队等槽的次数。
	Selected      int64 `json:"selected"`
	ConfirmFailed int64 `json:"confirm_failed"`
	Waited        int64 `json:"waited"`
	// SessionsSum 是选中时账号已有的会话数之和，除以 Selected 得平均会话占用（看宽限段账号此后的负担）。
	SessionsSum int64 `json:"sessions_sum"`
	// 选中 credits 层账号或中转时，排在前面、仍有周额度的第一层账号接不了的原因：已到上限加宽限、负载已满或未知、
	// 其余（暂停、复核不过、抢槽失败）。每次选中按账号各计一次。
	AheadLimit       int64 `json:"ahead_limit"`
	AheadBusy        int64 `json:"ahead_busy"`
	AheadUnavailable int64 `json:"ahead_unavailable"`
	// credits 层会话绑定复用时，会话从写下标记起已停留的时长。
	DwellN    int64 `json:"dwell_n"`
	DwellSumS int64 `json:"dwell_sum_s"`
	DwellMaxS int64 `json:"dwell_max_s"`
}

func (r *KongPlanStatRow) merge(o *KongPlanStatRow) {
	r.Selected += o.Selected
	r.ConfirmFailed += o.ConfirmFailed
	r.Waited += o.Waited
	r.SessionsSum += o.SessionsSum
	r.AheadLimit += o.AheadLimit
	r.AheadBusy += o.AheadBusy
	r.AheadUnavailable += o.AheadUnavailable
	r.DwellN += o.DwellN
	r.DwellSumS += o.DwellSumS
	if o.DwellMaxS > r.DwellMaxS {
		r.DwellMaxS = o.DwellMaxS
	}
}

// KongPlanStatsRepository 是决策记录的存储。
type KongPlanStatsRepository interface {
	// UpsertKongPlanStats 在一个事务里把各行累加进表（同维度的行相加，DwellMaxS 取大）。
	UpsertKongPlanStats(ctx context.Context, rows []KongPlanStatRow) error
	ListKongPlanStats(ctx context.Context, from, to time.Time) ([]KongPlanStatRow, error)
	PruneKongPlanStats(ctx context.Context, before time.Time) error
}

// KongPlanStats 是内存里的累计与定期写库。
type KongPlanStats struct {
	repo      KongPlanStatsRepository
	mu        sync.Mutex
	rows      map[KongPlanStatKey]*KongPlanStatRow
	flushMu   sync.Mutex
	lastPrune time.Time
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// NewKongPlanStats 创建决策记录；Start 之后才写库。
func NewKongPlanStats(repo KongPlanStatsRepository) *KongPlanStats {
	return &KongPlanStats{repo: repo, rows: map[KongPlanStatKey]*KongPlanStatRow{}, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动每分钟一次的写库。
func (s *KongPlanStats) Start() {
	if s == nil || s.repo == nil {
		return
	}
	go func() {
		defer close(s.done)
		t := time.NewTicker(kongPlanStatsFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				_ = s.Flush(context.Background())
			}
		}
	}()
}

// Stop 停止定期写库，并把剩下的写进去。
func (s *KongPlanStats) Stop() {
	if s == nil || s.repo == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Flush(ctx)
	})
}

func (s *KongPlanStats) add(key KongPlanStatKey, delta KongPlanStatRow) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rows[key]; r != nil {
		r.merge(&delta)
		return
	}
	if len(s.rows) >= kongPlanStatsMaxRows {
		return
	}
	delta.KongPlanStatKey = key
	s.rows[key] = &delta
}

// Flush 把内存里的增量写库；写失败时放回、下次再写，并返回这个错误。每天顺带清理一次 30 天前的行（清理失败只记日志）。
func (s *KongPlanStats) Flush(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	pending := s.rows
	s.rows = map[KongPlanStatKey]*KongPlanStatRow{}
	s.mu.Unlock()
	var writeErr error
	if len(pending) > 0 {
		rows := make([]KongPlanStatRow, 0, len(pending))
		for _, r := range pending {
			rows = append(rows, *r)
		}
		if err := s.repo.UpsertKongPlanStats(ctx, rows); err != nil {
			kongPlanWarn("kong plan: 决策记录写库失败，下次再写", "rows", len(rows), "error", err)
			for k, r := range pending {
				s.add(k, *r)
			}
			writeErr = err
		}
	}
	if now := time.Now(); now.Sub(s.lastPrune) >= 24*time.Hour {
		if err := s.repo.PruneKongPlanStats(ctx, now.Add(-kongPlanStatsRetention)); err != nil {
			kongPlanWarn("kong plan: 决策记录清理失败", "error", err)
		} else {
			s.lastPrune = now
		}
	}
	return writeErr
}

// List 先写库再读，读到的包含此刻之前的全部选号。增量写不进去时返回错误：读到的会缺这部分，调用方分不出
// "没有选号"和"还没写进去"。
func (s *KongPlanStats) List(ctx context.Context, from, to time.Time) ([]KongPlanStatRow, error) {
	if err := s.Flush(ctx); err != nil {
		return nil, err
	}
	return s.repo.ListKongPlanStats(ctx, from, to)
}

// ---- 选号里的记录点 ----

// attachStats 记下本次选号所用的输入，记录行的维度要用。
func (p *kongPlanSelection) attachStats(rt *kongPlanRuntime, st *kongPlanState, now time.Time) {
	p.stats = rt.stats
	p.inUse = st.inUse(now)
	if d := st.dispatch; d != nil {
		p.planVersion, p.seq = d.PlanVersion, d.Seq
	}
}

// keepLoads 是第 2 层主循环里过滤满并发账号之前的挂点：记下本轮全部候选的负载（刷新负载后的重试也会再调），满并发
// 而没进尝试序列的第一层账号同样要算作"接不了"。
func (p *kongPlanSelection) keepLoads(candidates []*Account, loads map[int64]*AccountLoadInfo) {
	if p == nil || p.stats == nil {
		return
	}
	facts := make([]kongPlanFacts, 0, len(candidates))
	for _, acc := range candidates {
		facts = append(facts, p.roundFacts(acc.ID, loads[acc.ID], true))
	}
	p.keepRound(facts)
}

// retierRound 在确认时复核改判之后同步本轮事实里的定层（负载照旧），记录行的层与"接不了"的原因按改判后的算。
func (p *kongPlanSelection) retierRound(id int64, res kongPlanTierResult) {
	if f, ok := p.round[id]; ok {
		f.Tier = res
		p.round[id] = f
	}
}

// keepRound 记下本轮各账号的事实（含负载），选中 credits 层或中转时据此给出前面的第一层账号接不了的原因。
func (p *kongPlanSelection) keepRound(facts []kongPlanFacts) {
	p.round = make(map[int64]kongPlanFacts, len(facts))
	for _, f := range facts {
		p.round[f.ID] = f
	}
}

func (p *kongPlanSelection) roundOf(id int64) kongPlanFacts {
	if f, ok := p.round[id]; ok {
		return f
	}
	return p.facts[id]
}

func (p *kongPlanSelection) statKey(id int64, layer kongPlanLayer, key kongPlanBucketKey) KongPlanStatKey {
	return KongPlanStatKey{Hour: p.now.UTC().Truncate(time.Hour), AccountID: id, Layer: layer.String(), Group: key.Group.String(),
		Seg: key.Seg.String(), InUse: p.inUse, PlanVersion: p.planVersion, PublishSeq: p.seq}
}

// recordSelected 记一次选中：所在位置的段（没有位置时按账号此刻的段）、已有会话数、是否排队等槽。
func (p *kongPlanSelection) recordSelected(id int64, s *kongPlanSlot) {
	if p.stats == nil {
		return
	}
	f := p.roundOf(id)
	key := kongPlanSlotFor(&f, kongPlanSegWith(&f, p.minSeg)).Key
	if s != nil {
		key = s.Key
	}
	row := KongPlanStatRow{Selected: 1}
	if f.SessionsKnown {
		row.SessionsSum = int64(f.Sessions)
	}
	if p.waiting {
		row.Waited = 1
	}
	p.stats.add(p.statKey(id, f.Tier.Layer, key), row)
	if key.Seg.credits() || key.Seg == kongPlanSegRelay {
		p.recordAhead(id)
	}
}

// recordAhead：选中 credits 层账号或中转时，本轮仍有周额度的第一层账号各记一次接不了的原因。降到溢出轮的是已到上限
// 加宽限（或名额被别的请求占满），负载已满或未知的是忙，其余（复核不过、抢不到槽）归一类。
func (p *kongPlanSelection) recordAhead(selected int64) {
	facts := p.round
	if facts == nil {
		facts = p.facts
	}
	for id, f := range facts {
		if id == selected || f.Tier.Layer != kongPlanLayerSubscription {
			continue
		}
		seg := kongPlanSegWith(&f, p.minSeg)
		var d KongPlanStatRow
		switch {
		case seg == kongPlanSegOverflow:
			d.AheadLimit = 1
		case !f.LoadKnown || f.ConcurrencyFull:
			d.AheadBusy = 1
		default:
			d.AheadUnavailable = 1
		}
		p.stats.add(p.statKey(id, f.Tier.Layer, kongPlanSlotFor(&f, seg).Key), d)
	}
}

// recordConfirmFailed 记一次确认登记失败（名额已被别的请求占满）。
func (p *kongPlanSelection) recordConfirmFailed(id int64, s *kongPlanSlot) {
	if p.stats == nil {
		return
	}
	f := p.roundOf(id)
	p.stats.add(p.statKey(id, f.Tier.Layer, s.Key), KongPlanStatRow{ConfirmFailed: 1})
}

// recordDwell 记一次 credits 层会话绑定的复用：会话从写下标记起已停留的时长。标记的时刻读不出（旧版写下的）时不记。
func (rt *kongPlanRuntime) recordDwell(accountID int64, markedAt, now time.Time) {
	if rt.stats == nil || markedAt.IsZero() || !now.After(markedAt) {
		return
	}
	st := rt.store.current()
	key := KongPlanStatKey{Hour: now.UTC().Truncate(time.Hour), AccountID: accountID, Layer: kongPlanLayerCredits.String(),
		Seg: kongPlanStatsDwellSeg, InUse: st.inUse(now)}
	if d := st.dispatch; d != nil {
		key.PlanVersion, key.PublishSeq = d.PlanVersion, d.Seq
	}
	sec := int64(now.Sub(markedAt) / time.Second)
	rt.stats.add(key, KongPlanStatRow{DwellN: 1, DwellSumS: sec, DwellMaxS: sec})
}
