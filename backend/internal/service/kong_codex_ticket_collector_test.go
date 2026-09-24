//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type kongFakeTicketStore struct {
	mu        sync.Mutex
	inserted  []KongObservedTicket
	expiresAt []time.Time
	seen      map[string]bool
	insertErr error

	deleteCalls   int
	deleteCutoffs []time.Time
	deleteResults []int64 // 按调用序号给出删掉的行数，用完之后返回 0
	deleteErr     error
}

func (s *kongFakeTicketStore) InsertObservedTicket(_ context.Context, t KongObservedTicket, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return false, s.insertErr
	}
	key := t.Model + "\x00" + t.State
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[key] {
		return false, nil
	}
	s.seen[key] = true
	s.inserted = append(s.inserted, t)
	s.expiresAt = append(s.expiresAt, expiresAt)
	return true, nil
}

func (s *kongFakeTicketStore) DeleteTicketsCapturedBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	s.deleteCutoffs = append(s.deleteCutoffs, cutoff)
	if s.deleteErr != nil {
		return 0, s.deleteErr
	}
	if i := s.deleteCalls - 1; i < len(s.deleteResults) {
		return s.deleteResults[i], nil
	}
	return 0, nil
}

func (s *kongFakeTicketStore) insertedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inserted)
}

func TestKongTicketCollectorWrites(t *testing.T) {
	store := &kongFakeTicketStore{}
	c := NewKongTicketCollector(store)
	c.Start()
	c.Start() // 只有第一次生效
	captured := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	c.Enqueue(KongObservedTicket{AccountID: 1, Model: "m", State: "a", CapturedAt: captured})
	c.Enqueue(KongObservedTicket{AccountID: 1, Model: "m", State: "a", CapturedAt: captured.Add(time.Minute)})
	c.Enqueue(KongObservedTicket{AccountID: 1, Model: "m", State: "b", CapturedAt: captured})
	c.Enqueue(KongObservedTicket{AccountID: 1, Model: "m", State: ""}) // 空票不入队

	deadline := time.Now().Add(5 * time.Second)
	for c.stats.inserted.Load()+c.stats.duplicates.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("写入协程没有按时处理完队列")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Stop()
	c.Stop() // 可以重复调用

	if store.insertedCount() != 2 || c.stats.inserted.Load() != 2 || c.stats.duplicates.Load() != 1 {
		t.Fatalf("写入 %d 张，计数 inserted=%d duplicates=%d", store.insertedCount(), c.stats.inserted.Load(), c.stats.duplicates.Load())
	}
	// expires_at 按首次收到的时刻估算，与写库的时刻无关。
	if !store.expiresAt[0].Equal(captured.Add(kongTicketTTLEstimate)) {
		t.Fatalf("expires_at = %v", store.expiresAt[0])
	}
	// 启动时先清一次。
	if store.deleteCalls == 0 {
		t.Fatal("启动时应先按保留期清理一次")
	}
}

func TestKongTicketCollectorWriteFailureCounted(t *testing.T) {
	c := NewKongTicketCollector(&kongFakeTicketStore{insertErr: errors.New("db down")})
	c.write(KongObservedTicket{AccountID: 1, State: "a"})
	c.write(KongObservedTicket{AccountID: 1, State: "b"})
	if c.stats.failed.Load() != 2 || c.stats.inserted.Load() != 0 {
		t.Fatalf("failed=%d inserted=%d", c.stats.failed.Load(), c.stats.inserted.Load())
	}
}

