package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 账号消耗节奏（设计见 DESIGN-openai-account-pace.md）：只调整旧版选号第 2 层的新会话尝试顺序，
// 让每个 OpenAI OAuth 账号的额度消耗进度跟上它自己的时间进度，同时避开短时用得太快、并发已紧张的
// 账号。只做软约束：不排除账号、不迁移已绑定会话、任何异常都退回原顺序。

const (
	kongPaceFactsInterval    = time.Minute
	kongPaceSampleInterval   = 5 * time.Second
	kongPaceSnapshotMaxAge   = 5 * time.Minute
	kongPaceStateTTL         = 10 * time.Minute
	kongPaceSummaryInterval  = 10 * time.Minute
	kongPaceSweepInterval    = 24 * time.Hour
	kongPaceIOTimeout        = 10 * time.Second
	kongPaceDecisionQueueCap = 1000
	kongPaceDecisionBatch    = 100
	kongPaceDecisionFlush    = 3 * time.Second
	kongPaceSweepBatch       = 5000
)

// KongPaceRepository 是节奏组件的数据库访问（repository 层实现，见 kong_openai_account_pace_repo.go）。
type KongPaceRepository interface {
	ListOpenAIOAuthAccounts(ctx context.Context) ([]KongPaceAccountRow, error)
	InsertDecisions(ctx context.Context, records []KongPaceDecisionRecord) error
	DeleteDecisionsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// KongPaceStore 是节奏组件的 Redis 访问。账号状态以 JSON 原样存取，编码由本组件负责。
type KongPaceStore interface {
	LoadAccountStates(ctx context.Context) (map[int64][]byte, error)
	SaveAccountStates(ctx context.Context, states map[int64][]byte, deleted []int64) error
	PublishState(ctx context.Context, accounts map[int64][]byte, meta []byte, ttl time.Duration) error
}

// kongPaceLoadReader 是读取账号"在途 + 排队"的接口（ConcurrencyService 满足）。
type kongPaceLoadReader interface {
	GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
}

// KongPaceDecisionRecord 是一条决策记录（设计文档 4.4），对应表 kong_openai_pace_decisions。
type KongPaceDecisionRecord struct {
	CreatedAt        time.Time
	RequestID        string
	ClientRequestID  string
	UserID           int64
	GroupID          int64
	Model            string
	SessionHash      string
	Reason           string
	Applied          bool
	NotAppliedReason string
	ConfigVersion    string
	Config           json.RawMessage
	SnapshotAgeMs    int64
	Rounds           json.RawMessage
	Outcome          string
	AccountID        int64
	AttemptIndex     int
}

// kongPaceFacts 是一个账号在事实快照里的样子：额度状态与账号资料，与调参无关。
type kongPaceFacts struct {
	Plan                  string
	Concurrency           int
	SubscriptionExpiresAt *time.Time
	// LastUsedAt 只供发布状态与周期日志；选号用候选自带的值（调度缓存里的更新）。
	LastUsedAt *time.Time
	State      kongPaceAccountState
}

type kongPaceSnapshot struct {
	at       time.Time
	accounts map[int64]kongPaceFacts
}

// KongOpenAIAccountPace 是节奏组件。协程总是运行（要能发现后来放进去的配置文件），配置文件不存在时
// 每拍只检查一次文件。
type KongOpenAIAccountPace struct {
	repo   KongPaceRepository
	store  KongPaceStore
	loads  kongPaceLoadReader
	config *kongPaceConfigSource
	now    func() time.Time
	rand   func() float64

	// active 是生效配置。选号只看它决定是否重排：enabled: false 或文件删除在检查配置时就生效，
	// 不依赖随后的查库成功。
	active   atomic.Pointer[kongPaceLoadedConfig]
	snapshot atomic.Pointer[kongPaceSnapshot]
	peaks    kongPacePeaks

	// 以下只由刷新协程访问。
	states      map[int64]*kongPaceAccountState
	statesReady bool
	// pendingDeletes 是已从内存移除、Redis 里的键还没删成功的账号。
	pendingDeletes map[int64]struct{}
	lastSweep      time.Time
	lastSummary    time.Time

	decisions chan KongPaceDecisionRecord
	counters  kongPaceCounters

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

type kongPaceCounters struct {
	decisions        atomic.Int64
	notApplied       atomic.Int64
	atLineChosen     atomic.Int64
	rollbacks        atomic.Int64
	decisionsDropped atomic.Int64
}

// NewKongOpenAIAccountPace 创建节奏组件。configPath 是数据目录下的配置文件路径。
func NewKongOpenAIAccountPace(repo KongPaceRepository, store KongPaceStore, loads kongPaceLoadReader, configPath string) *KongOpenAIAccountPace {
	return &KongOpenAIAccountPace{
		repo:           repo,
		store:          store,
		loads:          loads,
		config:         newKongPaceConfigSource(configPath),
		now:            time.Now,
		rand:           rand.Float64,
		peaks:          kongPacePeaks{byAccount: map[int64][]kongPacePeakBucket{}},
		states:         map[int64]*kongPaceAccountState{},
		pendingDeletes: map[int64]struct{}{},
		decisions:      make(chan KongPaceDecisionRecord, kongPaceDecisionQueueCap),
		stop:           make(chan struct{}),
	}
}

// Start 启动刷新协程与决策写入协程。
func (p *KongOpenAIAccountPace) Start() {
	if p == nil {
		return
	}
	p.wg.Add(2)
	go p.runLoop()
	go p.runDecisionWriter()
}

// Stop 停止协程，等待决策队列里已有的记录写完（受写库超时约束）。
func (p *KongOpenAIAccountPace) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
}

func (p *KongOpenAIAccountPace) runLoop() {
	defer p.wg.Done()
	p.factsTick()
	facts := time.NewTicker(kongPaceFactsInterval)
	defer facts.Stop()
	sample := time.NewTicker(kongPaceSampleInterval)
	defer sample.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-facts.C:
			p.factsTick()
		case <-sample.C:
			p.sampleTick()
		}
	}
}

