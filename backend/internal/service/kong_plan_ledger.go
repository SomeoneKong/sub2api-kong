package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 计划组件的余额观测、入层与离层（fork 专有，设计见 DESIGN-account-selection.md 第 4.1 节、第 4.2 节第 5 步）。
//
// 观测在写账号快照之前先登记进 Redis 的待处理列表，由节奏组件的刷新协程（唯一的推进者）按顺序处理：补记与
// 推进基点写进节奏状态，处理完的观测在提交节奏状态的同一个 Redis 事务里按原值删除。入层与离层标记由请求路径写
// （每个账号每个窗口各一次），推进者结算时在同一个事务里写结算标记、移出未结算集合。
//
// 网关在内存里镜像入层记录，请求路径判断时不为它额外访问 Redis。镜像的合并顺序：请求路径写入成功后立即加进镜像；
// 只有推进者自己提交的结算，才在提交成功、换上节奏快照之后标为已结算。启动与重载时从 Redis 读回。

// KongPlanLedgerStore 是这部分状态在 Redis 里的存取（repository 实现）。
type KongPlanLedgerStore interface {
	PushObservation(ctx context.Context, raw []byte) error
	ListObservations(ctx context.Context) ([][]byte, error)
	// MarkEntered 写入层标记并把窗口序号加进账号的未结算集合；标记已存在时不改，返回已有的入层时刻与它是否仍未结算。
	MarkEntered(ctx context.Context, accountID, seq int64, at time.Time) (created bool, enteredAt time.Time, open bool, err error)
	// MarkLeft 写离层标记；已存在时不改，返回已记的离层时刻。
	MarkLeft(ctx context.Context, accountID, seq int64, at time.Time) (time.Time, error)
	// LoadOpenEntries 读回全部未结算的入层。
	LoadOpenEntries(ctx context.Context) (map[int64][]KongPlanTierEntry, error)
}

// KongPlanSettlement 是推进者提交的一次结算。
type KongPlanSettlement struct {
	AccountID int64
	Seq       int64
	At        time.Time
}

// KongPaceCommitExtra 是与节奏状态同一个 Redis 事务提交的计划组件部分。
type KongPaceCommitExtra struct {
	DoneObservations [][]byte
	Settlements      []KongPlanSettlement
}

// KongPlanLedger 是余额观测与入层记录的镜像与推进逻辑。
type KongPlanLedger struct {
	store KongPlanLedgerStore
	plan  *KongPlanStore

	mu      sync.RWMutex
	entries map[int64]map[int64]*KongPlanTierEntry
	// latest 是登记过的最新一次有数值的观测：保底判断取它与已提交基点中较新的一个，不等推进者。
	latest map[int64]kongPlanBalancePoint
}

// NewKongPlanLedger 创建记账组件。plan 提供换算率（人工约束）。
func NewKongPlanLedger(store KongPlanLedgerStore, plan *KongPlanStore) *KongPlanLedger {
	return &KongPlanLedger{store: store, plan: plan,
		entries: map[int64]map[int64]*KongPlanTierEntry{}, latest: map[int64]kongPlanBalancePoint{}}
}

func kongPlanObservationID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// kongPlanBalanceOf 取额度查询里的余额数值；没有数值（未回报、无限额度、解析不了）为 nil。
func kongPlanBalanceOf(usage *OpenAIQuotaUsage) *float64 {
	if usage == nil || usage.Credits == nil || usage.Credits.Unlimited || usage.Credits.Balance == nil {
		return nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(*usage.Credits.Balance), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil // NaN、Inf 按"无数值"：编码不进节奏状态，也不能让保底判断放行
	}
	return &v
}

// SetKongPlanLedger 接上计划组件的余额观测登记（fork 专有）。
func (s *OpenAIQuotaService) SetKongPlanLedger(l *KongPlanLedger) {
	if s != nil {
		s.kongPlanLedger = l
	}
}

