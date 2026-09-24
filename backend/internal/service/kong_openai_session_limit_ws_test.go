//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// WebSocket 的后续轮次不经过选号，会话要在每轮解析处按建连选号的会话哈希续期（ctx_pool 与 HTTP bridge 共用这一处）。
func TestKongSessionWSIngressRenewsWithSelectionHash(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	account := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge,
			"max_sessions":                 1,
			"session_idle_timeout_minutes": 15,
		}}
	cfg := &config.Config{}
	cfg.Gateway.MaxLineSize = defaultMaxLineSize
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeHTTPBridge
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 5
	sse := `data: {"type":"response.completed","response":{"id":"resp_s","model":"gpt-5.1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	cache := newKongFakeSessionCache(time.Now)
	cache.put(account.ID, "other") // 账号已满，续期仍要放行登记
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSStateStore: NewOpenAIWSStateStore(nil)}
	svc.SetKongSessionLimitCache(cache)

	turns := make(chan struct{}, 1)
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, first, err := conn.Read(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		hooks := &OpenAIWSIngressHooks{
			ClientLifecycleContext: ctx,
			AfterTurn:              func(int, *OpenAIForwardResult, error) { turns <- struct{}{} },
		}
		serverErr <- svc.ProxyResponsesWebSocketFromClient(KongWithSessionCountHash(ctx, "s-ws"), c, conn, account, "test-token", first, hooks)
	}))
	defer server.Close()

	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"hi"}`)); err != nil {
		t.Fatalf("发帧: %v", err)
	}
	select {
	case <-turns:
	case err := <-serverErr:
		t.Fatalf("会话提前结束: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = conn.Close(coderws.StatusNormalClosure, "done")

	if !cache.has(account.ID, "s-ws") {
		t.Fatalf("每轮应按建连选号的会话哈希续期：%v", cache.sessions)
	}
	if len(cache.sessions[account.ID]) != 2 {
		t.Fatalf("续期不查上限，也不能把线程级执行作用域当成新会话：%v", cache.sessions)
	}
}
