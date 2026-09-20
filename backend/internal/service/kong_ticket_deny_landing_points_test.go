//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
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

// 票据拒服的处置规则要落到**每一条**转发入口与每一个时点上。这一组用例各盯一个曾经漏掉的落点，
// 都走真实入口——教训是"规则写对了、某条入口没接上"，只测判定函数看不出来。

// 交接分支：票据出口被**别的账号**占着时，拒服原因必须是 egress_busy 而不是 preparing。
//
// preparing 的含义是"本账号的任务正在跑，产物就是本账号要的票"，那是唯一值得先在同账号等一等的原因。
// 报成 preparing 会让上层把重试预算花在一个确定不会变的状态上（那个任务的票属于别的账号，而它那次
// 取票还会把共享出口的静默清零），并延后真正能服务的那次换号。
func TestKongHandoffEgressBusyIsNotReportedAsPreparing(t *testing.T) {
	repo, _, svc := kongManualFixture(t, kongBatchAstra)
	// 别的账号有合格票 → 交接值得做，于是走 prepareOrHandOff 这条分支。
	kongSeedElsewhereTicket(repo, 2, kongBatchAstra, 99, map[string]float64{kongBatchAstra: 1})
	// 第三个账号占住同一条票据出口。
	task, claim := svc.claimTask(3, kongBatchAstra, KongEgressKey(KongTicketEgressDirect, nil))
	require.Equal(t, kongClaimFresh, claim)
	defer svc.releaseTask(3, task)

	account, err := svc.accounts.GetByID(context.Background(), 1)
	require.NoError(t, err)
	cfg, _ := ParseKongTicketConfig(account.Extra)
	grant, err := svc.prepareOrHandOff(context.Background(), account, cfg, kongBatchAstra, KongActionFetch)
	require.NoError(t, err)
	require.Equal(t, KongDenyEgressBusy, grant.DenyReason)

	canFailover, retrySame := KongTicketFailover(context.Background(), &KongErrTicketDenied{Reason: grant.DenyReason})
	require.True(t, canFailover, "账号级条件，换号有用")
	require.False(t, retrySame, "等这个出口是白等：它一释放本号就变成 window_closed")
}

