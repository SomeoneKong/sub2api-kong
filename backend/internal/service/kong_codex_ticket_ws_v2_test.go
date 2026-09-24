//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// HTTP→WS 通路（forwardOpenAIWSV2）：HTTP 客户端、上游走连接池里的 WS。payload 是 map 形态；客户端的
// x-codex-turn-state 请求头由本服务放进与上游那条连接的握手头。

// kongWSTicketV2Context 构造一次 HTTP 请求的 gin 上下文；headerState 非空时放进 x-codex-turn-state 请求头。
func kongWSTicketV2Context(ctx context.Context, w http.ResponseWriter, headerState string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
	if headerState != "" {
		c.Request.Header.Set(openAICodexTurnStateHeader, headerState)
	}
	return c
}

// kongWSTicketV2Body 构造 HTTP 请求体；frameState 非空时把票放进 client_metadata。
func kongWSTicketV2Body(frameState string) []byte {
	if frameState == "" {
		return []byte(fmt.Sprintf(`{"model":%q,"stream":false,"input":[{"type":"input_text","text":"hi"}]}`, kongWSTicketModel))
	}
	return []byte(fmt.Sprintf(`{"model":%q,"stream":false,"input":[{"type":"input_text","text":"hi"}],"client_metadata":{"x-codex-turn-state":%q}}`, kongWSTicketModel, frameState))
}

func kongWSTicketV2Forward(t *testing.T, svc *OpenAIGatewayService, account *Account, headerState, frameState string) *OpenAIForwardResult {
	t.Helper()
	ctx := context.Background()
	return kongWSTicketV2ForwardWith(t, ctx, kongWSTicketV2Context(ctx, httptest.NewRecorder(), headerState), svc, account, kongWSTicketV2Body(frameState))
}

func kongWSTicketV2ForwardWith(t *testing.T, ctx context.Context, c *gin.Context, svc *OpenAIGatewayService, account *Account, body []byte) *OpenAIForwardResult {
	t.Helper()
	result, err := svc.Forward(ctx, c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode, "这条用例要走 HTTP→WS，不能回退 HTTP")
	return result
}

// 同一条池化连接上的三个请求：
//
//	请求 1：body 里没票 → 出站取握手时发出去的连接级票（客户端请求头带来的 dial-state）；握手票与带内
//	        下发都收，握手票不进特征。
//	请求 2：复用连接、body 里带 frame-state → 出站以 body 为准；上游不下发 → 下发一项为空；握手票不再收。
//	请求 3：复用连接、客户端这次带来的请求头是 later-state，但这次没握手 → 出站仍是建连时发出去的
//	        dial-state；同一轮下发两张，特征取后一张、两张都收。
func TestKongCodexTicketWSHTTPv2FeaturesAndTicketsAcrossPooledRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(kongWSTicketCodexMetadata("issued-1")),
		[]byte(kongWSTicketCompleted("resp_kong_v2_1")),
		[]byte(kongWSTicketCompleted("resp_kong_v2_2")),
		[]byte(kongWSTicketResponseMetadata("issued-3a")),
		[]byte(kongWSTicketCodexMetadata("issued-3b")),
		[]byte(kongWSTicketCompleted("resp_kong_v2_3")),
	}}
	dialer := &openAIWSCaptureDialer{conn: captureConn, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7501)

	first := kongWSTicketV2Forward(t, svc, account, "dial-state", "")
	require.Equal(t, "resp_kong_v2_1", first.RequestID)
	kongTestFeaturesEqual(t, first.KongRequestFeatures, kongStrPtr("dial-state"), kongStrPtr("issued-1"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, first.UpstreamModel, "hs-state", "issued-1")

	second := kongWSTicketV2Forward(t, svc, account, "later-state", "frame-state")
	require.Equal(t, "resp_kong_v2_2", second.RequestID)
	kongTestFeaturesEqual(t, second.KongRequestFeatures, kongStrPtr("frame-state"), nil)
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, second.UpstreamModel)

	third := kongWSTicketV2Forward(t, svc, account, "later-state", "")
	require.Equal(t, "resp_kong_v2_3", third.RequestID)
	kongTestFeaturesEqual(t, third.KongRequestFeatures, kongStrPtr("dial-state"), kongStrPtr("issued-3b"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, third.UpstreamModel, "issued-3a", "issued-3b")

	require.Equal(t, 1, dialer.DialCount(), "三个请求本该复用同一条上游连接")
	require.Equal(t, "dial-state", dialer.lastHeaders.Get(openAIWSTurnStateHeader))
	require.Len(t, captureConn.writes, 3)
	require.Equal(t, map[string]any{"x-codex-turn-state": "frame-state"}, captureConn.writes[1]["client_metadata"], "body 里的票应原样上送")
}

// 非 OAuth 账号：特征照记，握手票与下发票都不收。
func TestKongCodexTicketWSHTTPv2APIKeyAccountRecordsFeaturesWithoutTickets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(kongWSTicketCodexMetadata("issued-apikey")),
		[]byte(kongWSTicketCompleted("resp_kong_v2_apikey")),
	}}
	dialer := &openAIWSCaptureDialer{conn: captureConn, handshake: kongWSTicketHandshake("hs-state")}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolAPIKeyAccount(7502)

	result := kongWSTicketV2Forward(t, svc, account, "dial-state", "frame-state")
	kongTestFeaturesEqual(t, result.KongRequestFeatures, kongStrPtr("frame-state"), kongStrPtr("issued-apikey"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, "")
}

// 客户端在收到第一段输出后走了（请求 context 被取消）：HTTP→WS 改用脱离取消的 context 继续排空，之后
// 才到的带内下发照样记进本轮特征并收票。
//
// 取消落在下行写上（与 TestForwardOpenAIWSV2_ClientCancellationDrainsWithoutSyntheticFailure 同一搭法），
// 下一次读之前就能发现、换成脱离取消的读。
func TestKongCodexTicketWSHTTPv2ClientCanceledStillCollects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureConn := &openAIWSCancelSafeConn{openAIWSCaptureConn: &openAIWSCaptureConn{
		events: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_kong_v2_gone","model":"gpt-5.1"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			[]byte(kongWSTicketCodexMetadata("issued-after-cancel")),
			[]byte(kongWSTicketCompleted("resp_kong_v2_gone")),
		},
	}}
	dialer := &openAIWSClientConnCancelDialer{conn: captureConn}
	svc, collector := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
	account := kongWSTicketPoolOAuthAccount(7503)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelOnFirstWriteResponseWriter{cancel: cancel}
	// 流式请求：事件边到边写，第一段下行写就把请求取消掉。
	body := []byte(fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"type":"input_text","text":"hi"}]}`, kongWSTicketModel))
	result := kongWSTicketV2ForwardWith(t, ctx, kongWSTicketV2Context(ctx, writer, ""), svc, account, body)
	require.True(t, result.ClientDisconnect, "这条用例要覆盖客户端走后的排空")
	require.Equal(t, "resp_kong_v2_gone", result.RequestID)
	require.NotContains(t, writer.body.String(), "issued-after-cancel", "这张票应在客户端走之后才到")
	kongTestFeaturesEqual(t, result.KongRequestFeatures, nil, kongStrPtr("issued-after-cancel"))
	kongWSTicketRequireTickets(t, kongDrainTickets(collector), account.ID, result.UpstreamModel, "issued-after-cancel")
}
