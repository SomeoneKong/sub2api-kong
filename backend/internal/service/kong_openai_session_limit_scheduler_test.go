//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 调度入口上的会话上限：续接与 guardian 亲和放行登记；guardian 回退不带哈希进入旧版选号时仍按请求自己的会话计数。

func kongSessionSchedulerAccount(id int64, groupID int64, priority, maxSessions int) Account {
	a := kongSessionOAuth(id, priority, maxSessions)
	a.Concurrency = 1
	a.GroupIDs = []int64{groupID}
	a.Credentials = map[string]any{"plan_type": "team"}
	return a
}

func kongSessionSchedulerService(accounts []Account, bindings map[string]int64, cfg *config.Config) (*OpenAIGatewayService, *kongFakeSessionCache) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true // 会话上限只挂在批量加载开启时的三层上
	acquire := make(map[int64]bool, len(accounts))
	for _, a := range accounts {
		acquire[a.ID] = true
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              &schedulerTestGatewayCache{sessionBindings: bindings},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("false"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: acquire}),
	}
	cache := newKongFakeSessionCache(time.Now)
	svc.SetKongSessionLimitCache(cache)
	return svc, cache
}

func TestKongSessionGuardianParentAffinityPassesThrough(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	parentID := "55555555-5555-4555-8555-555555555555"
	groupID := int64(103001)
	svc, cache := kongSessionSchedulerService([]Account{
		kongSessionSchedulerAccount(39101, groupID, 10, 1),
		kongSessionSchedulerAccount(39102, groupID, 0, 1),
	}, map[string]int64{"openai:" + DeriveSessionHashFromSeed(parentID): 39101}, nil)
	cache.put(39101, "other")

	ctx := guardianAffinityTestContext(t, codexAutoReviewModel, "guardian", parentID, "")
	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child-session", codexAutoReviewModel, nil, OpenAIUpstreamTransportAny, false)
	if err != nil || selection == nil || selection.Account.ID != 39101 || decision.Layer != openAIAccountScheduleLayerGuardianParent {
		t.Fatalf("应跟随父会话的账号：selection=%+v decision=%+v err=%v", selection, decision, err)
	}
	if !cache.has(39101, "child-session") {
		t.Fatalf("guardian 亲和放行登记，允许超过上限：%v", cache.sessions)
	}
}

func TestKongSessionGuardianFallbackCountsOwnSession(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	parentID := "66666666-6666-4666-8666-666666666666"
	parentHash := DeriveSessionHashFromSeed(parentID)
	groupID, otherGroupID := int64(103011), int64(103012)
	svc, cache := kongSessionSchedulerService([]Account{
		kongSessionSchedulerAccount(39111, otherGroupID, 0, 0),
		kongSessionSchedulerAccount(39112, groupID, 0, 1),
		kongSessionSchedulerAccount(39113, groupID, 5, 1),
	}, map[string]int64{"openai:" + parentHash: 39111}, nil)
	cache.put(39112, "other")

	// 请求自己的会话哈希与父会话相同时，为保住父绑定，旧版选号收到的是空哈希。
	ctx := guardianAffinityTestContext(t, codexAutoReviewModel, "guardian", parentID, "")
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", parentHash, codexAutoReviewModel, nil, OpenAIUpstreamTransportAny, false)
	if err != nil || selection == nil {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
	if selection.Account.ID != 39113 || !cache.has(39113, parentHash) {
		t.Fatalf("回退仍按请求自己的会话计数，应避开满额的 39112：got %d sessions=%v", selection.Account.ID, cache.sessions)
	}
}

func TestKongSessionPreviousResponsePassesThrough(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	ctx := context.Background()
	groupID := int64(103021)
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	bound := kongSessionSchedulerAccount(39121, groupID, 5, 1)
	bound.Extra["openai_oauth_responses_websockets_v2_enabled"] = true // OAuth 的续接状态在 WSv2 会话上
	svc, cache := kongSessionSchedulerService([]Account{bound, kongSessionSchedulerAccount(39122, groupID, 0, 1)}, nil, cfg)
	cache.put(39121, "other")
	if err := svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, "resp_kong_prev", 39121, time.Hour); err != nil {
		t.Fatal(err)
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "resp_kong_prev", "s-prev", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	if err != nil || selection == nil || selection.Account.ID != 39121 || decision.Layer != openAIAccountScheduleLayerPreviousResponse {
		t.Fatalf("应按 previous_response_id 续接：selection=%+v decision=%+v err=%v", selection, decision, err)
	}
	if !cache.has(39121, "s-prev") {
		t.Fatalf("续接放行登记，允许超过上限：%v", cache.sessions)
	}
}
