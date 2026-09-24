package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 收票的后台写入：请求路径只把票放进有界队列，写库、去重与按保留期清理都在这里做。设计见
// DESIGN-codex-ticket.md。
//
// 这是观测，不追求零丢失：队列满、写入失败、进程退出时丢掉的票只计数，不重试、不阻塞请求与退出。

const (
	kongTicketQueueCap = 1024
	// kongTicketRetention 是票的保留期，按首次收到的时刻算。
	kongTicketRetention = 30 * 24 * time.Hour
	// kongTicketTTLEstimate 只用来填票表的 expires_at（列非空、为兼容旧格式保留），新代码不据它判断任何事。
	kongTicketTTLEstimate   = time.Hour
	kongTicketWriteTimeout  = 5 * time.Second
	kongTicketSweepInterval = time.Hour
	kongTicketSweepBatch    = 1000
	// kongTicketSweepMaxBatches 是一轮清理最多删几批，剩下的留给下一轮。
	kongTicketSweepMaxBatches = 50
	kongTicketStatsInterval   = 10 * time.Minute
	kongTicketWarnInterval    = time.Minute
)

// KongObservedTicket 是一张被动收到的票。CapturedAt 是请求路径上收到它的时刻，不是写库的时刻。
type KongObservedTicket struct {
	AccountID  int64
	Model      string
	State      string
	CapturedAt time.Time
}

// KongTicketStore 是票表的读写面（repository/kong_codex_ticket_repo.go）。
type KongTicketStore interface {
	// InsertObservedTicket 按 (账号, 模型, 原值) 去重写入；已存在时返回 false、不改动原行。
	InsertObservedTicket(ctx context.Context, t KongObservedTicket, expiresAt time.Time) (bool, error)
	// DeleteTicketsCapturedBefore 删除一批首次收到时刻早于 cutoff 的票，返回删掉的行数。
	DeleteTicketsCapturedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// KongTicketCollector 是收票的后台写入者。
type KongTicketCollector struct {
	store KongTicketStore
	now   func() time.Time
	queue chan KongObservedTicket
	stop  chan struct{}
	wg    sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   atomic.Bool

	stats    kongTicketCollectorStats
	lastWarn time.Time // 只在写入协程里读写
}

type kongTicketCollectorStats struct {
	inserted   atomic.Int64
	duplicates atomic.Int64
	dropped    atomic.Int64 // 队列满时丢弃
	failed     atomic.Int64 // 写入失败
	swept      atomic.Int64 // 超过保留期被删除
}

// NewKongTicketCollector 创建后台写入者。调用 Start 之后才开始写库。
func NewKongTicketCollector(store KongTicketStore) *KongTicketCollector {
	return &KongTicketCollector{
		store: store,
		now:   time.Now,
		queue: make(chan KongObservedTicket, kongTicketQueueCap),
		stop:  make(chan struct{}),
	}
}

// Enqueue 把一张票放进队列，队列满或已停止时丢弃并计数。不阻塞调用方。
func (c *KongTicketCollector) Enqueue(t KongObservedTicket) {
	if c == nil || t.State == "" {
		return
	}
	if c.stopped.Load() {
		c.stats.dropped.Add(1)
		return
	}
	select {
	case c.queue <- t:
	default:
		c.stats.dropped.Add(1)
	}
}

// Start 启动写入协程。只有第一次调用生效。
func (c *KongTicketCollector) Start() {
	if c == nil || c.store == nil {
		return
	}
	c.startOnce.Do(func() {
		c.wg.Add(1)
		go c.run()
	})
}

// Stop 停止写入协程并等它退出。队列里剩下的票不再写库，计入丢弃，最后打一行累计计数。
func (c *KongTicketCollector) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.stopped.Store(true)
		close(c.stop)
		c.wg.Wait()
		c.stats.dropped.Add(int64(len(c.queue)))
		c.logStats(kongTicketCollectorSnapshot{})
	})
}

func (c *KongTicketCollector) run() {
	defer c.wg.Done()
	sweep := time.NewTicker(kongTicketSweepInterval)
	defer sweep.Stop()
	stats := time.NewTicker(kongTicketStatsInterval)
	defer stats.Stop()
	var logged kongTicketCollectorSnapshot
	// 启动时先清一次：进程停过一段时间后，积压的超期票不必再等一个小时。
	c.sweep()
	for {
		select {
		case <-c.stop:
			return
		case t := <-c.queue:
			c.write(t)
		case <-sweep.C:
			c.sweep()
		case <-stats.C:
			logged = c.logStats(logged)
		}
	}
}

func (c *KongTicketCollector) write(t KongObservedTicket) {
	ctx, cancel := context.WithTimeout(context.Background(), kongTicketWriteTimeout)
	defer cancel()
	inserted, err := c.store.InsertObservedTicket(ctx, t, t.CapturedAt.Add(kongTicketTTLEstimate))
	switch {
	case err != nil:
		c.stats.failed.Add(1)
		c.warn("kong ticket: 收票写库失败，这张票丢弃", "account_id", t.AccountID, "model", t.Model, "error", err)
	case inserted:
		c.stats.inserted.Add(1)
	default:
		c.stats.duplicates.Add(1)
	}
}

// sweep 删除超过保留期的票，每轮最多 kongTicketSweepMaxBatches 批；失败或没删完的留给下一轮。
func (c *KongTicketCollector) sweep() {
	cutoff := c.now().Add(-kongTicketRetention)
	for i := 0; i < kongTicketSweepMaxBatches; i++ {
		select {
		case <-c.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), kongTicketWriteTimeout)
		n, err := c.store.DeleteTicketsCapturedBefore(ctx, cutoff, kongTicketSweepBatch)
		cancel()
		if err != nil {
			c.warn("kong ticket: 按保留期清理失败，下一轮再试", "error", err)
			return
		}
		c.stats.swept.Add(n)
		if n < kongTicketSweepBatch {
			return
		}
	}
}

// warn 按间隔节流打 Warn：数据库不可用时每张票都会失败，不能每张打一行。
func (c *KongTicketCollector) warn(msg string, args ...any) {
	now := c.now()
	if !c.lastWarn.IsZero() && now.Sub(c.lastWarn) < kongTicketWarnInterval {
		return
	}
	c.lastWarn = now
	slog.Warn(msg, args...)
}

type kongTicketCollectorSnapshot struct {
	inserted, duplicates, dropped, failed, swept int64
}

// logStats 在这一周期有变化时打一行计数（累计值），供观察收票量与丢失。
func (c *KongTicketCollector) logStats(prev kongTicketCollectorSnapshot) kongTicketCollectorSnapshot {
	cur := kongTicketCollectorSnapshot{
		inserted:   c.stats.inserted.Load(),
		duplicates: c.stats.duplicates.Load(),
		dropped:    c.stats.dropped.Load(),
		failed:     c.stats.failed.Load(),
		swept:      c.stats.swept.Load(),
	}
	if cur != prev {
		slog.Info("kong ticket: 收票计数（进程启动以来累计）",
			"inserted", cur.inserted, "duplicates", cur.duplicates,
			"dropped", cur.dropped, "failed", cur.failed, "swept", cur.swept)
	}
	return cur
}
