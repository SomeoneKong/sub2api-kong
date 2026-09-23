package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 这组测试验证默认比例（1，即 context_window 取 max）。比例在进程内只解析一次，
// 宿主设置了该变量时无法在测试里恢复默认，只能跳过。
func skipUnlessKongDefaultContextWindowRatio(t *testing.T) {
	t.Helper()
	if _, set := os.LookupEnv(service.KongCodexContextWindowRatioEnv); set {
		t.Skipf("%s 已设置，本组只验证默认比例", service.KongCodexContextWindowRatioEnv)
	}
}

const kongContextWindowUpstreamBody = `{"models":[{"slug":"gpt-5.6-sol","context_window":272000,"max_context_window":872000}]}`

func kongCodexContextWindowOf(t *testing.T, body []byte, slug string) (window, maxWindow float64) {
	t.Helper()
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope), "body=%s", body)
	for _, model := range envelope.Models {
		if model["slug"] == slug {
			window, windowOK := model["context_window"].(float64)
			maxWindow, maxOK := model["max_context_window"].(float64)
			require.True(t, windowOK && maxOK, "context_window / max_context_window 应为数字：%v", model)
			return window, maxWindow
		}
	}
	t.Fatalf("slug %q not in manifest: %s", slug, body)
	return 0, 0
}

// requireKongContextWindowRaisedWithFinalETag 验证正文已折算，且 304 按折算后的 ETag 判定。
func requireKongContextWindowRaisedWithFinalETag(t *testing.T, handler *OpenAIGatewayHandler, group *service.Group, staleETag string) {
	t.Helper()
	first := performCodexModelsRequestForGroup(t, handler, group, staleETag)
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
	window, maxWindow := kongCodexContextWindowOf(t, first.Body.Bytes(), "gpt-5.6-sol")
	require.Equal(t, float64(872000), maxWindow)
	require.Equal(t, float64(872000), window)
	finalETag := first.Header().Get("ETag")
	require.Equal(t, service.CodexModelsManifestETag(first.Body.Bytes()), finalETag)

	second := performCodexModelsRequestForGroup(t, handler, group, finalETag)
	require.Equal(t, http.StatusNotModified, second.Code, "body=%s", second.Body.String())
	require.Zero(t, second.Body.Len())
}

func newKongContextWindowAPIKeyHandler(accounts []service.Account) (*OpenAIGatewayHandler, *service.OpenAIGatewayService) {
	upstream := &codexModelsFailoverHTTPUpstream{firstBody: kongContextWindowUpstreamBody}
	gatewayService := service.NewOpenAIGatewayService(
		codexModelsFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return &OpenAIGatewayHandler{gatewayService: gatewayService}, gatewayService
}

func kongContextWindowAPIKeyAccount() service.Account {
	return service.Account{
		ID:          1,
		Name:        "custom-openai",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://upstream.example/v1",
		},
	}
}

func TestKongCodexModelsRaisesContextWindowOnSchedulerPath(t *testing.T) {
	skipUnlessKongDefaultContextWindowRatio(t)
	handler, _ := newKongContextWindowAPIKeyHandler([]service.Account{kongContextWindowAPIKeyAccount()})
	group := &service.Group{ID: 51, Platform: service.PlatformOpenAI}
	requireKongContextWindowRaisedWithFinalETag(t, handler, group, "")
}

func TestKongCodexModelsRaisesContextWindowOnPinnedPath(t *testing.T) {
	skipUnlessKongDefaultContextWindowRatio(t)
	handler, _ := newKongContextWindowAPIKeyHandler([]service.Account{kongContextWindowAPIKeyAccount()})
	group := &service.Group{
		ID:                        52,
		Platform:                  service.PlatformOpenAI,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{1}},
	}
	requireKongContextWindowRaisedWithFinalETag(t, handler, group, "")
}

func TestKongCodexModelsRaisesContextWindowOnConfiguredPath(t *testing.T) {
	skipUnlessKongDefaultContextWindowRatio(t)
	mapped := kongContextWindowAPIKeyAccount()
	mapped.Credentials["model_mapping"] = map[string]any{"glm-5.3": "glm-5.3"}
	oauth := service.Account{
		ID:          2,
		Name:        "chatgpt-oauth",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    1,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-test"},
	}
	handler, gatewayService := newKongContextWindowAPIKeyHandler([]service.Account{mapped, oauth})
	group := &service.Group{ID: 53, Platform: service.PlatformOpenAI}

	// 本地生成的目录里 gpt-5.6-sol 是 272000 / 872000；客户端手里是它的 ETag 时也要拿到折算结果。
	unraised, configured, err := gatewayService.BuildGroupConfiguredCodexModelsManifest(context.Background(), group, "")
	require.NoError(t, err)
	require.True(t, configured)
	window, maxWindow := kongCodexContextWindowOf(t, unraised.Body, "gpt-5.6-sol")
	require.Equal(t, float64(272000), window)
	require.Equal(t, float64(872000), maxWindow)

	requireKongContextWindowRaisedWithFinalETag(t, handler, group, unraised.ETag)
}
