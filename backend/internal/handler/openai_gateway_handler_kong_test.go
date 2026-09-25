package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func skipUnlessKongDefaultCodexReasoningIncluded(t *testing.T) {
	t.Helper()
	if _, set := os.LookupEnv(service.KongCodexReasoningIncludedEnv); set {
		t.Skipf("%s 已设置，本组只验证默认开启", service.KongCodexReasoningIncludedEnv)
	}
}

// 流式请求排队等用户槽时，排队心跳会在选定上游之前提交响应头。x-reasoning-included 要在那之前写好，
// 否则后面各出口再写都不会上线。开关在进程内只解析一次，宿主设置了该变量时只能跳过。
func TestOpenAIResponses_KongReasoningIncludedBeforeQueuePing(t *testing.T) {
	skipUnlessKongDefaultCodexReasoningIncluded(t)
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name string
		ua   string
		want string
	}{
		{name: "codex 客户端", ua: "codex_cli_rs/0.156.0 (Ubuntu 24.4.0; x86_64) xterm-256color", want: "1"},
		{name: "非 codex 客户端", ua: "curl/8.5.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":"hi"}`))
			c.Request.Header.Set("User-Agent", tc.ua)
			groupID := int64(1)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &groupID, Group: &service.Group{ID: groupID}, User: &service.User{ID: 20}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 20, Concurrency: 1})

			// 第一次取用户槽失败、进入排队，心跳间隔短于重试退避，所以取到槽之前先发出一次心跳。
			cache := &helperConcurrencyCacheStub{userSeq: []bool{false, true}, waitAllowed: true}
			h := &OpenAIGatewayHandler{
				gatewayService:      &service.OpenAIGatewayService{},
				billingCacheService: service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil),
				apiKeyService:       &service.APIKeyService{},
				concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatComment, 20*time.Millisecond),
				cfg:                 &config.Config{},
			}

			h.Responses(c)

			require.True(t, strings.HasPrefix(rec.Body.String(), ":\n\n"), "这条用例要让排队心跳最先写出")
			require.Equal(t, tc.want, rec.Result().Header.Get("X-Reasoning-Included"))
		})
	}
}

// 客户端直连 WS 时，x-reasoning-included 随 101 握手响应发出，只给 codex 客户端。
func TestOpenAIResponsesWebSocket_KongReasoningIncludedOnHandshake(t *testing.T) {
	skipUnlessKongDefaultCodexReasoningIncluded(t)
	gin.SetMode(gin.TestMode)
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.cfg = &config.Config{}
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	for _, tc := range []struct {
		name string
		ua   string
		want string
	}{
		{name: "codex 客户端", ua: "codex_cli_rs/0.156.0 (Ubuntu 24.4.0; x86_64) xterm-256color", want: "1"},
		{name: "非 codex 客户端", ua: "curl/8.5.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelDial()
			clientConn, response, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", &coderws.DialOptions{
				HTTPHeader: http.Header{"User-Agent": []string{tc.ua}},
			})
			require.NoError(t, err)
			defer func() { _ = clientConn.CloseNow() }()
			require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
			require.Equal(t, tc.want, response.Header.Get("X-Reasoning-Included"))
		})
	}
}