// Observe 登记一次余额观测，在写账号快照之前调用：之后快照写库失败也不影响这条观测，它是一次真实的查询结果。
// 登记失败只记日志，不影响刷新本身；这次下降会在下一次观测时一并补上。
func (l *KongPlanLedger) Observe(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) {
	if l == nil || usage == nil {
		return
	}
	obs := kongPlanObservation{ID: kongPlanObservationID(), AccountID: accountID, At: kongQuotaFetchedAt(usage), Balance: kongPlanBalanceOf(usage)}
	raw, err := json.Marshal(obs)
	if err != nil {
		return
	}
	if err := l.store.PushObservation(ctx, raw); err != nil {
		slog.Warn("kong plan: 余额观测登记失败", "account_id", accountID, "error", err)
		return
	}
	l.noteLatest(obs)
}

func (l *KongPlanLedger) noteLatest(obs kongPlanObservation) {
	if obs.Balance == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.latest[obs.AccountID]; !ok || obs.At.After(cur.At) {
		l.latest[obs.AccountID] = kongPlanBalancePoint{At: obs.At, Value: *obs.Balance}
	}
}

// LatestBalance 返回账号最新的有数值余额：已提交的基点与登记过、还没处理的观测中较新的一个。
func (l *KongPlanLedger) LatestBalance(accountID int64, committed *kongPlanBalancePoint) *kongPlanBalancePoint {
	var out *kongPlanBalancePoint
	if committed != nil {
		c := *committed
		out = &c
	}
	if l == nil {
		return out
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if cur, ok := l.latest[accountID]; ok && (out == nil || cur.At.After(out.At)) {
		c := cur
		out = &c
	}
	return out
}

// Entries 返回账号的入层记录（副本），按窗口序号升序。
func (l *KongPlanLedger) Entries(accountID int64) []KongPlanTierEntry {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	m := l.entries[accountID]
	out := make([]KongPlanTierEntry, 0, len(m))
	for _, e := range m {
		c := *e
		if e.LeftAt != nil {
			at := *e.LeftAt
			c.LeftAt = &at
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (l *KongPlanLedger) entryLocked(accountID, seq int64) *KongPlanTierEntry {
	m := l.entries[accountID]
	if m == nil {
		return nil
	}
	return m[seq]
}

func (l *KongPlanLedger) putLocked(accountID int64, e *KongPlanTierEntry) {
	m := l.entries[accountID]
	if m == nil {
		m = map[int64]*KongPlanTierEntry{}
		l.entries[accountID] = m
	}
	m[e.Seq] = e
}

// MarkEntered 在账号作为 credits 层第一次承接某个窗口的请求之前调用。返回错误时调用方不把它当 credits 层。
// 镜像里已有这个窗口的记录时不访问 Redis。
func (l *KongPlanLedger) MarkEntered(ctx context.Context, accountID, seq int64, at time.Time) error {
	l.mu.RLock()
	known := l.entryLocked(accountID, seq) != nil
	l.mu.RUnlock()
	if known {
		return nil
	}
	_, enteredAt, open, err := l.store.MarkEntered(ctx, accountID, seq, at)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entryLocked(accountID, seq) == nil {
		l.putLocked(accountID, &KongPlanTierEntry{Seq: seq, EnteredAt: enteredAt, Settled: !open})
	}
	return nil
}

// MarkLeft 在最新读数显示账号回到第一层时调用（旧窗口回报的读数不算）。只对已入层、还没离层的窗口写；读数时刻不晚于
// 入层时刻的不算离层证据——那是入层之前取到的同窗口旧读数。
func (l *KongPlanLedger) MarkLeft(ctx context.Context, accountID, seq int64, at time.Time) error {
	l.mu.RLock()
	e := l.entryLocked(accountID, seq)
	need := e != nil && !e.Settled && e.LeftAt == nil && at.After(e.EnteredAt)
	l.mu.RUnlock()
	if !need {
		return nil
	}
	leftAt, err := l.store.MarkLeft(ctx, accountID, seq, at)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entryLocked(accountID, seq); e != nil && e.LeftAt == nil {
		e.LeftAt = &leftAt
	}
	return nil
}

// hasOpen 表示账号此刻有未结算的入层。
func (l *KongPlanLedger) hasOpen(accountID int64) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries[accountID] {
		if !e.Settled {
			return true
		}
	}
	return false
}

// reload 从 Redis 读回未结算的入层与待处理观测里的最新余额，替换镜像。推进者在载入节奏状态时调用。
func (l *KongPlanLedger) reload(ctx context.Context) error {
	open, err := l.store.LoadOpenEntries(ctx)
	if err != nil {
		return err
	}
	raws, err := l.store.ListObservations(ctx)
	if err != nil {
		return err
	}
	entries := map[int64]map[int64]*KongPlanTierEntry{}
	for id, list := range open {
		m := map[int64]*KongPlanTierEntry{}
		for _, e := range list {
			c := e
			m[e.Seq] = &c
		}
		entries[id] = m
	}
	l.mu.Lock()
	// 载入期间请求路径写入的记录（Redis 里已有）一并保留：新的入层整条保留，已读到的入层补上期间写下的离层时刻。
	for id, m := range l.entries {
		for seq, e := range m {
			if entries[id] == nil {
				entries[id] = map[int64]*KongPlanTierEntry{}
			}
			loaded := entries[id][seq]
			switch {
			case loaded == nil && !e.Settled:
				entries[id][seq] = e
			case loaded != nil && loaded.LeftAt == nil && e.LeftAt != nil:
				loaded.LeftAt = e.LeftAt
			}
		}
	}
	l.entries = entries
	l.mu.Unlock()
	for _, raw := range raws {
		var obs kongPlanObservation
		if json.Unmarshal(raw, &obs) == nil {
			l.noteLatest(obs)
		}
	}
	return nil
}

func (l *KongPlanLedger) perPoint() *float64 {
	if l.plan == nil {
		return nil
	}
	return l.plan.PerPoint()
}

// advance 处理待处理的观测并判断结算，直接改 states 里的节奏状态（推进者持有的工作副本）。返回要随节奏状态一起
// 提交的部分，以及提交成功之后才应用到镜像的结算。tracked 是推进者认识的账号（OpenAI OAuth、非影子）：
// 别的账号的观测直接删掉。
func (l *KongPlanLedger) advance(ctx context.Context, states map[int64]*kongPaceAccountState, tracked map[int64]bool, now time.Time) (KongPaceCommitExtra, func(), error) {
	var extra KongPaceCommitExtra
	raws, err := l.store.ListObservations(ctx)
	if err != nil {
		return extra, nil, err
	}
	type item struct {
		raw []byte
		obs kongPlanObservation
		pos int
	}
	items := make([]item, 0, len(raws))
	for i, raw := range raws {
		var obs kongPlanObservation
		if err := json.Unmarshal(raw, &obs); err != nil {
			slog.Warn("kong plan: 余额观测无法解析，删除", "error", err)
			extra.DoneObservations = append(extra.DoneObservations, raw)
			continue
		}
		items = append(items, item{raw: raw, obs: obs, pos: i})
	}
	// 同一账号按源查询时刻处理；时刻相同的按登记顺序。
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].obs.AccountID != items[j].obs.AccountID {
			return items[i].obs.AccountID < items[j].obs.AccountID
		}
		return items[i].obs.At.Before(items[j].obs.At)
	})
	perPoint := l.perPoint()
	for _, it := range items {
		extra.DoneObservations = append(extra.DoneObservations, it.raw)
		st := states[it.obs.AccountID]
		if st == nil || !tracked[it.obs.AccountID] {
			continue
		}
		kongPlanApplyObservation(st, it.obs, perPoint, l.hasOpen(it.obs.AccountID))
	}

	l.mu.RLock()
	for id, m := range l.entries {
		for _, e := range m {
			if kongPlanSettleDue(*e, states[id], now) {
				extra.Settlements = append(extra.Settlements, KongPlanSettlement{AccountID: id, Seq: e.Seq, At: now})
			}
		}
	}
	l.mu.RUnlock()
	settled := extra.Settlements
	apply := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, s := range settled {
			if e := l.entryLocked(s.AccountID, s.Seq); e != nil {
				e.Settled = true
			}
		}
		// 已结算、又不是账号当前窗口的记录不再需要：请求路径只会为当前窗口写入层标记。
		for id, m := range l.entries {
			for seq, e := range m {
				if e.Settled && (states[id] == nil || seq != states[id].WindowSeq) {
					delete(m, seq)
				}
			}
			if len(m) == 0 {
				delete(l.entries, id)
			}
		}
	}
	return extra, apply, nil
}
