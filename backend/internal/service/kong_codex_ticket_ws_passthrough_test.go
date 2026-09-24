//go:build unit

package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// passthrough 通路：两个 goroutine 双向转发，每条客户端会话自己拨一次上游。出站的连接级值是握手请求头里
// 真正发出去的那个，特征按"这一帧到达时正在转发的那一轮"归属。

// kongWSTicketPassthroughDialer 把同一条分阶段上游连接交给 passthrough，记下握手请求头，并在握手响应里
// 带上配置的头。
type kongWSTicketPassthroughDialer struct {
	mu          sync.Mutex
	conn        *stagedPassthroughConn
	handshake   http.Header
	lastHeaders http.Header
	dials       int
}

func (d *kongWSTicketPassthroughDialer) Dial(_ context.Context, _ string, headers http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastHeaders = cloneHeader(headers)
	d.dials++
	return d.conn, http.StatusSwitchingProtocols, cloneHeader(d.handshake), nil
}

func (d *kongWSTicketPassthroughDialer) snapshot() (http.Header, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneHeader(d.lastHeaders), d.dials
}

func kongWSTicketPassthroughConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return cfg
}

func kongWSTicketPassthroughService(dialer *kongWSTicketPassthroughDialer) (*OpenAIGatewayService, *KongTicketCollector) {
	cfg := kongWSTicketPassthroughConfig()
	svc := &OpenAIGatewayService{
		cfg:                       cfg,
		httpUpstream:              &httpUpstreamRecorder{},
		cache:                     &stubGatewayCache{},
		openaiWSResolver:          NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:             NewCodexToolCorrector(),
		openaiWSPassthroughDialer: dialer,
	}
	observer, collector := newKongTestObserver()
	svc.SetKongTicketObserver(observer)
	return svc, collector
}

func kongWSTicketPassthroughOAuthAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "kong-ws-ticket-passthrough-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough},
	}
}

func kongWSTicketPassthroughAPIKeyAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "kong-ws-ticket-passthrough-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough},
	}
}

// kongWSTicketPassthroughUpstreamModel 取这一帧实际上送的模型。
func kongWSTicketPassthroughUpstreamModel(t *testing.T, upstream *stagedPassthroughConn) string {
	t.Helper()
	frame := requirePassthroughUpstreamWrite(t, upstream, kongWSTicketWaitLimit)
	require.Equal(t, "response.create", gjson.GetBytes(frame, "type").String())
	model := gjson.GetBytes(frame, "model").String()
	require.NotEmpty(t, model)
	return model
}

// 同一条 passthrough 会话上的三轮：每轮只带自己的票，握手票拨号后收一次、不进任何一轮的特征。
func TestKongCodexTicketWSPassthroughMultiTurnAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newStagedPassthroughConn()
	dialer := &kongWSTicketPassthroughDialer{conn: upstream, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPassthroughService(dialer)
	account := kongWSTicketPassthroughOAuthAccount(7401)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "conn-state")

	// 第 1 轮：帧里没票 → 出站取握手请求头里发出去的连接级票；上游带内下发 issued-1。
	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	model := kongWSTicketPassthroughUpstreamModel(t, upstream)
	upstream.Send(kongWSTicketCodexMetadata("issued-1"))
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_1"))
	require.Equal(t, []string{"codex.response.metadata", "response.completed"}, kongWSTicketReadUntilCompleted(t, client))
	first := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, first.KongRequestFeatures, kongStrPtr("conn-state"), kongStrPtr("issued-1"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, model, "hs-state", "issued-1")

	// 第 2 轮：帧里带票 → 出站以帧为准；同一轮下发两张，特征取后一张、两张都收。
	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame("frame-state"))
	require.Equal(t, model, kongWSTicketPassthroughUpstreamModel(t, upstream))
	upstream.Send(kongWSTicketResponseMetadata("issued-2a"))
	upstream.Send(kongWSTicketCodexMetadata("issued-2b"))
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_2"))
	kongWSTicketReadUntilCompleted(t, client)
	second := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, second.KongRequestFeatures, kongStrPtr("frame-state"), kongStrPtr("issued-2b"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, model, "issued-2a", "issued-2b")

	// 第 3 轮：帧里没票、上游不下发 → 出站回到连接级票，下发一项为空。
	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	kongWSTicketPassthroughUpstreamModel(t, upstream)
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_3"))
	kongWSTicketReadUntilCompleted(t, client)
	third := kongWSTicketNextResult(t, turns)
	kongTestFeaturesEqual(t, third.KongRequestFeatures, kongStrPtr("conn-state"), nil)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, model)

	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, model)

	headers, dials := dialer.snapshot()
	require.Equal(t, 1, dials)
	require.Equal(t, "conn-state", headers.Get(openAIWSTurnStateHeader), "连接级的票应放进上游握手头")
}

