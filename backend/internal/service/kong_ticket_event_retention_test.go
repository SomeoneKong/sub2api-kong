//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func kongOldAndNewEvents(repo *kongStubRepo, now time.Time, old, fresh int) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for i := 0; i < old; i++ {
		repo.events = append(repo.events, &KongTicketEvent{EventType: KongEventObserve, CreatedAt: now.Add(-100 * 24 * time.Hour)})
	}
	for i := 0; i < fresh; i++ {
		repo.events = append(repo.events, &KongTicketEvent{EventType: KongEventObserve, CreatedAt: now.Add(-time.Hour)})
	}
}

func kongEventCount(repo *kongStubRepo) int {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return len(repo.events)
}

// 超期行要删干净：一批删满就接着删，而不是每次触发只删一批——那样积压会永远追不上写入。
func TestKongSweepEventsDeletesOnlyExpiredAcrossBatches(t *testing.T) {
	repo := newKongStubRepo()
	svc := kongTestService(t, repo, &kongStubUpstream{}, &kongStubAccounts{accounts: map[int64]*Account{}})
	now := time.Now()
	kongOldAndNewEvents(repo, now, kongEventSweepBatch+7, 3)

	deleted, err := svc.sweepEvents(context.Background(), now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if deleted != int64(kongEventSweepBatch+7) {
		t.Errorf("删掉 %d 行，应为 %d", deleted, kongEventSweepBatch+7)
	}
	if n := kongEventCount(repo); n != 3 {
		t.Errorf("保留期内的事件应当全部留下，实际剩 %d 条", n)
	}
}

func TestKongSweepEventsReportsError(t *testing.T) {
	repo := newKongStubRepo()
	repo.deleteEventsErr = errors.New("库不可用")
	svc := kongTestService(t, repo, &kongStubUpstream{}, &kongStubAccounts{accounts: map[int64]*Account{}})
	if _, err := svc.sweepEvents(context.Background(), time.Now()); err == nil {
		t.Error("删除失败必须报出来")
	}
}

// 清理由事件写入触发，至多每个间隔一次；保留期为 0 时完全不清理。
func TestKongEventSweepIsTriggeredByEventWritesAndThrottled(t *testing.T) {
	t.Run("保留期为 0 不清理", func(t *testing.T) {
		repo := newKongStubRepo()
		svc := kongTestService(t, repo, &kongStubUpstream{}, &kongStubAccounts{accounts: map[int64]*Account{}})
		svc.maybeSweepEvents(context.Background(), time.Now())
		svc.mu.Lock()
		claimed := !svc.lastEventSweep.IsZero()
		svc.mu.Unlock()
		if claimed {
			t.Error("未设保留期时不该认领清理")
		}
	})

	t.Run("写事件时清掉超期行，间隔内不再认领", func(t *testing.T) {
		repo := newKongStubRepo()
		svc := kongTestService(t, repo, &kongStubUpstream{}, &kongStubAccounts{accounts: map[int64]*Account{}})
		svc.SetEventRetention(90 * 24 * time.Hour)
		now := time.Now()
		kongOldAndNewEvents(repo, now, 5, 0)

		svc.logEvent(context.Background(), &KongTicketEvent{EventType: KongEventObserve, CreatedAt: now})
		deadline := time.Now().Add(5 * time.Second)
		for kongEventCount(repo) != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("写事件后超期行没被清掉，还剩 %d 条", kongEventCount(repo))
			}
			time.Sleep(10 * time.Millisecond)
		}

		svc.mu.Lock()
		first := svc.lastEventSweep
		svc.mu.Unlock()
		svc.maybeSweepEvents(context.Background(), first.Add(kongEventSweepInterval-time.Minute))
		svc.mu.Lock()
		unchanged := svc.lastEventSweep.Equal(first)
		svc.mu.Unlock()
		if !unchanged {
			t.Error("间隔未满又认领了一次清理")
		}

		later := first.Add(kongEventSweepInterval)
		svc.maybeSweepEvents(context.Background(), later)
		svc.mu.Lock()
		reclaimed := svc.lastEventSweep.Equal(later)
		svc.mu.Unlock()
		if !reclaimed {
			t.Error("间隔满了应当再认领一次清理")
		}
	})
}
