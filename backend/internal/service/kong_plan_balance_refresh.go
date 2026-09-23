package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// credits 余额刷新（fork 专有，设计见 DESIGN-openai-plan-dispatch.md 第 13.5 节）：当前发布带 balance_refresh 时，网关
// 每分钟按三类选出该查余额的 OpenAI OAuth 账号去查——在用（正在 credits 层或余额在下降、近期有流量）、待用（白名单里、
// 接近阈值或在 credits 层）、离层结算（有未结算的离层记录，不看白名单）。查法与管理接口的刷新相同：查额度、写余额与用量、
// 写卡快照。同一账号的"查询并写缓存"经 KongQuotaAccountLock 串行，较早发出的查询不会晚于较新的那次落库。

const (
	kongRefreshTick    = time.Minute
	kongRefreshTimeout = 2 * time.Minute

	KongRefreshClassActive = "active"
	KongRefreshClassReady  = "ready"
	KongRefreshClassSettle = "settle"
)

var kongQuotaLocks sync.Map // 账号编号 → chan struct{}（容量 1 的信号量）

// kongQuotaLock 锁住一个账号的"查询额度并写缓存"：拿到锁返回解锁函数与 true；ctx 先结束返回空操作与 false。管理接口的
// 刷新、用卡后的刷新与网关的余额刷新都经它，同一账号的观测按查询先后落库、不倒退。
func kongQuotaLock(ctx context.Context, accountID int64) (func(), bool) {
	v, _ := kongQuotaLocks.LoadOrStore(accountID, make(chan struct{}, 1))
	ch, _ := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// KongQuotaAccountLock 是管理接口刷新用的账号锁：等锁时请求结束就不再等（随后的查询照样会因请求结束失败）。
func KongQuotaAccountLock(ctx context.Context, accountID int64) func() {
	unlock, _ := kongQuotaLock(ctx, accountID)
	return unlock
}

// KongSerializeResetQuota 包住用卡后处理的额度查询与缓存写入：查询前拿账号锁（等不到时这次刷新按失败处理），写完缓存
// 再放；查询失败或没有结果时立即放。解除限流等其余步骤不在锁内。
func KongSerializeResetQuota(q openAIQuotaResetWorkflowQuota) openAIQuotaResetWorkflowQuota {
	if q == nil {
		return nil
	}
	return &kongSerializedResetQuota{inner: q}
}

type kongSerializedResetQuota struct {
	inner  openAIQuotaResetWorkflowQuota
	unlock func()
}

func (q *kongSerializedResetQuota) QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error) {
	unlock, ok := kongQuotaLock(ctx, accountID)
	if !ok {
		return nil, ctx.Err()
	}
	usage, err := q.inner.QueryUsage(ctx, accountID)
	if err != nil || usage == nil {
		unlock()
		return usage, err
	}
	q.unlock = unlock
	return usage, nil
}

func (q *kongSerializedResetQuota) CachePostResetSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error {
	if q.unlock != nil {
		defer func() { q.unlock(); q.unlock = nil }()
	}
	return q.inner.CachePostResetSnapshot(ctx, accountID, usage)
}

// kongRefreshQuota 是余额刷新要用的额度服务（OpenAIQuotaService 满足）。
type kongRefreshQuota interface {
	QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
	CacheCreditsSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error
	CacheResetCreditsSnapshot(ctx context.Context, accountID int64, credits *OpenAIRateLimitResetCredits) error
}

// kongRefreshAccounts 是余额刷新读账号用的仓库（AccountRepository 满足）。
type kongRefreshAccounts interface {
	// ListAllWithFilters 不限状态列账号：停用或出错的账号也要结算离层（三类的其余条件在 kongRefreshSelect 里）。
	ListAllWithFilters(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error)
}

// KongBalanceRefreshView 是读回里一个账号的余额刷新状态。
type KongBalanceRefreshView struct {
	AccountID   int64      `json:"account_id"`
	Class       string     `json:"class"`
	LastAttempt *time.Time `json:"last_attempt"`
	LastOK      *time.Time `json:"last_ok"`
	LastError   string     `json:"last_error,omitempty"`
	Failures    int        `json:"failures"`
}

type kongRefreshStatus struct {
	mu sync.Mutex
	by map[int64]*KongBalanceRefreshView
}

var kongRefreshStatusAll = &kongRefreshStatus{by: map[int64]*KongBalanceRefreshView{}}