// 首输出守卫不得把"等本账号的票"改写成上游首输出超时。
//
// 守卫的 context 也套在 doOpenAIUpstream 里的票据准入上，所以等票超过首输出期限时守卫会先被触发。
// 照原顺序走，这次本地拒服会变成 504 / first_output_timeout：票据身份丢失、错误记到上游头上、
// 账号健康度被罚——而一个字节都没发给上游。
func TestKongTicketWaitIsNotRecordedAsFirstOutputTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	account.Concurrency = 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: account}}
	ticketSvc := kongBatchServiceWith(t, newKongStubRepo(), &kongStubUpstream{}, accounts, false, false)
	// 占住本账号的任务槽位且永不完成：请求会挂在 awaitTask 上，直到首输出期限到。
	task, claim := ticketSvc.claimTask(1, kongBatchAstra, "verify:1")
	require.Equal(t, kongClaimFresh, claim)
	defer ticketSvc.releaseTask(1, task)

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds: 1, MaxLineSize: defaultMaxLineSize,
		}},
		httpUpstream: upstream,
		kongTicket:   NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}
	body := []byte(`{"model":"` + kongBatchAstra + `","stream":true,"reasoning":{"effort":"low"},"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Empty(t, upstream.requests, "票没拿到就不该发出业务请求")
	require.True(t, KongIsTicketDeniedFailover(err), "本地等票不能被记成上游故障，否则账号健康度被误罚")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	reason, ok := KongTicketDenyReasonOf(failoverErr)
	require.True(t, ok)
	require.Equal(t, KongDenyWaitTimeout, reason, "等票超时是账号级条件，不是共享存储故障")
	require.True(t, failoverErr.ShouldRetryNextAccount(), "别的账号此刻可能就有票")
}

// HTTP→WS 通路（客户端 HTTP、上游 WebSocket）的准入失败要走 HTTP 那套统一转换。
//
// 它既不过 doOpenAIUpstream 也不过两条原生 WS 适配器。裸返回拒服会让 handler 的
// errors.As(*UpstreamFailoverError) 不命中，请求落进通用兜底的 502，而别的账号完全可能此刻就有票。
func TestKongHTTPToWSDenyReachesAccountFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	account.Concurrency = 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	account.Extra["responses_websockets_v2_enabled"] = true
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: account}}
	repo := newKongStubRepo()
	kongSeedElsewhereTicket(repo, 2, kongBatchAstra, 99, map[string]float64{kongBatchAstra: 1})
	ticketSvc := kongBatchServiceWith(t, repo, &kongStubUpstream{}, accounts, false, false)

	conn := &openAIWSCaptureConn{}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
	defer pool.Close()
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(),
		openaiWSPool: pool,
		kongTicket:   NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}
	body := []byte(`{"model":"` + kongBatchAstra + `","stream":false,"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.True(t, KongIsTicketDenied(err), "原始拒服类型要留住，调度上报据它豁免")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "拒服要能让上层换号")
	require.Empty(t, conn.writes, "拒服时一帧都不该上送")
	require.False(t, c.Writer.Written(), "呈现由耗尽 handler 负责，这一层不写响应")
}

// bridge 的**第二次准入**（doOpenAIUpstream → PrepareUpstream）失败时也要保住拒服身份。
//
// parseClientPayload 那一次准入成功，不代表这一次还成立——两者之间票可能过期或被并发撤销。走通用
// 路径会同时坏三件事：给客户端发一帧 502 upstream_error、把原始错误字符串化（类型丢了，调度豁免与
// 策略关闭都失效）、于是一次本地拒服被记成账号故障。
func TestKongBridgeSecondAdmissionKeepsPolicyClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.MaxLineSize = defaultMaxLineSize
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeHTTPBridge
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3

	account := kongTestAccount(7, KongTicketModeFull, KongTicketEgressNone)
	account.Concurrency = 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	accounts := &kongStubAccounts{accounts: map[int64]*Account{7: account}}
	repo := newKongStubRepo()
	repo.setCurrent(7, kongBatchAstra, &KongTicket{
		ID: 7, AccountID: 7, Model: kongBatchAstra, State: strings.Repeat("a", 292),
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
		FingerprintProbs: map[string]float64{kongBatchAstra: 1},
	})
	ticketSvc := kongBatchServiceWith(t, repo, &kongStubUpstream{}, accounts, false, false)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_first\",\"model\":\"" +
				kongBatchAstra + "\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
	}}
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(),
		kongTicket: NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}

	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, acceptErr := coderws.Accept(w, r, nil)
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		defer func() { _ = wsConn.CloseNow() }()
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = r.Clone(r.Context())
		_, first, readErr := wsConn.Read(r.Context())
		if readErr != nil {
			errCh <- readErr
			return
		}
		// 第二轮开始前撤掉当前票：模拟两次准入之间的并发撤销。
		hooks := &OpenAIWSIngressHooks{BeforeTurn: func(turn int) error {
			if turn == 2 {
				repo.setCurrent(7, kongBatchAstra)
			}
			return nil
		}}
		errCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, wsConn, account, "test-token", first, hooks)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	clientConn, _, dialErr := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, dialErr)
	defer func() { _ = clientConn.CloseNow() }()

	frame := []byte(`{"type":"response.create","model":"` + kongBatchAstra + `","input":"hi"}`)
	require.NoError(t, clientConn.Write(ctx, coderws.MessageText, frame))
	_, firstOut, readErr := clientConn.Read(ctx)
	require.NoError(t, readErr)
	require.Contains(t, string(firstOut), "response.completed")

	require.NoError(t, clientConn.Write(ctx, coderws.MessageText, frame))
	// 第二轮要么直接收到关闭帧，要么收到一帧——但那一帧绝不能是"上游故障"。
	if _, secondOut, secondErr := clientConn.Read(ctx); secondErr == nil {
		require.NotContains(t, string(secondOut), "upstream_error", "本地拒服不该被写成上游故障帧")
	}

	proxyErr := <-errCh
	require.True(t, KongIsTicketDenied(proxyErr), "第二次准入要保住原始拒服类型")
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, proxyErr, &closeErr, "会话已建立，后续轮次按策略关闭而不是换号")
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	require.Len(t, upstream.requests, 1, "第二轮没拿到票就不该再发业务请求")
}

