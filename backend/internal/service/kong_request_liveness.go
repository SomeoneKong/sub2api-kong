package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// 请求存活续期（设计见 DESIGN-request-liveness.md）：在途的并发占用与 OpenAI 会话登记由进程内一个续期器
// 统一续期，长请求进行多久，它们就保持多久。
//
// 两类续期都只刷新仍存在的成员：并发占用释放之后，迟到的续期不会让它复活；会话只刷新账号上已有的登记，
// 选号先抢槽、再确认会话名额，确认被拒的尝试从没登记过，续期与结束刷新都不会把它计入。
// 周期续期与请求结束时的刷新在同一个工作协程里依次执行，释放路径从不等待它们。

// KongSlotKind 是并发占用的类别。
type KongSlotKind uint8

const (
	KongSlotAccount KongSlotKind = iota + 1
	KongSlotUser
	KongSlotAPIKey
)

func (k KongSlotKind) String() string {
	switch k {
	case KongSlotAccount:
		return "account"
	case KongSlotUser:
		return "user"
	case KongSlotAPIKey:
		return "api_key"
	default:
		return "none"
	}
}

// KongSlotRefresher 由并发缓存实现：成员还在才把分数刷新为当前时刻并刷新键的过期时间，返回成员是否还在。
type KongSlotRefresher interface {
	KongRefreshSlot(ctx context.Context, kind KongSlotKind, id int64, requestID string) (bool, error)
}

// kongLivenessSessions 是会话部分的依赖：读账号当前的空闲超时，只刷新账号上已有的会话登记。
type kongLivenessSessions interface {
	kongLivenessIdleTimeout(ctx context.Context, accountID int64) (time.Duration, error)
	kongLivenessRefreshSession(ctx context.Context, accountID int64, hash string, idle time.Duration) error
}

const (
	// kongLivenessInterval 是续期间隔。并发槽 TTL（缺省 30 分钟）与会话空闲超时都远大于它。
	kongLivenessInterval = 30 * time.Second
	// kongLivenessCallTimeout 是每次 Redis 调用与配置读取的上下文超时，另受 Redis 客户端读写超时约束。
	kongLivenessCallTimeout = 3 * time.Second
	// kongLivenessLongHeld 是长时登记告警的阈值，远超真实请求时长。触发了基本就是某条路径漏了释放。
	kongLivenessLongHeld = 2 * time.Hour
	// kongLivenessFinalQueueSize 是结束刷新队列的容量，满了丢弃并计数，不阻塞释放。
	kongLivenessFinalQueueSize = 1024
	// kongLivenessStatsInterval 是周期计数日志的间隔。
	kongLivenessStatsInterval = 10 * time.Minute
)

// kongLivenessEntry 是一次成功获取得到的登记项。kind 为 0 表示没有并发部分（不限并发的账号槽），
// accountID 为 0 表示没有会话部分。
type kongLivenessEntry struct {
	kind       KongSlotKind
	slotID     int64
	requestID  string
	accountID  int64
	hash       string
	acquiredAt time.Time
	warned     bool
}

type kongLivenessSessionRef struct {
	accountID int64
	hash      string
}

type kongLivenessStats struct {
	slotRefreshed   int64 // 并发占用刷新成功
	slotMissing     int64 // 并发占用的成员已不在（已释放或已过期）
	slotErrors      int64 // 并发占用刷新出错
	sessionRefresh  int64 // 会话刷新调用（成员不存在时什么也不做，同样计在这里）
	sessionErrors   int64 // 会话刷新出错
	idleErrors      int64 // 读账号空闲超时失败
	sessionSkipped  int64 // 从没读到过空闲超时而跳过的会话刷新
	finalRefreshes  int64 // 结束刷新
	finalDropped    int64 // 结束刷新队列满而丢弃
	longHeldWarning int64 // 长时登记告警
}