// factsTick 是每分钟一拍（设计文档 4.1）。
func (p *KongOpenAIAccountPace) factsTick() {
	now := p.now()
	loaded, ev, err := p.config.check(now)
	switch ev {
	case kongPaceConfigLoaded:
		slog.Info("kong pace: 配置已加载", "path", p.config.path, "version", loaded.version, "enabled", loaded.cfg.Enabled)
	case kongPaceConfigRemoved:
		slog.Info("kong pace: 配置文件已不存在，功能关闭", "path", p.config.path)
	case kongPaceConfigInvalid:
		if loaded == nil {
			slog.Error("kong pace: 配置文件非法，功能不启用", "path", p.config.path, "error", err)
		} else {
			slog.Warn("kong pace: 配置文件非法，沿用上一份有效配置", "path", p.config.path, "version", loaded.version, "error", err)
		}
	}
	p.active.Store(loaded)
	if loaded == nil {
		p.snapshot.Store(nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), kongPaceIOTimeout)
	defer cancel()
	if !p.statesReady && !p.restoreStates(ctx) {
		return
	}
	rows, err := p.repo.ListOpenAIOAuthAccounts(ctx)
	if err != nil {
		slog.Warn("kong pace: 读取账号额度失败，沿用上一份快照", "error", err)
		return
	}

	snap := &kongPaceSnapshot{at: now, accounts: make(map[int64]kongPaceFacts, len(rows))}
	seen := make(map[int64]bool, len(rows))
	for _, row := range rows {
		seen[row.ID] = true
		st, obs := kongPaceObserve(p.states[row.ID], row, now)
		p.states[row.ID] = st
		switch obs {
		case kongPaceObsRollback:
			p.counters.rollbacks.Add(1)
			slog.Info("kong pace: 读数判为回滚，已忽略", "account_id", row.ID, "reset_at", row.ResetAt, "used", row.UsedPercent)
		case kongPaceObsPlanChange:
			slog.Info("kong pace: 套餐变化，短时消耗重新统计", "account_id", row.ID, "plan", row.Plan)
		}
		facts := kongPaceFacts{Plan: row.Plan, Concurrency: row.Concurrency, SubscriptionExpiresAt: row.SubscriptionExpiresAt, LastUsedAt: row.LastUsedAt, State: *st}
		facts.State.Rises = append([]kongPaceRise(nil), st.Rises...)
		snap.accounts[row.ID] = facts
	}
	var deleted []int64
	for id := range p.states {
		if !seen[id] {
			delete(p.states, id)
			deleted = append(deleted, id)
			p.pendingDeletes[id] = struct{}{}
		}
	}
	for id := range p.pendingDeletes {
		// 重新出现的账号不能再删：同一批里先写后删会把刚写的状态删掉。
		if seen[id] {
			delete(p.pendingDeletes, id)
		}
	}
	p.peaks.forget(deleted)
	p.snapshot.Store(snap)

	p.persist(ctx)
	p.publish(ctx, loaded, snap, now)
	p.maybeSweepDecisions(loaded, now)
	p.maybeLogSummary(loaded, snap, now)
}

