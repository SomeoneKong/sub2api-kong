//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codex 票的被动观测在原生 WS 三条通路上的集成测试。本文件放三条通路共用的辅助函数与 ctx_pool
// 通路的用例；passthrough 与 HTTP→WS 的用例在同前缀的另外两个文件里。
//
// 各用例共同的口径：
//   - 出站那一项（State）：帧内 client_metadata 有票时取帧内值，没有时取这条连接握手时真正发出去的值；
//   - 下发那一项（Reissued）：只取这一轮响应里带内 metadata 事件下发的票，同一轮多张取最后一张；
//   - 握手响应里的票只进票表、不进任何一轮的特征，同一条物理连接只收一次；
//   - 只有 OpenAI OAuth 账号收票，其它账号照记特征、不收票。

const (
	kongWSTicketModel     = "gpt-5.1"
	kongWSTicketWaitLimit = 3 * time.Second
)

// kongWSTicketCodexMetadata 是原生 WS 上游的带内下发形态：事件名带 codex. 前缀，票在顶层 headers。
func kongWSTicketCodexMetadata(state string) string {
	return fmt.Sprintf(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":%q}}`, state)
}

// kongWSTicketResponseMetadata 是 SSE 同款形态：事件名不带前缀，票在 response.headers，键名大小写不同。
func kongWSTicketResponseMetadata(state string) string {
	return fmt.Sprintf(`{"type":"response.metadata","response":{"headers":{"X-Codex-Turn-State":%q}}}`, state)
}

func kongWSTicketCompleted(responseID string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":%q,"usage":{"input_tokens":1,"output_tokens":1}}}`, responseID, kongWSTicketModel)
}

// kongWSTicketCreateFrame 构造一帧 response.create；frameState 非空时把票放进 client_metadata。
func kongWSTicketCreateFrame(frameState string) string {
	if frameState == "" {
		return fmt.Sprintf(`{"type":"response.create","model":%q,"stream":false}`, kongWSTicketModel)
	}
	return fmt.Sprintf(`{"type":"response.create","model":%q,"stream":false,"client_metadata":{"x-codex-turn-state":%q}}`, kongWSTicketModel, frameState)
}

// kongWSTicketHandshake 是上游握手响应头：带一张握手票。
func kongWSTicketHandshake(state string) http.Header {
	return http.Header{http.CanonicalHeaderKey(openAIWSTurnStateHeader): []string{state}}
}

// kongWSTicketTurn 是一次 AfterTurn 回调。
type kongWSTicketTurn struct {
	turn   int
	result *OpenAIForwardResult
	err    error
}

// kongWSTicketTurnHooks 返回只记录 AfterTurn 的 hooks，各轮结果按回调顺序进 channel。
func kongWSTicketTurnHooks() (*OpenAIWSIngressHooks, <-chan kongWSTicketTurn) {
	ch := make(chan kongWSTicketTurn, 16)
	hooks := &OpenAIWSIngressHooks{
		AfterTurn: func(turn int, result *OpenAIForwardResult, turnErr error) {
			ch <- kongWSTicketTurn{turn: turn, result: result, err: turnErr}
		},
	}
	return hooks, ch
}

// kongWSTicketNextResult 等下一轮的结果，要求这一轮成功。
func kongWSTicketNextResult(t *testing.T, ch <-chan kongWSTicketTurn) *OpenAIForwardResult {
	t.Helper()
	select {
	case got := <-ch:
		require.NoError(t, got.err, "第 %d 轮不该失败", got.turn)
		require.NotNil(t, got.result, "第 %d 轮没有结果", got.turn)
		return got.result
	case <-time.After(kongWSTicketWaitLimit):
		t.Fatal("等待本轮结果超时")
		return nil
	}
}

// kongWSTicketStartIngressServer 起一个客户端 WS 入口：读首帧后交给 ProxyResponsesWebSocketFromClient。
// 走哪条通路（ctx_pool / passthrough）由配置与账号决定。每个会话的返回值依次进 channel。
func kongWSTicketStartIngressServer(t *testing.T, svc *OpenAIGatewayService, account *Account, token string, hooks *OpenAIWSIngressHooks) (*httptest.Server, <-chan error) {
	t.Helper()
	serverErr := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		req := r.Clone(r.Context())
		req.Header = req.Header.Clone()
		req.Header.Set("User-Agent", "unit-test-agent/1.0")
		ginCtx.Request = req

		readCtx, cancel := context.WithTimeout(r.Context(), kongWSTicketWaitLimit)
		_, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			serverErr <- readErr
			return
		}
		serverErr <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, token, firstMessage, hooks)
	}))
	t.Cleanup(server.Close)
	return server, serverErr
}

