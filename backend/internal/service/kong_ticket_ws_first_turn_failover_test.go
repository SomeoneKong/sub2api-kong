//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 原生 WS 首轮拿不到票时必须能换账号，不能直接断连。
//
// 这条走**真实入口** `ProxyResponsesWebSocketFromClient`：拒服发生在 `parseClientPayload(1, ...)`，
// 那一步在取上游连接与首帧上送之前，客户端也还没收到任何字节——所以换号是安全的，而别的账号完全可能
// 此刻就有票。只测包装 helper 不够：helper 对了、入口没接上去的缺陷，只有走真实入口才看得见。
func TestKongWSIngressFirstTurnDenyReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	// 票据出口配成 none：这个账号没有主动取票能力，拒服原因是 no_ticket_source——账号级，换号有用。
	account := kongTestAccount(7, KongTicketModeFull, KongTicketEgressNone)
	account.Platform = PlatformOpenAI
	account.Type = AccountTypeAPIKey
	account.Status = StatusActive
	account.Schedulable = true
	account.Concurrency = 1
	account.Credentials = map[string]any{"api_key": "sk-test"}
	account.Extra["responses_websockets_v2_enabled"] = true

	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(account)
	ticketSvc := kongBatchServiceWith(t, newKongStubRepo(), &kongStubUpstream{}, accounts, false, false)

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     newOpenAIWSConnPool(cfg),
		kongTicket:       NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}

	proxyErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{})
		if err != nil {
			proxyErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		req := r.Clone(r.Context())
		req.Header = req.Header.Clone()
		req.Header.Set("User-Agent", "unit-test-agent/1.0")
		ginCtx.Request = req
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			proxyErrCh <- readErr
			return
		}
		proxyErrCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "sk-test", firstMessage, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText,
		[]byte(`{"type":"response.create","model":"`+kongBatchAstra+`","stream":false,"input":[{"type":"input_text","text":"hi"}]}`)))
	cancelWrite()

	var proxyErr error
	select {
	case proxyErr = <-proxyErrCh:
	case <-time.After(8 * time.Second):
		t.Fatal("等待 ingress websocket 结束超时")
	}

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(proxyErr, &failoverErr),
		"首轮票据拒服必须包成 failover 错误让上层换号，实得 %v", proxyErr)
	require.True(t, failoverErr.ShouldRetryNextAccount(), "no_ticket_source 是账号级条件，必须允许换号")
	reason, ok := KongTicketDenyReasonOf(failoverErr)
	require.True(t, ok, "耗尽呈现要认得出这是票据拒服")
	require.Equal(t, KongDenyNoTicketSource, reason)

	var closeErr *OpenAIWSClientCloseError
	require.False(t, errors.As(proxyErr, &closeErr), "首轮不该直接按策略关闭连接")
}
