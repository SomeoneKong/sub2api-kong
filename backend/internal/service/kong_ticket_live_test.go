//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Live（realtime）把模型放在 `session.model`，顶层没有 `model`。按顶层判会得出"非门控"，于是受保护
// 账号会在门控模型上发出一次**无票上送**——那是放行未受保障的输出，本项目最重的一类缺陷。
//
// 这条通路承载不了票据（创建之后是 sideband 原始双向转发，没有逐轮注入点也没有接受证据），所以按默认
// 拒绝极性拒服。
func TestKongLiveGatedSessionModelIsRefused(t *testing.T) {
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	account.Concurrency = 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: account}}
	ticketSvc := kongBatchServiceWith(t, newKongStubRepo(), &kongStubUpstream{}, accounts, false, false)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: 201, Header: http.Header{"Location": {"/backend-api/codex/call_test"}},
		Body: io.NopCloser(strings.NewReader("sdp-answer")),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		kongTicket:   NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}

	gated := &LiveCallRequest{SDP: "sdp-offer", Session: json.RawMessage(`{"model":"` + kongBatchAstra + `"}`)}
	require.NoError(t, ValidateLiveCallRequest(gated))
	_, err := svc.createUpstreamLiveCall(context.Background(), account, gated, "test-attestation")
	require.Error(t, err, "受保护账号无票时不得发出门控 Live 会话")
	require.Empty(t, upstream.requests, "一个字节都不该发出去")
	require.True(t, KongIsTicketDenied(err))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	reason, ok := KongTicketDenyReasonOf(failoverErr)
	require.True(t, ok)
	require.Equal(t, KongDenyLiveUnsupported, reason)
	require.False(t, failoverErr.ShouldRetryNextAccount(), "换号得不出别的结论")

	// **只拦受保护的账号**：off / observe / 未配置的账号本来就不受票据保障，拦它们是无谓拒服，还会让
	// mode=off 这个退出手段在 Live 上失效。这个分支排在 EnsureTicket 之前，拿不到模式判定的保护。
	for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve} {
		relaxed := kongTestAccount(1, mode, KongTicketEgressNone)
		relaxed.Credentials = account.Credentials
		accounts.set(relaxed)
		before := len(upstream.requests)
		if _, relaxedErr := svc.createUpstreamLiveCall(context.Background(), relaxed, gated, "test-attestation"); relaxedErr != nil {
			require.False(t, KongIsTicketDenied(relaxedErr), "%s 不在保护范围内，不该被票据拦住：%v", mode, relaxedErr)
		}
		require.Greater(t, len(upstream.requests), before, "%s 的会话应当照常上送", mode)
	}
	accounts.set(account)

	// 非门控的 Live 会话照常服务——门控之外的请求本来就不受票据保障。
	plain := &LiveCallRequest{SDP: "sdp-offer", Session: json.RawMessage(`{"model":"gpt-realtime"}`)}
	if _, plainErr := svc.createUpstreamLiveCall(context.Background(), account, plain, "test-attestation"); plainErr != nil {
		require.False(t, KongIsTicketDenied(plainErr), "非门控 Live 不该被票据拦住：%v", plainErr)
	}
	require.Len(t, upstream.requests, 3, "off / observe 各一次 + 非门控一次，都应照常上送")
}

// sideband 是原始双向转发：创建端点挡住了门控模型，但协议允许会话中途改模型，不挡这一步就留了一条
// "先建非门控会话、再切到门控模型"的绕行。
func TestKongLiveSidebandFrameGuard(t *testing.T) {
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: account}}
	ticketSvc := kongBatchServiceWith(t, newKongStubRepo(), &kongStubUpstream{}, accounts, false, false)
	g := NewKongTicketGateway(ticketSvc, []string{kongBatchAstra})
	ctx := context.Background()
	// 账号在建连时解析一次，逐帧判定不再读库。
	guard, guardErr := g.LiveFrameGuard(ctx, 1)
	require.NoError(t, guardErr)

	blocked := [][]byte{
		[]byte(`{"type":"session.update","session":{"model":"` + kongBatchAstra + `"}}`),
		[]byte(`{"type":"response.create","response":{"model":"` + kongBatchAstra + `"}}`),
		// 重复键即不可判定：上游读哪一个我们说不准，按默认拒绝极性拒。
		[]byte(`{"type":"session.update","session":{"model":"gpt-realtime","model":"` + kongBatchAstra + `"}}`),
	}
	for _, payload := range blocked {
		err := guard(payload)
		require.Error(t, err, "声明门控模型的帧必须挡住：%s", payload)
		require.True(t, KongIsTicketDenied(err))
	}

	allowed := [][]byte{
		[]byte(`{"type":"session.update","session":{"model":"gpt-realtime"}}`),
		[]byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`),
		[]byte(`not-json`),
	}
	for _, payload := range allowed {
		require.NoError(t, guard(payload), "不声明门控模型的帧照常转发：%s", payload)
	}

	// 未受保护的账号不受影响。
	observe := kongTestAccount(2, KongTicketModeObserve, KongTicketEgressNone)
	accounts.set(observe)
	observeGuard, observeErr := g.LiveFrameGuard(ctx, 2)
	require.NoError(t, observeErr)
	require.NoError(t, observeGuard(
		[]byte(`{"type":"session.update","session":{"model":"`+kongBatchAstra+`"}}`)))

	// **装配失败必须 fail-closed**：账号读不到时返回错误，而不是一个放行一切的空守卫——那等于让一次
	// 瞬时读库故障把整条连接的保护关掉，而会话可以先用非门控模型建好再切过去。
	if _, err := g.LiveFrameGuard(ctx, 404); err == nil {
		t.Error("账号不存在时应当报错而不是返回空守卫")
	}
}