// KongRequestLiveness 是进程内的续期器。所有方法对 nil 接收者安全。
type KongRequestLiveness struct {
	slots    KongSlotRefresher
	sessions kongLivenessSessions
	now      func() time.Time
	interval time.Duration

	mu      sync.Mutex
	nextID  uint64
	entries map[uint64]*kongLivenessEntry
	idle    map[int64]time.Duration // 账号最近一次读到的空闲超时；只在工作协程里读写
	stats   kongLivenessStats
	lastLog time.Time

	finals    chan kongLivenessSessionRef
	ctx       context.Context // 生命周期上下文：Stop 时取消，正在进行与剩余的调用随之放弃
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewKongRequestLiveness 创建续期器。cache 不支持刷新时只续会话；gateway 为 nil 时只续并发占用。
func NewKongRequestLiveness(cache ConcurrencyCache, gateway *OpenAIGatewayService) *KongRequestLiveness {
	var slots KongSlotRefresher
	if r, ok := cache.(KongSlotRefresher); ok {
		slots = r
	}
	var sessions kongLivenessSessions
	if gateway != nil {
		sessions = gateway
	}
	return newKongRequestLiveness(slots, sessions, time.Now, kongLivenessInterval)
}

func newKongRequestLiveness(slots KongSlotRefresher, sessions kongLivenessSessions, now func() time.Time, interval time.Duration) *KongRequestLiveness {
	ctx, cancel := context.WithCancel(context.Background())
	return &KongRequestLiveness{
		slots:    slots,
		sessions: sessions,
		now:      now,
		interval: interval,
		entries:  make(map[uint64]*kongLivenessEntry),
		idle:     make(map[int64]time.Duration),
		lastLog:  now(),
		finals:   make(chan kongLivenessSessionRef, kongLivenessFinalQueueSize),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
}

// Start 启动工作协程。只生效一次。
func (l *KongRequestLiveness) Start() {
	if l == nil {
		return
	}
	l.startOnce.Do(func() { go l.loop() })
}

// Stop 停止工作协程并等待它退出：正在进行的调用被取消，本批剩余的续期与队列里尚未处理的结束刷新随之放弃，
// 那些会话按空闲超时出计数。
func (l *KongRequestLiveness) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() {
		l.cancel()
		started := true
		l.startOnce.Do(func() { started = false })
		if started {
			<-l.done
		}
	})
}

// track 登记一次成功获取，返回注销函数（幂等）。kind 为 0 且没有会话时不登记，返回 nil。
func (l *KongRequestLiveness) track(kind KongSlotKind, slotID int64, requestID string, accountID int64, hash string) func() {
	if l == nil {
		return nil
	}
	if kind != 0 && requestID == "" {
		kind = 0
	}
	if hash == "" {
		accountID = 0
	}
	if kind == 0 && accountID == 0 {
		return nil
	}
	entry := &kongLivenessEntry{
		kind:       kind,
		slotID:     slotID,
		requestID:  requestID,
		accountID:  accountID,
		hash:       hash,
		acquiredAt: l.now(),
	}
	l.mu.Lock()
	l.nextID++
	id := l.nextID
	l.entries[id] = entry
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { l.untrack(id, entry) })
	}
}

func (l *KongRequestLiveness) untrack(id uint64, entry *kongLivenessEntry) {
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
	if entry.accountID == 0 {
		return
	}
	select {
	case l.finals <- kongLivenessSessionRef{accountID: entry.accountID, hash: entry.hash}:
	default:
		l.mu.Lock()
		l.stats.finalDropped++
		l.mu.Unlock()
	}
}

func (l *KongRequestLiveness) loop() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case ref := <-l.finals:
			if l.stopped() {
				return
			}
			l.refreshFinal(ref)
		case <-ticker.C:
			if l.stopped() {
				return
			}
			l.cycle()
		}
	}
}

func (l *KongRequestLiveness) stopped() bool {
	return l.ctx.Err() != nil
}