// restoreStates 在第一拍从 Redis 载入各账号状态，成功才返回 true。读取失败时这一拍不推进状态、
// 下一拍重试：从空开始会用新起点覆盖 Redis 里仍在的历史。读到了但某个账号没有状态时，该账号从空开始。
func (p *KongOpenAIAccountPace) restoreStates(ctx context.Context) bool {
	raw, err := p.store.LoadAccountStates(ctx)
	if err != nil {
		slog.Warn("kong pace: 载入账号状态失败，下一拍重试", "error", err)
		return false
	}
	p.statesReady = true
	for id, data := range raw {
		var st kongPaceAccountState
		if err := json.Unmarshal(data, &st); err != nil {
			slog.Warn("kong pace: 账号状态无法解析，该账号从空开始", "account_id", id, "error", err)
			continue
		}
		p.states[id] = &st
	}
	return true
}

func (p *KongOpenAIAccountPace) persist(ctx context.Context) {
	out := make(map[int64][]byte, len(p.states))
	for id, st := range p.states {
		data, err := json.Marshal(st)
		if err != nil {
			slog.Warn("kong pace: 账号状态无法编码", "account_id", id, "error", err)
			continue
		}
		out[id] = data
	}
	deleted := make([]int64, 0, len(p.pendingDeletes))
	for id := range p.pendingDeletes {
		deleted = append(deleted, id)
	}
	// 每拍整体写一遍：上一拍写失败的内容（包括没删成的键）这一拍一并补上。
	if err := p.store.SaveAccountStates(ctx, out, deleted); err != nil {
		slog.Warn("kong pace: 写入账号状态失败", "error", err)
		return
	}
	clear(p.pendingDeletes)
}