// kongWSTicketDialClient 以客户端身份连上入口；upgradeState 非空时放进 upgrade 请求头（连接级的票）。
func kongWSTicketDialClient(t *testing.T, server *httptest.Server, upgradeState string) *coderws.Conn {
	t.Helper()
	var opts *coderws.DialOptions
	if upgradeState != "" {
		opts = &coderws.DialOptions{HTTPHeader: http.Header{
			http.CanonicalHeaderKey(openAIWSTurnStateHeader): []string{upgradeState},
		}}
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), kongWSTicketWaitLimit)
	defer cancel()
	conn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func kongWSTicketClientWrite(t *testing.T, conn *coderws.Conn, payload string) {
	t.Helper()
	writeCtx, cancel := context.WithTimeout(context.Background(), kongWSTicketWaitLimit)
	defer cancel()
	require.NoError(t, conn.Write(writeCtx, coderws.MessageText, []byte(payload)))
}

// kongWSTicketReadUntilCompleted 读客户端收到的帧直到 response.completed，返回这期间收到的事件类型。
func kongWSTicketReadUntilCompleted(t *testing.T, conn *coderws.Conn) []string {
	t.Helper()
	var types []string
	for {
		readCtx, cancel := context.WithTimeout(context.Background(), kongWSTicketWaitLimit)
		_, message, err := conn.Read(readCtx)
		cancel()
		require.NoError(t, err)
		eventType := gjson.GetBytes(message, "type").String()
		types = append(types, eventType)
		if eventType == "response.completed" {
			return types
		}
	}
}

// kongWSTicketWaitServer 等入口会话结束。客户端正常关闭时各通路应返回 nil（passthrough 可能带回
// 客户端的正常关闭码）。
func kongWSTicketWaitServer(t *testing.T, serverErr <-chan error) {
	t.Helper()
	select {
	case err := <-serverErr:
		if err != nil {
			require.Contains(t, err.Error(), "StatusNormalClosure", "入口会话异常结束")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待入口会话结束超时")
	}
}

// kongWSTicketRequireTickets 断言收票队列里恰好是这些票（按收到的顺序），账号与模型都对得上。
func kongWSTicketRequireTickets(t *testing.T, got []KongObservedTicket, accountID int64, model string, states ...string) {
	t.Helper()
	gotStates := make([]string, 0, len(got))
	for _, ticket := range got {
		gotStates = append(gotStates, ticket.State)
	}
	if len(states) == 0 {
		require.Empty(t, gotStates, "不该收到任何票")
		return
	}
	require.Equal(t, states, gotStates, "收到的票不对")
	require.NotEmpty(t, model, "模型不该为空")
	for _, ticket := range got {
		require.Equal(t, accountID, ticket.AccountID, "票 %q 的账号不对", ticket.State)
		require.Equal(t, model, ticket.Model, "票 %q 的模型不对", ticket.State)
		require.False(t, ticket.CapturedAt.IsZero(), "票 %q 缺收到时刻", ticket.State)
	}
}

// ---- ctx_pool ----

func kongWSTicketPoolConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return cfg
}

// kongWSTicketPoolService 起一个走连接池的服务（ctx_pool 与 HTTP→WS 共用），装上带收票队列的观测入口。
func kongWSTicketPoolService(t *testing.T, cfg *config.Config, dialer openAIWSClientDialer) (*OpenAIGatewayService, *KongTicketCollector) {
	t.Helper()
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	observer, collector := newKongTestObserver()
	svc.SetKongTicketObserver(observer)
	return svc, collector
}

func kongWSTicketPoolOAuthAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "kong-ws-ticket-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_enabled": true},
	}
}

func kongWSTicketPoolAPIKeyAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "kong-ws-ticket-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
}

