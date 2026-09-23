//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// WS 客户端经 HTTP 上游（bridge）的那一轮，用量结果要带上本次上送记下的请求特征。
//
// 特征记录器挂在 doOpenAIUpstream 的出站请求上，而 bridge 的结果在发送循环之外构造：不在循环里取，
// 用量行就只剩空值——门控请求照样上送、照样回发 state，证据却一条不留。
func TestKongWSHTTPBridgeResultCarriesRequestFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeOff, strings.Repeat("a", 780))
	account.Platform = PlatformOpenAI
	account.Type = AccountTypeAPIKey
	account.Concurrency = 1
	account.ProxyID = nil

	reissued := strings.Repeat("r", 780)
	sse := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_k","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{reissued},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream: upstream,
	}
	svc.SetKongTicketGateway(g)

	payload := []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "test-token", payload, len(payload),
		"gpt-6-astra", "", "", "", "", 1,
		"",
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("bridge 这一轮不该失败: %v", err)
	}
	f := result.KongRequestFeatures
	if f == nil || f.ReissuedLen == nil || *f.ReissuedLen != len(reissued) || f.ReissuedFP != kongStateFingerprint(reissued) {
		t.Fatalf("上游回发的 state 应当进入结果的请求特征：%+v", f)
	}
}

// bridge 首轮上游回发 state（注入没被接受）：零正文交付，按策略关闭（1008），不能呈现成上游故障。
func TestKongWSHTTPBridgeFirstTurnDeliveryBlockedClosesAsPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, strings.Repeat("a", 780))
	account.Platform = PlatformOpenAI
	account.Type = AccountTypeAPIKey
	account.Concurrency = 1
	account.ProxyID = nil

	sse := `data: {"type":"response.completed","response":{"id":"resp_k","status":"completed"}}` + "\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{strings.Repeat("r", 780)},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream: upstream,
	}
	svc.SetKongTicketGateway(g)

	payload := []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	writes := 0
	_, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "test-token", payload, len(payload),
		"gpt-6-astra", "", "", "", "", 1,
		"",
		func([]byte) error { writes++; return nil },
	)
	var closeErr *OpenAIWSClientCloseError
	if !errors.As(err, &closeErr) || closeErr.StatusCode() != coderws.StatusPolicyViolation {
		t.Fatalf("首轮交付拦截应当按策略关闭（1008），得到 %v", err)
	}
	if !KongIsDeliveryBlocked(err) {
		t.Error("错误类型要保留，调度上报据它识别本地拦截")
	}
	if writes != 0 {
		t.Errorf("被拦截的一轮不得向客户端写任何东西，实写 %d 帧", writes)
	}
}
