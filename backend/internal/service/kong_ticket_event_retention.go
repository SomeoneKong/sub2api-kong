package service

import (
	"context"
	"log/slog"
	"time"
)

// 事件表的保留清理，口径与 usage_logs 相同：保留期按天配置，至多每 6 小时清一次，按批删到没有
// 超期行为止。
//
// 触发点是事件写入本身而不是定时器：事件只在有流量时产生，没流量时表不增长，也就不需要清理。
// 清理放到独立 goroutine 里跑——写事件的调用方可能正占着 WS 上游 reader，只有几百毫秒的预算。

// kongEventSweepInterval 是两次事件清理之间的最短间隔。
const kongEventSweepInterval = 6 * time.Hour

// kongEventSweepBatch 是事件清理单批删除的行数。
const kongEventSweepBatch = 5000

// kongEventSweepTimeout 是一次事件清理（含全部批次）的总期限。
const kongEventSweepTimeout = 2 * time.Minute

// SetEventRetention 设定事件的保留期，0 表示不清理。须在开始服务前调用。
func (s *KongTicketService) SetEventRetention(d time.Duration) {
	s.eventRetention = d
}

// maybeSweepEvents 在距上次清理满间隔时认领一次清理，并交给后台执行。
//
// 认领时就推进时刻，失败也不提前重试：清理失败只意味着表多留几小时的行，不值得让每次事件写入都
// 再去撞一次同样的故障。
func (s *KongTicketService) maybeSweepEvents(ctx context.Context, now time.Time) {
	if s.eventRetention <= 0 {
		return
	}
	s.mu.Lock()
	if !s.lastEventSweep.IsZero() && now.Sub(s.lastEventSweep) < kongEventSweepInterval {
		s.mu.Unlock()
		return
	}
	s.lastEventSweep = now
	s.mu.Unlock()

	cutoff := now.Add(-s.eventRetention)
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongEventSweepTimeout)
		defer cancel()
		deleted, err := s.sweepEvents(sctx, cutoff)
		if err != nil {
			slog.Error("kong ticket: 事件保留清理失败", "cutoff", cutoff, "deleted", deleted, "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("kong ticket: 事件保留清理完成", "cutoff", cutoff, "deleted", deleted)
		}
	}()
}

// sweepEvents 按批删掉 created_at 早于 cutoff 的事件，直到某一批不满为止，返回删掉的总行数。
func (s *KongTicketService) sweepEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		n, err := s.repo.DeleteEventsBefore(ctx, cutoff, kongEventSweepBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < kongEventSweepBatch {
			return total, nil
		}
	}
}