// sampleTick 每 5 秒读一次全部账号的"在途 + 排队"，覆盖不经过第 2 层的粘性会话请求。
func (p *KongOpenAIAccountPace) sampleTick() {
	if p.active.Load() == nil {
		return
	}
	snap := p.snapshot.Load()
	if snap == nil || p.loads == nil {
		return
	}
	req := make([]AccountWithConcurrency, 0, len(snap.accounts))
	for id, f := range snap.accounts {
		req = append(req, AccountWithConcurrency{ID: id, MaxConcurrency: max(f.Concurrency, 1)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), kongPaceSampleInterval)
	defer cancel()
	loads, err := p.loads.GetAccountsLoadBatch(ctx, req)
	if err != nil {
		return
	}
	now := p.now()
	for id, info := range loads {
		if info != nil {
			p.peaks.observe(id, info.CurrentConcurrency+info.WaitingCount, now)
		}
	}
}

// kongPaceAccountView 是按一份配置算出的账号状态：写进 Redis state、决策记录的候选明细，
// 选号时排序也用它。
type kongPaceAccountView struct {
	AccountID   int64  `json:"account_id"`
	Plan        string `json:"plan"`
	Priority    int    `json:"priority,omitempty"`
	CompactTier int    `json:"compact_tier,omitempty"`
	// Rate 是按倍率排序时该账号的调度倍率；倍率不参与排序（未开启或各账号相同）时为空。
	Rate           *float64   `json:"rate,omitempty"`
	Known          bool       `json:"known"`
	Running        bool       `json:"running"`
	Used           float64    `json:"used"`
	WindowStart    *time.Time `json:"window_start,omitempty"`
	Deadline       *time.Time `json:"deadline,omitempty"`
	DeadlineSource string     `json:"deadline_source,omitempty"`
	TimeProgress   float64    `json:"time_progress"`
	Gap            float64    `json:"gap"`
	Capacity       float64    `json:"capacity"`
	Yield          float64    `json:"yield"`
	Inc6           float64    `json:"inc6"`
	Inc24          float64    `json:"inc24"`
	Q6             float64    `json:"q6"`
	Q24            float64    `json:"q24"`
	Q              float64    `json:"q"`
	AtLine         bool       `json:"at_line"`
	Limit          int        `json:"concurrency_limit"`
	Peak           int        `json:"occupancy_peak"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	C              float64    `json:"c"`
	// 会话占用只在选号时有（来自会话上限的分段）；不受会话上限约束的候选与 state 里没有这几项，state 里的 w
	// 也就不含 M。
	ActiveSessions *int     `json:"active_sessions,omitempty"`
	MaxSessions    int      `json:"max_sessions,omitempty"`
	M              *float64 `json:"m,omitempty"`
	Weight         float64  `json:"w"`
	// Paced 表示所在的段按节奏排序（段内全是 OpenAI OAuth）；FirstChoice 只对这样的段有意义。
	Paced        bool       `json:"paced,omitempty"`
	FirstChoice  float64    `json:"first_choice,omitempty"`
	TrackedSince *time.Time `json:"tracked_since,omitempty"`
}

// view 计算一个账号的节奏状态。f 为 nil 表示快照里没有这个账号（刚加入）：按进度正常处理。
// sess 为 nil 表示没有会话计数，M = 0。
func (p *KongOpenAIAccountPace) view(cfg *KongPaceConfig, id int64, plan string, limit int, f *kongPaceFacts, current int, lastUsed *time.Time, sess *kongPaceSessionUse, now time.Time) kongPaceAccountView {
	v := kongPaceAccountView{AccountID: id, Plan: plan, Limit: limit, LastUsedAt: lastUsed}
	var st *kongPaceAccountState
	var expires *time.Time
	if f != nil {
		v.Known = true
		st, expires = &f.State, f.SubscriptionExpiresAt
		if !st.TrackedSince.IsZero() {
			v.TrackedSince = kongPaceTimePtr(st.TrackedSince)
		}
	}
	prog := kongPaceComputeProgress(st, expires, now)
	v.Running, v.Used, v.TimeProgress, v.Gap, v.DeadlineSource = prog.Running, prog.Used, prog.TimeProgress, prog.Gap, prog.DeadlineSource
	if !prog.WindowStart.IsZero() {
		v.WindowStart = kongPaceTimePtr(prog.WindowStart)
	}
	if !prog.Deadline.IsZero() {
		v.Deadline = kongPaceTimePtr(prog.Deadline)
	}

	pp := cfg.plan(plan)
	v.Capacity, v.Yield = pp.Capacity, pp.YieldPP
	v.Inc6 = kongPaceIncrease(st, now, 6*time.Hour)
	v.Inc24 = kongPaceIncrease(st, now, 24*time.Hour)
	v.Q6, v.Q24, v.Q, v.AtLine = kongPaceShortPenalty(v.Inc6, v.Inc24, pp, cfg.MaxPenaltyPP)

	window := time.Duration(cfg.Concurrency.PeakWindowMinutes) * time.Minute
	v.Peak = max(p.peaks.peak(id, now, window), current)
	recentlyUsed := lastUsed != nil && now.Sub(*lastUsed) < window
	v.C = kongPaceConcurrencyAdjust(v.Peak, limit, recentlyUsed, cfg.Concurrency)
	var m float64
	if sess != nil {
		m = kongPaceSessionAdjust(*sess, cfg.Sessions)
		v.ActiveSessions, v.MaxSessions, v.M = sess.Active, sess.Limit, &m
	}
	v.Weight = kongPaceWeight(v.Gap, pp, v.Q, v.C, m, cfg)
	return v
}

func (p *KongOpenAIAccountPace) publish(ctx context.Context, loaded *kongPaceLoadedConfig, snap *kongPaceSnapshot, now time.Time) {
	cfg := &loaded.cfg
	accounts := make(map[int64][]byte, len(snap.accounts))
	for id, f := range snap.accounts {
		f := f
		v := p.view(cfg, id, f.Plan, f.Concurrency, &f, 0, f.LastUsedAt, nil, now)
		data, err := json.Marshal(struct {
			kongPaceAccountView
			UpdatedAt time.Time `json:"updated_at"`
		}{v, now})
		if err != nil {
			slog.Warn("kong pace: 账号状态无法编码，未发布", "account_id", id, "error", err)
			continue
		}
		accounts[id] = data
	}
	_, lastErr := p.config.status()
	meta, err := json.Marshal(map[string]any{
		"config":            cfg,
		"config_version":    loaded.version,
		"config_loaded_at":  loaded.loadedAt,
		"config_error":      lastErr,
		"facts_at":          snap.at,
		"decisions_dropped": p.counters.decisionsDropped.Load(),
		"updated_at":        now,
	})
	if err != nil {
		slog.Warn("kong pace: meta 无法编码，本拍未发布", "error", err)
		return
	}
	if err := p.store.PublishState(ctx, accounts, meta, kongPaceStateTTL); err != nil {
		slog.Warn("kong pace: 写入 Redis 状态失败", "error", err)
	}
}

func (p *KongOpenAIAccountPace) maybeLogSummary(loaded *kongPaceLoadedConfig, snap *kongPaceSnapshot, now time.Time) {
	if !p.lastSummary.IsZero() && now.Sub(p.lastSummary) < kongPaceSummaryInterval {
		return
	}
	p.lastSummary = now
	ids := make([]int64, 0, len(snap.accounts))
	for id := range snap.accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	cfg := &loaded.cfg
	for _, id := range ids {
		f := snap.accounts[id]
		v := p.view(cfg, id, f.Plan, f.Concurrency, &f, 0, f.LastUsedAt, nil, now)
		slog.Info("kong pace: 账号状态", "account_id", id, "plan", v.Plan, "used", v.Used, "gap", v.Gap,
			"deadline_source", v.DeadlineSource, "inc6", v.Inc6, "inc24", v.Inc24, "q", v.Q, "at_line", v.AtLine,
			"peak", v.Peak, "limit", v.Limit, "c", v.C, "w", v.Weight)
	}
	slog.Info("kong pace: 周期计数", "enabled", cfg.Enabled, "config_version", loaded.version,
		"decisions", p.counters.decisions.Swap(0), "not_applied", p.counters.notApplied.Swap(0),
		"at_line_chosen_with_alternative", p.counters.atLineChosen.Swap(0),
		"rollback_readings", p.counters.rollbacks.Swap(0), "decisions_dropped", p.counters.decisionsDropped.Load())
}

func (p *KongOpenAIAccountPace) maybeSweepDecisions(loaded *kongPaceLoadedConfig, now time.Time) {
	if !p.lastSweep.IsZero() && now.Sub(p.lastSweep) < kongPaceSweepInterval {
		return
	}
	p.lastSweep = now
	cutoff := now.Add(-time.Duration(loaded.cfg.DecisionLog.RetentionDays) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var total int64
	for {
		n, err := p.repo.DeleteDecisionsBefore(ctx, cutoff, kongPaceSweepBatch)
		total += n
		if err != nil {
			slog.Warn("kong pace: 决策记录清理失败", "cutoff", cutoff, "deleted", total, "error", err)
			return
		}
		if n < kongPaceSweepBatch {
			break
		}
	}
	if total > 0 {
		slog.Info("kong pace: 决策记录清理完成", "cutoff", cutoff, "deleted", total)
	}
}

func (p *KongOpenAIAccountPace) enqueueDecision(rec KongPaceDecisionRecord) {
	select {
	case p.decisions <- rec:
	default:
		p.counters.decisionsDropped.Add(1)
	}
}

// runDecisionWriter 批量写决策记录，不阻塞选号。写库失败的批次留到下一次重试，积压超过队列容量
// 时丢弃最旧的并计数。
func (p *KongOpenAIAccountPace) runDecisionWriter() {
	defer p.wg.Done()
	ticker := time.NewTicker(kongPaceDecisionFlush)
	defer ticker.Stop()
	var pending []KongPaceDecisionRecord
	flush := func() {
		for len(pending) > 0 {
			n := min(len(pending), kongPaceDecisionBatch)
			ctx, cancel := context.WithTimeout(context.Background(), kongPaceIOTimeout)
			err := p.repo.InsertDecisions(ctx, pending[:n])
			cancel()
			if err != nil {
				slog.Warn("kong pace: 写入决策记录失败，稍后重试", "pending", len(pending), "error", err)
				if over := len(pending) - kongPaceDecisionQueueCap; over > 0 {
					pending = pending[over:]
					p.counters.decisionsDropped.Add(int64(over))
				}
				return
			}
			pending = pending[n:]
		}
	}
	for {
		select {
		case <-p.stop:
			for {
				select {
				case rec := <-p.decisions:
					pending = append(pending, rec)
				default:
					flush()
					return
				}
			}
		case rec := <-p.decisions:
			pending = append(pending, rec)
			if len(pending) >= kongPaceDecisionBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// kongPacePeaks 保存每个账号近若干分钟"在途 + 排队"的逐分钟最大值。
type kongPacePeaks struct {
	mu        sync.Mutex
	byAccount map[int64][]kongPacePeakBucket
}

type kongPacePeakBucket struct {
	minute time.Time
	max    int
}

// kongPacePeakKeep 是保留的分钟数，也是配置允许的最长峰值窗口。
const kongPacePeakKeep = 60

// observe 记一次观测。选号与采样的时刻可能交错到达（选号用进入第 2 层的时刻），所以按分钟找到
// 已有的桶合并、保持按时间排序，按时间而不是桶数淘汰。
func (k *kongPacePeaks) observe(id int64, value int, now time.Time) {
	minute := now.Truncate(time.Minute)
	k.mu.Lock()
	defer k.mu.Unlock()
	buckets := k.byAccount[id]
	i := len(buckets)
	for i > 0 && buckets[i-1].minute.After(minute) {
		i--
	}
	if i > 0 && buckets[i-1].minute.Equal(minute) {
		buckets[i-1].max = max(buckets[i-1].max, value)
	} else {
		buckets = slices.Insert(buckets, i, kongPacePeakBucket{minute: minute, max: value})
	}
	cutoff := buckets[len(buckets)-1].minute.Add(-kongPacePeakKeep * time.Minute)
	drop := 0
	for drop < len(buckets) && buckets[drop].minute.Before(cutoff) {
		drop++
	}
	k.byAccount[id] = buckets[drop:]
}

// forget 丢掉已不存在的账号。
func (k *kongPacePeaks) forget(ids []int64) {
	if len(ids) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, id := range ids {
		delete(k.byAccount, id)
	}
}

// peak 返回近 window 内的最大值；窗口按整分钟对齐，包含当前分钟。
func (k *kongPacePeaks) peak(id int64, now time.Time, window time.Duration) int {
	since := now.Add(-window).Truncate(time.Minute)
	k.mu.Lock()
	defer k.mu.Unlock()
	best := 0
	for _, b := range k.byAccount[id] {
		if !b.minute.Before(since) {
			best = max(best, b.max)
		}
	}
	return best
}

func kongPaceTimePtr(t time.Time) *time.Time { return &t }