// 客户端没带连接级的票、握手也没发：出站那一项只看帧；帧里也没有时整份特征为空。
func TestKongCodexTicketWSPassthroughNoConnectionState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newStagedPassthroughConn()
	dialer := &kongWSTicketPassthroughDialer{conn: upstream}
	svc, collector := kongWSTicketPassthroughService(dialer)
	account := kongWSTicketPassthroughOAuthAccount(7402)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	kongWSTicketPassthroughUpstreamModel(t, upstream)
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_none_1"))
	kongWSTicketReadUntilCompleted(t, client)
	kongTestFeaturesEqual(t, kongWSTicketNextResult(t, turns).KongRequestFeatures, nil, nil)

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame("frame-only"))
	kongWSTicketPassthroughUpstreamModel(t, upstream)
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_none_2"))
	kongWSTicketReadUntilCompleted(t, client)
	kongTestFeaturesEqual(t, kongWSTicketNextResult(t, turns).KongRequestFeatures, kongStrPtr("frame-only"), nil)

	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, "")
	headers, _ := dialer.snapshot()
	require.Empty(t, headers.Get(openAIWSTurnStateHeader))
}

// 非 OAuth 账号：特征照记，握手票与下发票都不收。
func TestKongCodexTicketWSPassthroughAPIKeyAccountRecordsFeaturesWithoutTickets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newStagedPassthroughConn()
	dialer := &kongWSTicketPassthroughDialer{conn: upstream, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPassthroughService(dialer)
	account := kongWSTicketPassthroughAPIKeyAccount(7403)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "sk-test", hooks)
	client := kongWSTicketDialClient(t, server, "conn-state")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame(""))
	kongWSTicketPassthroughUpstreamModel(t, upstream)
	upstream.Send(kongWSTicketCodexMetadata("issued-apikey"))
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_apikey"))
	kongWSTicketReadUntilCompleted(t, client)
	result := kongWSTicketNextResult(t, turns)
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	kongWSTicketWaitServer(t, serverErr)

	kongTestFeaturesEqual(t, result.KongRequestFeatures, kongStrPtr("conn-state"), kongStrPtr("issued-apikey"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, "")
}

// 客户端在这一轮中途正常关闭：passthrough 进入排空，之后才到的带内下发照样记进本轮特征并收票。
func TestKongCodexTicketWSPassthroughClientGoneDrainStillCollects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newStagedPassthroughConn()
	dialer := &kongWSTicketPassthroughDialer{conn: upstream}
	svc, collector := kongWSTicketPassthroughService(dialer)
	account := kongWSTicketPassthroughOAuthAccount(7404)
	hooks, turns := kongWSTicketTurnHooks()
	server, serverErr := kongWSTicketStartIngressServer(t, svc, account, "test-token", hooks)
	client := kongWSTicketDialClient(t, server, "")

	kongWSTicketClientWrite(t, client, kongWSTicketCreateFrame("frame-state"))
	model := kongWSTicketPassthroughUpstreamModel(t, upstream)
	// 第一帧下行之后 relay 才开始读客户端：先让客户端收到一帧，它的关闭才会被当作"客户端走了"。
	upstream.Send(`{"type":"response.created","response":{"id":"resp_kong_pt_gone","model":"gpt-5.1"}}`)
	readCtx, cancel := context.WithTimeout(context.Background(), kongWSTicketWaitLimit)
	_, created, err := client.Read(readCtx)
	cancel()
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.GetBytes(created, "type").String())
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "bye"))
	// 给 relay 一点时间把"客户端已走"落定成排空（丢弃下行写），再让上游发后面的事件。
	time.Sleep(150 * time.Millisecond)

	upstream.Send(kongWSTicketCodexMetadata("issued-while-draining"))
	upstream.Send(kongWSTicketCompleted("resp_kong_pt_gone"))
	result := kongWSTicketNextResult(t, turns)
	require.Equal(t, "resp_kong_pt_gone", result.RequestID)
	kongWSTicketWaitServer(t, serverErr)

	kongTestFeaturesEqual(t, result.KongRequestFeatures, kongStrPtr("frame-state"), kongStrPtr("issued-while-draining"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, model, "issued-while-draining")
}
