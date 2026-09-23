package service

import (
	"context"
	"errors"
	"time"
)

// KongPlanLiveView 是 GET /api/v1/admin/kong-plan/accounts 一项的实时占用（DESIGN-quota-gateway-reads.md 第 3 节）：
// 并发是常规槽位加实时租约，与选号读的同一份负载；会话数只对会话上限生效（max_sessions > 0）的账号读，不限的账号
// 网关不登记会话，为 nil。
type KongPlanLiveView struct {
	Concurrency int  `json:"concurrency"`
	Waiting     int  `json:"waiting"`
	Sessions    *int `json:"sessions"`
}

// KongPlanLiveReader 读实时占用：负载走并发服务，活跃会话走会话上限组件的缓存，都与网关自己用的是同一份读数。
type KongPlanLiveReader interface {
	KongPlanLoads(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
	KongPlanSessions(ctx context.Context, ids []int64, idleTimeouts map[int64]time.Duration) (map[int64]int, error)
}

var errKongPlanLiveUnavailable = errors.New("live reader unavailable")

// KongPlanLoads 实现 KongPlanLiveReader。
func (s *OpenAIGatewayService) KongPlanLoads(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if s == nil || s.concurrencyService == nil {
		return nil, errKongPlanLiveUnavailable
	}
	return s.concurrencyService.GetAccountsLoadBatch(ctx, accounts)
}

// KongPlanSessions 实现 KongPlanLiveReader。
func (s *OpenAIGatewayService) KongPlanSessions(ctx context.Context, ids []int64, idleTimeouts map[int64]time.Duration) (map[int64]int, error) {
	if s == nil || s.kongSessionLimit == nil || s.kongSessionLimit.cache == nil {
		return nil, errKongPlanLiveUnavailable
	}
	return s.kongSessionLimit.cache.GetActiveSessionCountBatch(ctx, ids, idleTimeouts)
}

// kongPlanAttachLive 给每一项填上实时占用。全部账号合成一次负载读取、一次会话读取；负载读不到（报错或结果里没有这个账号）
// 时这一项的 Live 为 nil，会话读不到时只有 Sessions 为 nil——会话批量读取会跳过失败的单项、整批失败也可能返回空结果而不报错，
// 所以逐个检查结果里有没有这个账号，没有就是读不到，不当成 0。accounts 按 ID 给出视图里的账号，用来取会话空闲超时。
func kongPlanAttachLive(ctx context.Context, live KongPlanLiveReader, views []KongPlanAccountView, accounts map[int64]*Account) {
	if live == nil || len(views) == 0 {
		return
	}
	loadReq := make([]AccountWithConcurrency, 0, len(views))
	sessionIDs := make([]int64, 0, len(views))
	idle := make(map[int64]time.Duration, len(views))
	for _, v := range views {
		loadReq = append(loadReq, AccountWithConcurrency{ID: v.ID, MaxConcurrency: v.MaxConcurrency})
		if a := accounts[v.ID]; a != nil && v.MaxSessions > 0 {
			sessionIDs = append(sessionIDs, v.ID)
			idle[v.ID] = kongSessionIdleTimeout(a)
		}
	}
	loads, err := live.KongPlanLoads(ctx, loadReq)
	if err != nil {
		return
	}
	var sessions map[int64]int
	sessionsOK := false
	if len(sessionIDs) > 0 {
		sessions, err = live.KongPlanSessions(ctx, sessionIDs, idle)
		sessionsOK = err == nil
	}
	for i := range views {
		l := loads[views[i].ID]
		if l == nil {
			continue
		}
		lv := &KongPlanLiveView{Concurrency: l.CurrentConcurrency, Waiting: l.WaitingCount}
		if _, asked := idle[views[i].ID]; asked && sessionsOK {
			if n, ok := sessions[views[i].ID]; ok {
				lv.Sessions = &n
			}
		}
		views[i].Live = lv
	}
}