func kongBalanceRefreshStatus() []KongBalanceRefreshView {
	s := kongRefreshStatusAll
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]KongBalanceRefreshView, 0, len(s.by))
	for _, v := range s.by {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out
}

func (s *kongRefreshStatus) lastAttempt(id int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.by[id]; v != nil && v.LastAttempt != nil {
		return *v.LastAttempt
	}
	return time.Time{}
}

func (s *kongRefreshStatus) record(id int64, class string, at time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.by[id]
	if v == nil {
		v = &KongBalanceRefreshView{AccountID: id}
		s.by[id] = v
	}
	a := at
	v.Class, v.LastAttempt = class, &a
	if err != nil {
		v.LastError, v.Failures = err.Error(), v.Failures+1
		return
	}
	v.LastOK, v.LastError, v.Failures = &a, "", 0
}

// KongPlanBalanceRefresher 是余额刷新协程。
type KongPlanBalanceRefresher struct {
	quota    kongRefreshQuota
	accounts kongRefreshAccounts
	now      func() time.Time
}

// NewKongPlanBalanceRefresher 创建余额刷新（fork 专有，在 wire_gen.go 手工装配，Start 在计划运行时装好之后）。
func NewKongPlanBalanceRefresher(quota kongRefreshQuota, accounts kongRefreshAccounts) *KongPlanBalanceRefresher {
	return &KongPlanBalanceRefresher{quota: quota, accounts: accounts, now: time.Now}
}

// Start 启动刷新协程：每分钟看一次当前发布，有 balance_refresh 才查。
func (r *KongPlanBalanceRefresher) Start() {
	if r == nil || r.quota == nil || r.accounts == nil {
		return
	}
	go func() {
		t := time.NewTicker(kongRefreshTick)
		defer t.Stop()
		for range t.C {
			r.tick()
		}
	}()
}

// kongRefreshDue 是一个该查的账号。
type kongRefreshDue struct {
	ID    int64
	Class string
}

func kongRefreshClassRank(c string) int {
	switch c {
	case KongRefreshClassSettle:
		return 0
	case KongRefreshClassActive:
		return 1
	}
	return 2
}

// errKongRefreshWithdrawn：当前发布已不带 balance_refresh（换了发布或清空），本轮剩下的不再查，不算失败。
var errKongRefreshWithdrawn = errors.New("余额刷新已撤销")

// kongRefreshConfigNow 是当前发布里的余额刷新参数；没有时为 nil。
func kongRefreshConfigNow(rt *kongPlanRuntime) *KongBalanceRefresh {
	if rt == nil || rt.store == nil {
		return nil
	}
	st := rt.store.current()
	if st == nil || st.dispatch == nil || st.dispatch.GatewayRules == nil {
		return nil
	}
	return st.dispatch.GatewayRules.BalanceRefresh
}

func (r *KongPlanBalanceRefresher) tick() {
	rt := kongPlanRuntimeNow()
	b := kongRefreshConfigNow(rt)
	if b == nil {
		return
	}
	st := rt.store.current()
	cfg := *b
	ctx, cancel := context.WithTimeout(context.Background(), kongRefreshTimeout)
	defer cancel()
	accounts, err := r.accounts.ListAllWithFilters(ctx, PlatformOpenAI, "", "", "", 0, "")
	if err != nil {
		slog.Warn("kong plan: 余额刷新读账号失败，下一分钟重试", "error", err)
		return
	}
	now := r.now()
	due := kongRefreshSelect(rt, st, cfg, accounts, now)
	// 到期的按顺序交给最多 Concurrency 个协程连续消费：同时查询的不超过它，一分钟内到期的都查；本轮期限到了不再领新的。
	queue := make(chan kongRefreshDue, len(due))
	for _, d := range due {
		queue <- d
	}
	close(queue)
	var wg sync.WaitGroup
	for i := 0; i < min(cfg.Concurrency, len(due)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range queue {
				if ctx.Err() != nil || kongRefreshConfigNow(rt) == nil {
					return
				}
				err := r.refresh(ctx, d.ID)
				if errors.Is(err, errKongRefreshWithdrawn) {
					return
				}
				kongRefreshStatusAll.record(d.ID, d.Class, r.now(), err)
				if err != nil {
					slog.Warn("kong plan: 余额刷新失败", "account_id", d.ID, "class", d.Class, "error", err)
				}
			}
		}()
	}
	wg.Wait()
}

