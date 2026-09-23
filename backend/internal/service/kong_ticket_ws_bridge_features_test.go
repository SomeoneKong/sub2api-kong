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
	"time"

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

// 帧里看到过客户端 state 时，client_state_* 以帧为准，并按最终出站那一份判"是否被替换"。
func TestKongMergeFrameClientState(t *testing.T) {
	ticket := strings.Repeat("t", 780)
	frameState := strings.Repeat("x", 700)
	headerState := strings.Repeat("y", 650)
	httpFeatures := func(client string) *KongRequestFeatures {
		f := &KongRequestFeatures{StateFP: kongStateFingerprint(ticket), StateLen: kongIntPtr(len(ticket))}
		if client != "" {
			f.ClientStateFP, f.ClientStateLen = kongStateFingerprint(client), kongIntPtr(len(client))
		}
		return f
	}
	frameRecorder := func(client kongStateObservation) *KongFeatureRecorder {
		r := &KongFeatureRecorder{}
		r.recordOutbound(client, kongObservedState(ticket), 7)
		return r
	}

	t.Run("upgrade 头为空：补上帧内那份", func(t *testing.T) {
		got := frameRecorder(kongObservedState(frameState)).MergeFrameClientState(httpFeatures(""))
		if got.ClientStateFP != kongStateFingerprint(frameState) || got.ClientStateLen == nil || *got.ClientStateLen != len(frameState) {
			t.Errorf("客户端那份应当取帧内的值：%+v", got)
		}
		if got.StateFP != kongStateFingerprint(ticket) {
			t.Error("出站那一项仍以最终 HTTP 上送为准")
		}
	})
	t.Run("upgrade 头是旧值：不能记成客户端那份", func(t *testing.T) {
		got := frameRecorder(kongObservedState(frameState)).MergeFrameClientState(httpFeatures(headerState))
		if got.ClientStateFP != kongStateFingerprint(frameState) {
			t.Errorf("应当是帧内的值而不是 upgrade 头：%+v", got)
		}
	})
	t.Run("帧内那份与最终出站相同：不算替换", func(t *testing.T) {
		got := frameRecorder(kongObservedState(ticket)).MergeFrameClientState(httpFeatures(headerState))
		if got.ClientStateFP != "" || got.ClientStateLen != nil {
			t.Errorf("与出站相同就是同一件事，不该记客户端那份：%+v", got)
		}
	})
	t.Run("被跨账号剥离的那份同样算帧内观测", func(t *testing.T) {
		r := frameRecorder(kongStateObservation{})
		r.RecordStrippedClientState(frameState)
		got := r.MergeFrameClientState(httpFeatures(headerState))
		if got.ClientStateFP != kongStateFingerprint(frameState) {
			t.Errorf("被剥掉的客户端原值应当顶替 upgrade 头：%+v", got)
		}
	})
	t.Run("帧里没带 state：保留 HTTP 那侧的观测", func(t *testing.T) {
		got := frameRecorder(kongStateObservation{}).MergeFrameClientState(httpFeatures(headerState))
		if got.ClientStateFP != kongStateFingerprint(headerState) {
			t.Errorf("帧里没看到客户端 state 时不该改动：%+v", got)
		}
	})
}

// 完整的 ingress → bridge 路径：用量结果里的客户端 state 来自这一轮的帧，而不是 upgrade 头。
//
// 直接调 proxyOpenAIWSHTTPBridgeTurn 的用例覆盖不到这一交接——ingress 解析帧时记下的观测要经这条路
// 带到 bridge 的结果上，漏接不会报错，只会让 client_state 一直显示成连接级的旧值或干脆缺失。
func TestKongWSIngressBridgeCarriesFrameClientState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticket := strings.Repeat("t", 780)
	frameState := strings.Repeat("x", 700)
	headerState := strings.Repeat("y", 650)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	account.Platform = PlatformOpenAI
	account.Type = AccountTypeOAuth
	account.Credentials = map[string]any{"access_token": "test-token"}
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
	account.Concurrency = 1
	account.ProxyID = nil

	cfg := &config.Config{}
	cfg.Gateway.MaxLineSize = defaultMaxLineSize
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeHTTPBridge
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 5
	sse := `data: {"type":"response.completed","response":{"id":"resp_k","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSStateStore: NewOpenAIWSStateStore(nil)}
	svc.SetKongTicketGateway(g)

	features := make(chan *KongRequestFeatures, 1)
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
			AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) {
				if result != nil {
					features <- result.KongRequestFeatures
				}
			},
		}
		serverErr <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test-token", first, hooks)
	}))
	defer server.Close()

	header := http.Header{}
	header.Set(openAIWSTurnStateHeader, headerState)
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	frame := `{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"hi","client_metadata":{"` +
		openAIWSTurnStateMetadataKey + `":"` + frameState + `"}}`
	if err := conn.Write(ctx, coderws.MessageText, []byte(frame)); err != nil {
		t.Fatalf("发帧: %v", err)
	}

	select {
	case f := <-features:
		if f == nil {
			t.Fatal("这一轮应当带请求特征")
		}
		if f.StateFP != kongStateFingerprint(ticket) {
			t.Errorf("出站那一项应当是注入的票：%+v", f)
		}
		if f.ClientStateFP != kongStateFingerprint(frameState) || f.ClientStateLen == nil || *f.ClientStateLen != len(frameState) {
			t.Errorf("客户端那一项应当取这一轮帧内的值，而不是 upgrade 头：%+v", f)
		}
	case err := <-serverErr:
		t.Fatalf("会话提前结束: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = conn.Close(coderws.StatusNormalClosure, "done")
}