// cycle 是一次周期续期：锁内复制登记项，锁外逐项调用。
func (l *KongRequestLiveness) cycle() {
	now := l.now()
	var (
		slots    []kongLivenessEntry
		sessions = make(map[int64]map[string]struct{})
		warns    []kongLivenessEntry
	)
	l.mu.Lock()
	for _, e := range l.entries {
		if e.kind != 0 {
			slots = append(slots, *e)
		}
		if e.accountID != 0 {
			if sessions[e.accountID] == nil {
				sessions[e.accountID] = make(map[string]struct{})
			}
			sessions[e.accountID][e.hash] = struct{}{}
		}
		if !e.warned && now.Sub(e.acquiredAt) >= kongLivenessLongHeld {
			e.warned = true
			warns = append(warns, *e)
		}
	}
	l.stats.longHeldWarning += int64(len(warns))
	l.mu.Unlock()

	for _, e := range warns {
		slog.Error("kong liveness: 登记项已存在超过告警阈值，可能有路径漏了释放",
			"kind", e.kind.String(), "id", e.slotID, "account_id", e.accountID, "acquired_at", e.acquiredAt)
	}
	for _, e := range slots {
		if l.stopped() {
			return
		}
		l.refreshSlot(e)
	}
	for accountID, hashes := range sessions {
		if l.stopped() {
			return
		}
		idle, ok := l.idleTimeout(accountID)
		if !ok {
			l.record(func(st *kongLivenessStats) { st.sessionSkipped += int64(len(hashes)) })
			continue
		}
		for hash := range hashes {
			if l.stopped() {
				return
			}
			l.refreshSession(accountID, hash, idle)
		}
	}
	l.logStats()
}

func (l *KongRequestLiveness) refreshSlot(e kongLivenessEntry) {
	if l.slots == nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, kongLivenessCallTimeout)
	present, err := l.slots.KongRefreshSlot(ctx, e.kind, e.slotID, e.requestID)
	cancel()
	l.record(func(st *kongLivenessStats) {
		switch {
		case err != nil:
			st.slotErrors++
		case present:
			st.slotRefreshed++
		default:
			st.slotMissing++
		}
	})
}

// refreshFinal 处理一次结束刷新；处理完才计数。
func (l *KongRequestLiveness) refreshFinal(ref kongLivenessSessionRef) {
	defer l.record(func(st *kongLivenessStats) { st.finalRefreshes++ })
	idle, ok := l.idleTimeout(ref.accountID)
	if !ok {
		l.record(func(st *kongLivenessStats) { st.sessionSkipped++ })
		return
	}
	l.refreshSession(ref.accountID, ref.hash, idle)
}

func (l *KongRequestLiveness) refreshSession(accountID int64, hash string, idle time.Duration) {
	if l.sessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, kongLivenessCallTimeout)
	err := l.sessions.kongLivenessRefreshSession(ctx, accountID, hash, idle)
	cancel()
	l.record(func(st *kongLivenessStats) {
		st.sessionRefresh++
		if err != nil {
			st.sessionErrors++
		}
	})
}

// idleTimeout 读账号当前的空闲超时；读不到时沿用最近一次读到的值，从没读到过则返回 false。
func (l *KongRequestLiveness) idleTimeout(accountID int64) (time.Duration, bool) {
	if l.sessions == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(l.ctx, kongLivenessCallTimeout)
	idle, err := l.sessions.kongLivenessIdleTimeout(ctx, accountID)
	cancel()
	if err == nil && idle > 0 {
		l.idle[accountID] = idle
		return idle, true
	}
	l.record(func(st *kongLivenessStats) { st.idleErrors++ })
	last, ok := l.idle[accountID]
	return last, ok
}

func (l *KongRequestLiveness) record(update func(*kongLivenessStats)) {
	l.mu.Lock()
	update(&l.stats)
	l.mu.Unlock()
}