// 同一条上游连接上的三轮：每轮的特征只带自己的票，握手票只在第一轮收一次、不进任何一轮的特征。
//
//	第 1 轮：帧里没票 → 出站取握手时发出去的连接级票；上游带内下发 issued-1。
//	第 2 轮：帧里带 frame-state → 出站以帧为准；同一轮下发两张（两种事件形态），特征取后一张、两张都收。
//	第 3 轮：帧里没票、上游不下发 → 出站回到连接级票，下发一项为空（不能顶着上一轮的票）。
func TestKongCodexTicketWSCtxPoolMultiTurnAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(kongWSTicketCodexMetadata("issued-1")),
		[]byte(kongWSTicketCompleted("resp_kong_ctx_1")),
		[]byte(kongWSTicketResponseMetadata("issued-2a")),
		[]byte(kongWSTicketCodexMetadata("issued-2b")),
		[]byte(kongWSTicketCompleted("resp_kong_ctx_2")),
		[]byte(kongWSTicketCompleted("resp_kong_ctx_3")),
	}}
	dialer := &openAIWSCaptureDialer{conn: captureConn, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7301)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "conn-state")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	require.Equal(t, []string{"codex.response.metadata", "response.completed"}, kongWSTicketReadUntilCompleted(t, client))
	first := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, first.KongRequestFeatures, kongStrPtr("conn-state"), kongStrPtr("issued-1"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, first.UpstreamModel, "hs-state", "issued-1")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame("frame-state"))
	kongWSTicketReadUntilCompleted(t, client)
	second := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, second.KongRequestFeatures, kongStrPtr("frame-state"), kongStrPtr("issued-2b"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, second.UpstreamModel, "issued-2a", "issued-2b")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	kongWSTicketReadUntilCompleted(t, client)
	third := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, third.KongRequestFeatures, kongStrPtr("conn-state"), nil)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, third.UpstreamModel)

	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	require.Equal(t, 1, dialer.DialCount(), "三轮应在同一条上游连接上")
	require.Equal(t, "conn-state", dialer.lastHeaders.Get(openAIWSTurnStateHeader), "连接级的票应放进上游握手头")
	require.Len(t, captureConn.writes, 3)
	for i, write := range captureConn.writes {
		require.Equal(t, first.UpstreamModel, write["model"], "第 %d 帧上送的模型", i+1)
	}
}

// 连接池把上一个会话用过的连接交给下一个会话：握手票不再收；本会话客户端 upgrade 头上带来的票一个字节
// 都没上送（本轮根本没握手），出站那一项必须是这条连接建连时真正发出去的那个。
func TestKongCodexTicketWSCtxPoolReusedConnKeepsDialStateAndSkipsHandshakeTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(kongWSTicketCompleted("resp_kong_reuse_1")),
		[]byte(kongWSTicketCodexMetadata("issued-reuse")),
		[]byte(kongWSTicketCompleted("resp_kong_reuse_2")),
	}}
	dialer := &openAIWSCaptureDialer{conn: captureConn, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7302)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)

	firstClient := kongWSTicketDialClient(t, server, "dial-state")
	kongWSTicketClientWrite(t, firstClient, kongWSTicketCreateFrame(""))
	kongWSTicketReadUntilCompleted(t, firstClient)
	first := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, first.KongRequestFeatures, kongStrPtr("dial-state"), nil)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, first.UpstreamModel, "hs-state")
	require.NoError(t, firstClient.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	secondClient := kongWSTicketDialClient(t, server, "later-state")
	kongWSTicketClientWrite(t, secondClient, kongWSTicketCreateFrame(""))
	kongWSTicketReadUntilCompleted(t, secondClient)
	second := kongWSTicketNextResult(t, turns)
	require.NoError(t, secondClient.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	require.Equal(t, 1, dialer.DialCount(), "第二个会话本该复用池里那条连接")
	kongTestFeaturesEqual(t, second.KongRequestFeatures, kongStrPtr("dial-state"), kongStrPtr("issued-reuse"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, second.UpstreamModel, "issued-reuse")
}

// kongWSTicketQueueDialer 按顺序交出连接，每条连接配自己的握手响应头，并记下每次拨号的请求头。
type kongWSTicketQueueDialer struct {
	mu         sync.Mutex
	conns      []openAIWSClientConn
	handshakes []http.Header
	sent       []http.Header
}

func (d *kongWSTicketQueueDialer) Dial(_ context.Context, _ string, headers http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := len(d.sent)
	if i >= len(d.conns) {
		return nil, http.StatusServiceUnavailable, nil, errors.New("没有可交出的测试连接")
	}
	d.sent = append(d.sent, cloneHeader(headers))
	return d.conns[i], 0, cloneHeader(d.handshakes[i]), nil
}

func (d *kongWSTicketQueueDialer) sentHeaders() []http.Header {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]http.Header(nil), d.sent...)
}

