package admin

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// OpenAI OAuth 账号的会话数上限在管理列表与详情里的活跃会话数（设计见 DESIGN-openai-session-limit.md）。
// 上游只为 Anthropic OAuth/SetupToken 账号查询；这里把设了上限的 OpenAI OAuth 账号并进同一次查询，
// 空闲超时取账号自己的值，与选号时的计数口径一致。

// kongAppendOpenAISessionLimitAccounts 把设了会话上限的 OpenAI OAuth 账号加入活跃会话数的批量查询。
func kongAppendOpenAISessionLimitAccounts(accounts []service.Account, ids []int64, idleTimeouts map[int64]time.Duration) []int64 {
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAIOAuth() || acc.GetMaxSessions() <= 0 {
			continue
		}
		ids = append(ids, acc.ID)
		idleTimeouts[acc.ID] = time.Duration(acc.GetSessionIdleTimeoutMinutes()) * time.Minute
	}
	return ids
}

// kongFillOpenAIActiveSessions 为账号详情填上 OpenAI OAuth 账号的活跃会话数。
func (h *AccountHandler) kongFillOpenAIActiveSessions(ctx context.Context, account *service.Account, item *AccountWithConcurrency) {
	if h == nil || h.sessionLimitCache == nil || account == nil || item == nil || !account.IsOpenAIOAuth() || account.GetMaxSessions() <= 0 {
		return
	}
	idleTimeouts := map[int64]time.Duration{account.ID: time.Duration(account.GetSessionIdleTimeoutMinutes()) * time.Minute}
	if sessions, err := h.sessionLimitCache.GetActiveSessionCountBatch(ctx, []int64{account.ID}, idleTimeouts); err == nil {
		if count, ok := sessions[account.ID]; ok {
			item.ActiveSessions = &count
		}
	}
}