// refresh 查一次额度并写缓存，与管理接口的刷新相同：余额与用量一起写，卡快照另写（明细不全时保留旧的，不算失败）。
// 等到账号锁之后再看一次发布：等锁期间撤销了就不查。
func (r *KongPlanBalanceRefresher) refresh(ctx context.Context, id int64) error {
	unlock, ok := kongQuotaLock(ctx, id)
	if !ok {
		return ctx.Err()
	}
	defer unlock()
	if kongRefreshConfigNow(kongPlanRuntimeNow()) == nil {
		return errKongRefreshWithdrawn
	}
	usage, err := r.quota.QueryUsage(ctx, id)
	if err != nil {
		return err
	}
	if err := r.quota.CacheCreditsSnapshot(ctx, id, usage); err != nil {
		return err
	}
	if err := r.quota.CacheResetCreditsSnapshot(ctx, id, usage.RateLimitResetCredits); err != nil {
		slog.Info("kong plan: 余额刷新时卡快照未写入，保留旧的", "account_id", id, "error", err)
	}
	return nil
}

// kongRefreshSelect 选出这一分钟该查的账号，离层结算在先、在用其次、待用最后；同类按上次观测从早到晚。
func kongRefreshSelect(rt *kongPlanRuntime, st *kongPlanState, cfg KongBalanceRefresh, accounts []Account, now time.Time) []kongRefreshDue {
	creditsOn := st.control.CreditsMode == KongPlanCreditsModeOn || st.control.CreditsMode == KongPlanCreditsModeShadow
	allowed := func(id int64) bool {
		i := sort.Search(len(st.control.Allow), func(i int) bool { return st.control.Allow[i] >= id })
		return i < len(st.control.Allow) && st.control.Allow[i] == id
	}
	type cand struct {
		kongRefreshDue
		last time.Time
	}
	var out []cand
	active := time.Duration(cfg.ActiveMin) * time.Minute
	ready := time.Duration(cfg.ReadyMin) * time.Minute
	for i := range accounts {
		a := &accounts[i]
		if !a.IsOpenAIOAuth() || a.IsShadow() {
			continue
		}
		ps, _ := rt.paceState(a.ID)
		var committed *kongPlanBalancePoint
		if ps != nil {
			committed = ps.Balance
		}
		var lastObs time.Time
		if b := rt.ledger.LatestBalance(a.ID, committed); b != nil {
			lastObs = b.At
		}
		last := lastObs
		if t := kongRefreshStatusAll.lastAttempt(a.ID); t.After(last) {
			last = t
		}
		var inCredits, unsettled bool
		var leftAt time.Time
		for _, e := range rt.ledger.Entries(a.ID) {
			switch {
			case e.Settled:
			case e.LeftAt == nil:
				inCredits = true
			default:
				unsettled = true
				if e.LeftAt.After(leftAt) {
					leftAt = *e.LeftAt
				}
			}
		}
		if unsettled && !lastObs.After(leftAt) && now.Sub(last) >= active {
			out = append(out, cand{kongRefreshDue{a.ID, KongRefreshClassSettle}, lastObs})
			continue
		}
		if !creditsOn || !allowed(a.ID) {
			continue
		}
		dropping := ps != nil && !ps.BalanceDropAt.IsZero()
		traffic := a.LastUsedAt != nil && now.Sub(*a.LastUsedAt) < time.Duration(cfg.RecentTrafficMin)*time.Minute
		if (inCredits || dropping) && traffic {
			if now.Sub(last) >= active {
				out = append(out, cand{kongRefreshDue{a.ID, KongRefreshClassActive}, lastObs})
			}
			continue
		}
		if !a.IsActive() || !a.Schedulable {
			continue
		}
		near := inCredits
		if !near {
			if u, ok := resolveOpenAIQuotaUtilization(a.Extra, "7d", now); ok {
				near = u*100 >= rt.windowThresholdPercent(context.Background(), a, "7d", now)-cfg.NearPP
			}
		}
		if near && (last.IsZero() || now.Sub(last) >= ready) {
			out = append(out, cand{kongRefreshDue{a.ID, KongRefreshClassReady}, lastObs})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := kongRefreshClassRank(out[i].Class), kongRefreshClassRank(out[j].Class); ri != rj {
			return ri < rj
		}
		return out[i].last.Before(out[j].last)
	})
	res := make([]kongRefreshDue, len(out))
	for i, c := range out {
		res[i] = c.kongRefreshDue
	}
	return res
}