// 新拨的连接 A 首帧就写失败（write_upstream 重试换到新拨的连接 B）：两条都是真实握手过的物理连接，
// 两张握手票都要收，模型都记这一轮的模型，且都不进本轮的下发特征。
func TestKongCodexTicketWSCtxPoolWriteFailRetryCollectsBothHandshakeTickets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	good := &openAIWSCaptureConn{events: [][]byte{[]byte(kongWSTicketCompleted("resp_kong_retry"))}}
	dialer := &kongWSTicketQueueDialer{
		conns:      []openAIWSClientConn{&openAIWSWriteFailAfterFirstTurnConn{failOnWrite: true}, good},
		handshakes: []http.Header{kongWSTicketHandshake("hs-A"), kongWSTicketHandshake("hs-B")},
	}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7305)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	kongWSTicketReadUntilCompleted(t, client)
	result := kongWSTicketNextResult(t, turns)
	require.Equal(t, "resp_kong_retry", result.RequestID)
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	sent := dialer.sentHeaders()
	require.Len(t, sent, 2, "首帧写失败后本该换一条新拨的连接重试")
	require.Empty(t, sent[0].Get(openAIWSTurnStateHeader), "客户端没带票，第一次拨号不该发")
	// ctx_pool 把上一条连接握手拿到的票放进之后的拨号头，B 握手时真正发出去的就是 hs-A。
	require.Equal(t, "hs-A", sent[1].Get(openAIWSTurnStateHeader))
	require.Len(t, good.writes, 1)
	require.Equal(t, result.UpstreamModel, good.writes[0]["model"], "票的模型应是这一轮实际上送的模型")

	kongTestFeaturesEqual(t, result.KongRequestFeatures, kongStrPtr("hs-A"), nil)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, result.UpstreamModel, "hs-A", "hs-B")
}

// 非 OAuth 账号：特征照记（帧内出站 + 带内下发），但握手票与下发票都不收。
func TestKongCodexTicketWSCtxPoolAPIKeyAccountRecordsFeaturesWithoutTickets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(kongWSTicketCodexMetadata("issued-apikey")),
		[]byte(kongWSTicketCompleted("resp_kong_apikey")),
	}}
	dialer := &openAIWSCaptureDialer{conn: captureConn, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolAPIKeyAccount(7303)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "sk-test", hooks)
	client := kongWSTicketDialClient(t, server, "")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame("frame-state"))
	kongWSTicketReadUntilCompleted(t, client)
	result := kongWSTicketNextResult(t, turns)
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	kongTestFeaturesEqual(t, result.KongRequestFeatures, kongStrPtr("frame-state"), kongStrPtr("issued-apikey"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, "")
}

// 客户端在这一轮中途断开：ctx_pool 继续排空上游，之后才到的带内下发照样记进本轮特征并收票。
func TestKongCodexTicketWSCtxPoolClientDisconnectStillCollects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 第一个事件延迟 250ms，让客户端的断开先传到服务端，使后续下行写可靠失败。
	captureConn := &openAIWSCaptureConn{
		readDelays: []time.Duration{250 * time.Millisecond, 0, 0},
		events: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_kong_gone","model":"gpt-5.1"}}`),
			[]byte(kongWSTicketCodexMetadata("issued-after-gone")),
			[]byte(kongWSTicketCompleted("resp_kong_gone")),
		},
	}
	dialer := &openAIWSCaptureDialer{conn: captureConn}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7304)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	require.NoError(t, client.CloseNow(), "模拟客户端在这一轮中途断开")

	result := kongWSTicketNextResult(t, turns)
	require.Equal(t, "resp_kong_gone", result.RequestID)
	kongWSTicketWaitServer(t, serverErr)

	kongTestFeaturesEqual(t, result.KongRequestFeatures, nil, kongStrPtr("issued-after-gone"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, result.UpstreamModel, "issued-after-gone")
}
