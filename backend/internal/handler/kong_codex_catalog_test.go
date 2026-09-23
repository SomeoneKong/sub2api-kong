//go:build unit

package handler

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const kongCatalogHandlerBody = `{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-luna"}]}`

func kongCatalogOAuthAccount(id int64, lastUsed *time.Time) service.Account {
	return service.Account{
		ID:          id,
		Name:        "chatgpt-oauth",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		LastUsedAt:  lastUsed,
		Credentials: map[string]any{"access_token": "oauth-test", "plan_type": "pro"},
	}
}

type kongCatalogHandlerFetches struct {
	mu    sync.Mutex
	calls map[int64]int
	err   error
}

func (f *kongCatalogHandlerFetches) fetch(account *service.Account, _ string) (*service.OpenAIModelsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[int64]int{}
	}
	f.calls[account.ID]++
	if f.err != nil {
		return nil, f.err
	}
	return &service.OpenAIModelsResponse{Body: []byte(kongCatalogHandlerBody)}, nil
}

func (f *kongCatalogHandlerFetches) count(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func newKongCatalogHandler(t *testing.T, accounts []service.Account, candidates []service.Account, fetches *kongCatalogHandlerFetches) (*OpenAIGatewayHandler, *codexModelsFailoverHTTPUpstream) {
	t.Helper()
	upstream := &codexModelsFailoverHTTPUpstream{firstBody: `{"models":[{"slug":"gpt-5.6-sol"}]}`}
	gatewayService := service.NewOpenAIGatewayService(
		codexModelsFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	stop := service.KongInstallCodexCatalogForTest(gatewayService, func() []service.Account { return candidates }, fetches.fetch)
	t.Cleanup(stop)
	return &OpenAIGatewayHandler{gatewayService: gatewayService}, upstream
}

func TestKongCodexCatalogHandlerServesConsensusVersion(t *testing.T) {
	used := time.Now().Add(-time.Minute)
	accounts := []service.Account{kongCatalogOAuthAccount(1, &used), kongCatalogOAuthAccount(2, &used)}
	fetches := &kongCatalogHandlerFetches{}
	handler, upstream := newKongCatalogHandler(t, accounts, accounts, fetches)
	group := &service.Group{ID: 61, Platform: service.PlatformOpenAI}

	first := performCodexModelsRequestForGroup(t, handler, group, "")
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
	require.Equal(t, []string{"gpt-6.1-sol", "gpt-6-luna"}, codexHandlerManifestSlugs(t, first))
	etag := first.Header().Get("ETag")
	require.Equal(t, service.CodexModelsManifestETag(first.Body.Bytes()), etag)
	require.Empty(t, upstream.calls(), "对外版本从存档给出，不经原循环")

	second := performCodexModelsRequestForGroup(t, handler, group, etag)
	require.Equal(t, http.StatusNotModified, second.Code)
	require.Equal(t, 1, fetches.count(1))
	require.Equal(t, 1, fetches.count(2))
}

func TestKongCodexCatalogHandlerLeavesAPIKeyGroupsAlone(t *testing.T) {
	account := kongContextWindowAPIKeyAccount()
	handler, upstream := newKongCatalogHandler(t, []service.Account{account}, nil, &kongCatalogHandlerFetches{})
	group := &service.Group{ID: 62, Platform: service.PlatformOpenAI}

	etag := ""
	for i := 0; i < 3; i++ {
		recorder := performCodexModelsRequestForGroup(t, handler, group, etag)
		if i == 0 {
			require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
			etag = recorder.Header().Get("ETag")
			continue
		}
		require.Equal(t, http.StatusNotModified, recorder.Code, "API key 账号不纳管，连续请求不受准入影响")
	}
	require.Len(t, upstream.calls(), 1, "API key 清单照旧走账号级缓存")
}

func TestKongCodexCatalogHandlerExcludesCoolingAccount(t *testing.T) {
	idle := time.Now().Add(-2 * time.Hour)
	accounts := []service.Account{kongCatalogOAuthAccount(1, &idle)}
	fetches := &kongCatalogHandlerFetches{err: errors.New("upstream unavailable")}
	handler, _ := newKongCatalogHandler(t, accounts, accounts, fetches)
	group := &service.Group{ID: 63, Platform: service.PlatformOpenAI}

	first := performCodexModelsRequestForGroup(t, handler, group, "")
	require.NotEqual(t, http.StatusOK, first.Code)
	require.Equal(t, 1, fetches.count(1), "闲置账号不在冷启动里拉，回退时经任务拉一次")

	second := performCodexModelsRequestForGroup(t, handler, group, "")
	require.Equal(t, http.StatusServiceUnavailable, second.Code, "冷却中又没有观测：排除后无账号可选")
	require.Equal(t, 1, fetches.count(1), "冷却期内不重复拉取")
}