// 队列满时丢弃并计数，不阻塞请求路径。
func TestKongTicketCollectorDropsWhenFull(t *testing.T) {
	c := NewKongTicketCollector(&kongFakeTicketStore{}) // 不启动：队列只进不出
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < kongTicketQueueCap+5; i++ {
			c.Enqueue(KongObservedTicket{AccountID: 1, State: "s"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("队列满时 Enqueue 阻塞了")
	}
	if got := c.stats.dropped.Load(); got != 5 {
		t.Fatalf("dropped = %d，期望 5", got)
	}
}

func TestKongTicketCollectorSweep(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	t.Run("删满一批就接着删，不满一批即止", func(t *testing.T) {
		store := &kongFakeTicketStore{deleteResults: []int64{kongTicketSweepBatch, kongTicketSweepBatch, 3}}
		c := NewKongTicketCollector(store)
		c.now = func() time.Time { return now }
		c.sweep()
		if store.deleteCalls != 3 || c.stats.swept.Load() != 2*kongTicketSweepBatch+3 {
			t.Fatalf("调用 %d 次、清理 %d 行", store.deleteCalls, c.stats.swept.Load())
		}
		if !store.deleteCutoffs[0].Equal(now.Add(-kongTicketRetention)) {
			t.Fatalf("cutoff = %v", store.deleteCutoffs[0])
		}
	})
	t.Run("一轮最多删若干批", func(t *testing.T) {
		results := make([]int64, kongTicketSweepMaxBatches+10)
		for i := range results {
			results[i] = kongTicketSweepBatch
		}
		store := &kongFakeTicketStore{deleteResults: results}
		c := NewKongTicketCollector(store)
		c.sweep()
		if store.deleteCalls != kongTicketSweepMaxBatches {
			t.Fatalf("调用 %d 次，期望 %d", store.deleteCalls, kongTicketSweepMaxBatches)
		}
	})
	t.Run("失败即止，留给下一轮", func(t *testing.T) {
		store := &kongFakeTicketStore{deleteErr: errors.New("db down")}
		c := NewKongTicketCollector(store)
		c.sweep()
		if store.deleteCalls != 1 {
			t.Fatalf("调用 %d 次", store.deleteCalls)
		}
	})
	t.Run("已在停止时不再删", func(t *testing.T) {
		store := &kongFakeTicketStore{deleteResults: []int64{kongTicketSweepBatch}}
		c := NewKongTicketCollector(store)
		c.Stop()
		c.sweep()
		if store.deleteCalls != 0 {
			t.Fatalf("调用 %d 次", store.deleteCalls)
		}
	})
}

// 停止时队列里剩下的票与停止之后才到的票都计入丢弃。
func TestKongTicketCollectorStopCountsDropped(t *testing.T) {
	c := NewKongTicketCollector(&kongFakeTicketStore{}) // 不启动：入队的票都留在队列里
	c.Enqueue(KongObservedTicket{AccountID: 1, State: "a"})
	c.Enqueue(KongObservedTicket{AccountID: 1, State: "b"})
	c.Stop()
	c.Enqueue(KongObservedTicket{AccountID: 1, State: "c"})
	c.Stop()
	if got := c.stats.dropped.Load(); got != 3 {
		t.Fatalf("dropped = %d，期望 3", got)
	}
}

func TestKongTicketCollectorNilSafe(t *testing.T) {
	var c *KongTicketCollector
	c.Enqueue(KongObservedTicket{State: "s"})
	c.Start()
	c.Stop()
	// 没有存储时 Start 不启动协程，Stop 也不会卡住。
	noStore := NewKongTicketCollector(nil)
	noStore.Start()
	noStore.Stop()
}

// 数据库不可用时每张票都会失败：告警按间隔节流。
func TestKongTicketCollectorWarnThrottled(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	c := NewKongTicketCollector(nil)
	c.now = func() time.Time { return now }
	c.warn("first")
	first := c.lastWarn
	now = now.Add(kongTicketWarnInterval / 2)
	c.warn("throttled")
	if !c.lastWarn.Equal(first) {
		t.Fatal("间隔内的告警应被节流")
	}
	now = now.Add(kongTicketWarnInterval)
	c.warn("again")
	if !c.lastWarn.Equal(now) {
		t.Fatal("过了间隔应再打一次")
	}
}