// kongCtxBlockedAccounts 让账号读取一直等到调用方的 context 结束：模拟准入读库阶段被守卫取消。
type kongCtxBlockedAccounts struct {
	blockID  int64
	fallback KongAccountLoader
}

func (a *kongCtxBlockedAccounts) GetByID(ctx context.Context, id int64) (*Account, error) {
	if id == a.blockID {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.fallback.GetByID(ctx, id)
}

// 首输出守卫在**准入读库阶段**取消时，也必须归成账号级 wait_timeout。
//
// 只修 `awaitTask` 那个出口不够：守卫的 context 同样覆盖账号读取、当前票/候选/出口活动/冷却的读取。
// 那些位置返回 context 错误时被包成 `ensure_failed`（系统级、带 NextAccountStop），于是"这个账号没
// 来得及、别的账号有票"变成整个请求终止——无谓拒服。所以判据落在准入的共同边界上。
func TestKongAdmissionCancelDuringDBReadStaysAccountScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	blocked := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	blocked.Concurrency = 1
	blocked.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	ready := kongTestAccount(2, KongTicketModeFull, KongTicketEgressNone)
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: blocked, 2: ready}}
	repo := newKongStubRepo()
	// 账号 2 有一张合格票：换过去就能服务，所以"在账号 1 上终止"是可证的损害。
	kongSeedElsewhereTicket(repo, 2, kongBatchAstra, 99, map[string]float64{kongBatchAstra: 1})
	ticketSvc := kongBatchServiceWith(t, repo, &kongStubUpstream{}, accounts, false, false)
	ticketSvc.accounts = &kongCtxBlockedAccounts{blockID: 1, fallback: accounts}

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds: 1, MaxLineSize: defaultMaxLineSize,
		}},
		httpUpstream: upstream,
		kongTicket:   NewKongTicketGateway(ticketSvc, []string{kongBatchAstra}),
	}
	body := []byte(`{"model":"` + kongBatchAstra + `","stream":true,"reasoning":{"effort":"low"},"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	_, err := svc.Forward(context.Background(), c, blocked, body)
	require.Error(t, err)
	require.Empty(t, upstream.requests, "票没拿到就不该发出业务请求")
	require.NoError(t, c.Request.Context().Err(), "客户端还在：这次取消来自守卫，不是客户端断开")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	reason, ok := KongTicketDenyReasonOf(failoverErr)
	require.True(t, ok, "身份要留住，否则本地拒服被计入账号健康度")
	require.Equal(t, KongDenyWaitTimeout, reason)
	require.True(t, failoverErr.ShouldRetryNextAccount(), "账号 2 有票，必须允许换过去")
}

// 图片 Responses 驱动的**顶层模型由 SUB2API_IMAGES_MAIN_MODEL 独立决定**，所以"图片模型白名单保证受控
// 模型不可达"这个推断不成立。那条路上的拒服必须保住身份，不能被字符串化成普通错误。
func TestKongImagesDriverModelDenyKeepsIdentity(t *testing.T) {
	t.Setenv("SUB2API_IMAGES_MAIN_MODEL", kongBatchAstra)
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressNone)
	account.Concurrency = 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	accounts := &kongStubAccounts{accounts: map[int64]*Account{1: account}}
	ticketSvc := kongBatchServiceWith(t, newKongStubRepo(), &kongStubUpstream{}, accounts, false, false)

	upstream := &httpUpstreamRecorder{}
	svc := newOpenAIImagesTestService(upstream)
	svc.kongTicket = NewKongTicketGateway(ticketSvc, []string{kongBatchAstra})
	body := []byte(`{"model":"gpt-image-1","prompt":"draw a cat"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed := &OpenAIImagesRequest{
		Model: "gpt-image-1", Prompt: "draw a cat",
		Endpoint: openAIImagesGenerationsEndpoint, Body: body,
	}

	_, err := svc.forwardOpenAIImagesOAuth(context.Background(), c, account, parsed, "")
	require.Error(t, err)
	require.Empty(t, upstream.requests, "无票不得上送")
	require.True(t, KongIsTicketDenied(err), "原始拒服类型要留住，调度上报据它豁免")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "拒服要能让上层换号、到达 503 呈现")
}