// logStats 按间隔输出一行周期计数，并附上此刻的在途登记项数。
func (l *KongRequestLiveness) logStats() {
	now := l.now()
	l.mu.Lock()
	if now.Sub(l.lastLog) < kongLivenessStatsInterval {
		l.mu.Unlock()
		return
	}
	st := l.stats
	l.stats = kongLivenessStats{}
	l.lastLog = now
	entries, withSession := len(l.entries), 0
	for _, e := range l.entries {
		if e.accountID != 0 {
			withSession++
		}
	}
	l.mu.Unlock()
	slog.Info("kong liveness: 周期计数",
		"entries", entries, "with_session", withSession,
		"slot_refreshed", st.slotRefreshed, "slot_missing", st.slotMissing, "slot_errors", st.slotErrors,
		"session_refresh", st.sessionRefresh, "session_errors", st.sessionErrors,
		"idle_errors", st.idleErrors, "session_skipped", st.sessionSkipped,
		"final_refreshes", st.finalRefreshes, "final_dropped", st.finalDropped,
		"long_held", st.longHeldWarning)
}

// SetKongRequestLiveness 注入续期器（可选依赖）。装配后调用一次；传 nil 等于不启用。
func (s *ConcurrencyService) SetKongRequestLiveness(l *KongRequestLiveness) {
	if s != nil {
		s.kongLiveness = l
	}
}

// kongTrack 把一次成功获取登记到续期器，返回包装后的释放函数：先注销登记项，再做原有的释放。
// 账号槽另从上下文取会话计数身份；requestID 为空表示不限并发、没有写 Redis，此时只登记会话部分。
func (s *ConcurrencyService) kongTrack(ctx context.Context, kind KongSlotKind, id int64, requestID string, release func()) func() {
	if s == nil || s.kongLiveness == nil {
		return release
	}
	var (
		accountID int64
		hash      string
	)
	if kind == KongSlotAccount && ctx != nil {
		if hash = kongSessionCountHash(ctx, ""); hash != "" {
			accountID = id
		}
	}
	untrack := s.kongLiveness.track(kind, id, requestID, accountID, hash)
	if untrack == nil {
		return release
	}
	return func() {
		untrack()
		if release != nil {
			release()
		}
	}
}

// SetKongRequestLiveness 让网关服务持有续期器，以便进程退出时停止它。
func (s *OpenAIGatewayService) SetKongRequestLiveness(l *KongRequestLiveness) {
	if s != nil {
		s.kongLiveness = l
	}
}

// StopKongRequestLiveness 在进程退出时停止续期器。
func (s *OpenAIGatewayService) StopKongRequestLiveness() {
	if s != nil && s.kongLiveness != nil {
		s.kongLiveness.Stop()
	}
}

var errKongLivenessNoAccount = errors.New("account not found")

// kongLivenessIdleTimeout 读账号当前的空闲超时（调度快照优先）。
func (s *OpenAIGatewayService) kongLivenessIdleTimeout(ctx context.Context, accountID int64) (time.Duration, error) {
	var (
		account *Account
		err     error
	)
	switch {
	case s.schedulerSnapshot != nil:
		account, err = s.schedulerSnapshot.GetAccount(ctx, accountID)
	case s.accountRepo != nil:
		account, err = s.accountRepo.GetByID(ctx, accountID)
	default:
		return 0, errKongLivenessNoAccount
	}
	if err != nil {
		return 0, err
	}
	if account == nil {
		return 0, errKongLivenessNoAccount
	}
	return kongSessionIdleTimeout(account), nil
}

// kongLivenessRefreshSession 只刷新账号上已有的会话登记，不加入、不清理别的成员。
func (s *OpenAIGatewayService) kongLivenessRefreshSession(ctx context.Context, accountID int64, hash string, idle time.Duration) error {
	if s.kongSessionLimit == nil || s.kongSessionLimit.cache == nil {
		return nil
	}
	return s.kongSessionLimit.cache.RefreshSession(ctx, accountID, hash, idle)
}
