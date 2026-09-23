package admin

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type kongFakeSessionCountCache struct {
	service.SessionLimitCache
	counts   map[int64]int
	timeouts map[int64]time.Duration
}

func (c *kongFakeSessionCountCache) GetActiveSessionCountBatch(_ context.Context, ids []int64, timeouts map[int64]time.Duration) (map[int64]int, error) {
	c.timeouts = timeouts
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		out[id] = c.counts[id]
	}
	return out, nil
}

func kongSessionTestAccount(id int64, platform, accountType string, maxSessions int) service.Account {
	a := service.Account{ID: id, Platform: platform, Type: accountType}
	if maxSessions > 0 {
		a.Extra = map[string]any{"max_sessions": float64(maxSessions), "session_idle_timeout_minutes": float64(15)}
	}
	return a
}

func TestKongAppendOpenAISessionLimitAccounts(t *testing.T) {
	accounts := []service.Account{
		kongSessionTestAccount(1, service.PlatformOpenAI, service.AccountTypeOAuth, 2),
		kongSessionTestAccount(2, service.PlatformOpenAI, service.AccountTypeOAuth, 0),
		kongSessionTestAccount(3, service.PlatformOpenAI, service.AccountTypeAPIKey, 2),
		kongSessionTestAccount(4, service.PlatformAnthropic, service.AccountTypeOAuth, 2),
	}
	timeouts := map[int64]time.Duration{}
	ids := kongAppendOpenAISessionLimitAccounts(accounts, []int64{99}, timeouts)
	if len(ids) != 2 || ids[0] != 99 || ids[1] != 1 {
		t.Fatalf("只追加设了上限的 OpenAI OAuth 账号：%v", ids)
	}
	if timeouts[1] != 15*time.Minute {
		t.Fatalf("空闲超时取账号自己的值：%v", timeouts)
	}
}

func TestKongAccountDetailShowsOpenAIActiveSessions(t *testing.T) {
	cache := &kongFakeSessionCountCache{counts: map[int64]int{1: 2}}
	h := &AccountHandler{sessionLimitCache: cache}
	limited := kongSessionTestAccount(1, service.PlatformOpenAI, service.AccountTypeOAuth, 3)
	item := h.buildAccountResponseWithRuntime(context.Background(), &limited)
	if item.ActiveSessions == nil || *item.ActiveSessions != 2 || cache.timeouts[1] != 15*time.Minute {
		t.Fatalf("详情要带 OpenAI OAuth 账号的活跃会话数：%v timeouts=%v", item.ActiveSessions, cache.timeouts)
	}
	unlimited := kongSessionTestAccount(2, service.PlatformOpenAI, service.AccountTypeOAuth, 0)
	if item := h.buildAccountResponseWithRuntime(context.Background(), &unlimited); item.ActiveSessions != nil {
		t.Fatal("未设上限的账号不查")
	}
}
