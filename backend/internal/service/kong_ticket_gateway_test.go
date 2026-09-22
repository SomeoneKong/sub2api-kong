package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tidwall/gjson"
)

// 保护边界的判据全都不是「看起来对」能验的：漏一条路径不会报错，只会安静地交付降智输出。
// 这些用例锁住的正是那几条判定。

func kongTestGatedRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	return req
}

// 门控判定必须看**最终请求体解码后的顶层 model**，并且必须区分「确定非门控」与「判不出」。
//
// 这里每一条都是一条真实的绕过面：转义值/键、重复键（gjson 取第一个而 encoding/json 取最后
// 一个，上游按哪个解释我们不知道）、非字符串 model、畸形请求体。
func TestKongExtractTopLevelModel(t *testing.T) {
	cases := []struct {
		name             string
		body             string
		wantModel        string
		wantDeterminable bool
	}{
		{"顶层 model", `{"model":"gpt-6-astra","input":[]}`, "gpt-6-astra", true},
		{"非门控模型照样解析出来", `{"model":"gpt-5.6-sol"}`, "gpt-5.6-sol", true},
		{"值用 JSON 转义写", `{"model":"gpt-6-\u0061stra"}`, "gpt-6-astra", true},
		{"键用 JSON 转义写", `{"\u006dodel":"gpt-6-astra"}`, "gpt-6-astra", true},
		{"model 不是第一个键", `{"input":[{"text":"x"}],"model":"gpt-6-astra"}`, "gpt-6-astra", true},
		{"被跳过的字段里有超大数字", `{"tools":[{"schema":1e400}],"model":"gpt-6-astra"}`, "gpt-6-astra", true},
		{"模型名只出现在用户正文里", `{"model":"gpt-5.6-sol","input":[{"text":"gpt-6-astra"}]}`, "gpt-5.6-sol", true},
		{"没有顶层 model", `{"input":[{"text":"gpt-6-astra"}]}`, "", true},
		{"顶层不是对象", `["gpt-6-astra"]`, "", true},

		// 以下都是「判不出」：受保护账号必须拒服，不能当成非门控放行。
		{"重复 model 键", `{"model":"gpt-5.6-sol","model":"gpt-6-astra"}`, "", false},
		{"重复 model 键（反序）", `{"model":"gpt-6-astra","model":"gpt-5.6-sol"}`, "", false},
		{"重复键其中一个非字符串", `{"model":123,"model":"gpt-6-astra"}`, "", false},
		{"model 不是字符串", `{"model":123}`, "", false},
		{"不是 JSON", `not json but "gpt-6-astra"`, "", false},
		{"截断的 JSON", `{"model":"gpt-6-astra"`, "", false},
		{"尾随垃圾", `{"model":"gpt-6-astra"} trailing`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kongExtractTopLevelModel([]byte(c.body))
			if got.Determinable != c.wantDeterminable {
				t.Fatalf("Determinable = %v（原因 %q），want %v", got.Determinable, got.Reason, c.wantDeterminable)
			}
			if got.Model != c.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, c.wantModel)
			}
			if !got.Determinable && got.Reason == "" {
				t.Error("判不出时必须给出原因，否则拒服事件无从排查")
			}
		})
	}
}

// 判不出模型时的处置按账号是否受保护分流：full 拒服，off/observe 照常服务。
func TestKongUndeterminableModelHandling(t *testing.T) {
	g := NewKongTicketGateway(&KongTicketService{}, []string{"gpt-6-astra"})
	// 重复 model 键 → 判不出。
	body := `{"model":"gpt-5.6-sol","model":"gpt-6-astra"}`

	full := &Account{ID: 1, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeFull)}}
	if _, err := g.PrepareUpstream(context.Background(), kongTestGatedRequest(t, body), full); !KongIsTicketDenied(err) {
		t.Errorf("受保护账号判不出模型时必须拒服，得到 %v", err)
	}
	for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve} {
		account := &Account{ID: 2, Extra: map[string]any{KongTicketModeKey: string(mode)}}
		attempt, err := g.PrepareUpstream(context.Background(), kongTestGatedRequest(t, body), account)
		if err != nil || attempt != nil {
			t.Errorf("%s 账号不该被拒：attempt=%v err=%v", mode, attempt, err)
		}
	}
}

// 读请求体不得改变它：注入之后原请求必须仍可逐字节发送。
func TestKongPrepareUpstreamKeepsBodyIntact(t *testing.T) {
	g := NewKongTicketGateway(&KongTicketService{}, []string{"gpt-6-astra"})
	body := `{"model":"gpt-5.6-sol","input":[{"text":"hello"}]}`
	req := kongTestGatedRequest(t, body)

	if _, err := g.PrepareUpstream(context.Background(), req, &Account{ID: 1}); err != nil {
		t.Fatalf("非门控模型不该报错: %v", err)
	}
	sent, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读原请求体: %v", err)
	}
	if string(sent) != body {
		t.Errorf("原请求体被改动了：\n得到 %q\n应为 %q", sent, body)
	}
}

// 请求体不可重读时，受保护账号必须拒服：放行等于开一个说不清受不受保护的出口。
// 未受保护的账号照常服务——它的请求本来就不注入、不判定。
func TestKongGatewayUnreadableBodyHandling(t *testing.T) {
	g := NewKongTicketGateway(&KongTicketService{}, []string{"gpt-6-astra"})
	newReq := func() *http.Request {
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		req.GetBody = nil
		return req
	}

	full := &Account{ID: 1, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeFull)}}
	if _, err := g.PrepareUpstream(context.Background(), newReq(), full); !KongIsTicketDenied(err) {
		t.Fatalf("受保护账号应当拒服，得到 %v", err)
	}
	off := &Account{ID: 2}
	if attempt, err := g.PrepareUpstream(context.Background(), newReq(), off); err != nil || attempt != nil {
		t.Errorf("未受保护账号不该被拒：attempt=%v err=%v", attempt, err)
	}
}

// 功能未启用（门控集合为空）时必须完全无行为：fork 的其它使用者不该因为这个定制而有任何变化。
func TestKongGatewayDisabledIsInert(t *testing.T) {
	g := NewKongTicketGateway(&KongTicketService{}, nil)
	req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)

	attempt, err := g.PrepareUpstream(context.Background(), req, &Account{ID: 1})
	if err != nil || attempt != nil {
		t.Fatalf("未启用时应当无行为，得到 attempt=%v err=%v", attempt, err)
	}
	if req.Header.Get(openAICodexTurnStateHeader) != "" {
		t.Error("未启用时不该注入任何头")
	}
	if err := g.AfterUpstream(context.Background(), &Account{ID: 1}, nil, &http.Response{Header: http.Header{}}); err != nil {
		t.Errorf("未启用时交付判定应当放行: %v", err)
	}
}

// AfterUpstream 的三种组合：注入过且上游重发 state → 拦；注入过且上游未重发 → 放行；
// 未注入（off/observe 账号）→ 一律放行，只收票。
func TestKongGatewayAfterUpstreamDecision(t *testing.T) {
	cases := []struct {
		name      string
		attempt   *KongUpstreamAttempt
		respState string
		wantBlock bool
	}{
		{
			name:      "注入过 + 上游又下发 state = 注入未被接受，必须拦",
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			respState: "aaaaaaaa",
			wantBlock: true,
		},
		{
			name:      "注入过 + 上游没下发 = 票被接受，放行",
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			respState: "",
			wantBlock: false,
		},
		{
			name:      "未注入（账号没开 full）+ 上游下发 state = 照常服务，只收票",
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra"},
			respState: "aaaaaaaa",
			wantBlock: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &KongTicketService{repo: newKongStubRepo()}
			g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})
			resp := &http.Response{Header: http.Header{}}
			if c.respState != "" {
				resp.Header.Set(openAICodexTurnStateHeader, c.respState)
			}
			err := g.AfterUpstream(context.Background(), &Account{ID: 1}, c.attempt, resp)
			if got := KongIsDeliveryBlocked(err); got != c.wantBlock {
				t.Fatalf("拦截 = %v（err=%v），want %v", got, err, c.wantBlock)
			}
		})
	}
}

// ProtectsAccount 决定「判不出模型时拒服还是放行」，只有 full 账号在保护范围内。
func TestKongProtectsAccount(t *testing.T) {
	g := NewKongTicketGateway(&KongTicketService{}, []string{"gpt-6-astra"})
	account := func(mode KongTicketMode) *Account {
		return &Account{ID: 1, Extra: map[string]any{KongTicketModeKey: string(mode)}}
	}
	if !g.ProtectsAccount(account(KongTicketModeFull)) {
		t.Error("full 账号应当在保护范围内")
	}
	for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve} {
		if g.ProtectsAccount(account(mode)) {
			t.Errorf("%s 账号不该在保护范围内：哪些账号要保护是人工配的", mode)
		}
	}
	if NewKongTicketGateway(&KongTicketService{}, nil).ProtectsAccount(account(KongTicketModeFull)) {
		t.Error("功能未启用时不该有任何账号在保护范围内")
	}
}

// WS 上票走 payload 的 client_metadata、逐轮上送——不是握手头。
func TestKongPrepareWSTurnInjectsClientMetadata(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	// 预置一张可用的当前票，让准入直接走 inject 分支。
	state := strings.Repeat("a", 292)
	repo.setCurrent(1, "gpt-6-astra", &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: state,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
		// 归因结果要齐，**分布也要**：采纳判据按白名单对分布求和，只给 argmax 的票会被判无分布
		// 而拒掉，于是准入停在拒服、根本走不到这里要验的注入逻辑。
		FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
		FingerprintProbs: kongTestAttr(0.99).Probs,
	})
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})

	payload := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"session_id":"s"}}`)
	next, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", payload)
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}
	if attempt == nil || attempt.Grant == nil {
		t.Fatal("应当注入了票")
	}
	if got := gjson.GetBytes(next, "client_metadata."+kongWSTurnStateMetadataKey).String(); got != state {
		t.Errorf("client_metadata 里的票 = %q, want %q", got, state)
	}
	// 既有的 client_metadata 字段不能被抹掉。
	if gjson.GetBytes(next, "client_metadata.session_id").String() != "s" {
		t.Error("注入时把 client_metadata 里的其它键弄丢了")
	}

	// 调用方给的模型与 payload 字段不一致时，按**实际出站字段**判定。
	//
	// 调用方的值可能取自规范化之前的 payload（规范化会重建 JSON、合并重复键），而上游只看字段。
	// 按调用方的值判会得出「非门控」而实际上送的是门控模型——那正是一条无票交付的路径。
	_, attempt2, err := g.PrepareWSTurn(context.Background(), account, "gpt-5.6-sol", payload)
	if err != nil || attempt2 == nil || attempt2.Grant == nil {
		t.Errorf("payload 字段是门控模型时必须按它判定：attempt=%v err=%v", attempt2, err)
	} else if attempt2.Model != "gpt-6-astra" {
		t.Errorf("attempt 记的模型 = %q，应当是实际出站的那个", attempt2.Model)
	}

	// payload 省略 model 时回落到调用方的值——Realtime 允许后续帧不重述模型。
	noModel := []byte(`{"type":"response.create"}`)
	if _, attempt3, err := g.PrepareWSTurn(context.Background(), account, "gpt-5.6-sol", noModel); err != nil || attempt3 != nil {
		t.Errorf("字段缺失且调用方模型非门控时应原样放行：attempt=%v err=%v", attempt3, err)
	}
	if _, attempt4, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", noModel); err != nil || attempt4 == nil || attempt4.Grant == nil {
		t.Errorf("字段缺失时应按调用方的门控模型判定：attempt=%v err=%v", attempt4, err)
	}
}

// WS 的本轮证据在带内 response.metadata 事件里；带 state 即注入未被接受，必须拦。
func TestKongGuardWSDownstream(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		attempt   *KongUpstreamAttempt
		wantBlock bool
	}{
		{
			name:      "response.metadata 带 state + 注入过 = 必须拦",
			payload:   `{"type":"response.metadata","headers":{"x-codex-turn-state":"aaa"}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: true,
		},
		{
			name:      "state 在 response.headers 下（codex 先读这一处）",
			payload:   `{"type":"response.metadata","response":{"headers":{"X-Codex-Turn-State":"aaa"}}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: true,
		},
		{
			name:      "response.metadata 不带 state = 票被接受，放行",
			payload:   `{"type":"response.metadata","headers":{"openai-model":"gpt-6-astra"}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: false,
		},
		{
			name:      "内容分片不参与判定",
			payload:   `{"type":"response.output_text.delta","delta":"x"}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: false,
		},
		{
			name:      "未注入（off/observe）+ 带 state = 只收票，照常交付",
			payload:   `{"type":"response.metadata","headers":{"x-codex-turn-state":"aaa"}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra"},
			wantBlock: false,
		},
		{
			// 原生 WS 上游发的是带 codex. 前缀的拼写，headers 里还混着别的 x-codex-* 项。
			// 帧形状取自线上实测。
			name: "codex.response.metadata 带 state = 必须拦",
			payload: `{"type":"codex.response.metadata","headers":{` +
				`"x-codex-safety-buffering-enabled":"true",` +
				`"x-codex-safety-buffering-faster-model":"gpt-fast",` +
				`"x-codex-turn-state":"aaa","x-models-etag":"W/\"abc\""}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: true,
		},
		{
			// 票被接受的那一轮：metadata 帧照样来，只是不带 turn-state。不能因为「这帧来了」就拦，
			// 否则受保护账号在 WS 上会被整轮误拦。帧形状取自线上实测的第二轮。
			name: "codex.response.metadata 不带 state = 票被接受，放行",
			payload: `{"type":"codex.response.metadata","headers":{` +
				`"x-codex-safety-buffering-enabled":"true","x-models-etag":"W/\"abc\""}}`,
			attempt:   &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}},
			wantBlock: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &KongTicketService{repo: newKongStubRepo()}
			g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})
			err := g.GuardWSDownstream(context.Background(), &Account{ID: 1}, c.attempt, []byte(c.payload))
			if got := KongIsDeliveryBlocked(err); got != c.wantBlock {
				t.Fatalf("拦截 = %v（err=%v），want %v", got, err, c.wantBlock)
			}
		})
	}
}

// 载体事件的拼写有两种，只认一种就会让整条通路静默失效：上游在原生 WS 上发的是
// codex.response.metadata，SSE 上是不带前缀的 response.metadata。
// 同一条连接上还有别的带外事件，不能按 codex. 前缀泛化。
func TestKongWSTurnStateMetadataEvent(t *testing.T) {
	for _, ok := range []string{"response.metadata", "codex.response.metadata", "  codex.response.metadata  "} {
		if !kongWSTurnStateMetadataEvent(ok) {
			t.Errorf("%q 应判为载体事件", ok)
		}
	}
	for _, no := range []string{
		"", "codex.rate_limits", "responsesapi.websocket_timing",
		"response.completed", "response.output_text.delta",
		"metadata", "codex.response.metadata.extra",
	} {
		if kongWSTurnStateMetadataEvent(no) {
			t.Errorf("%q 不该判为载体事件", no)
		}
	}
}

// 票据拒服与交付拦截不得被当成传输故障——那会记假的 request_error、可能停掉健康账号的调度，
// 还会包装成 502 去换号，把「拒服」悄悄变成「换个号照发」。
func TestKongTicketErrorsAreNotTransportFaults(t *testing.T) {
	for _, err := range []error{
		&KongErrTicketDenied{Reason: "window_closed"},
		&KongErrDeliveryBlocked{AccountID: 1, Model: "gpt-6-astra"},
	} {
		if classifyUpstreamTransportError(err).Persistent {
			t.Errorf("%T 不该被判为持久性传输故障", err)
		}
	}
}

// 计 token 的端点不参与门控：它不产出模型内容，门控它只会在无票时白拒一次计数请求。
//
// 这条同时挡住一个归因缺口：`/v1/responses/input_tokens` 的两个调用点自己写 ops 传输错误、不走
// failover 也不走 503 呈现，于是那条路上的拒服会被记成上游故障。
func TestKongNonGeneratingUpstreamPath(t *testing.T) {
	cases := map[string]bool{
		"https://chatgpt.com/backend-api/codex/responses":   false,
		"https://api.openai.com/v1/responses":               false,
		"https://api.openai.com/v1/responses/input_tokens":  true,
		"https://api.openai.com/v1/responses/input_tokens/": true,
		// 自定义 base URL 会把端点前缀成 /api/v1/...，所以按后缀匹配。
		"https://relay.example.com/api/v1/responses/input_tokens": true,
		// 形近但不同的端点不能误判：放过一个真正会产出内容的端点就是开了一个降智出口。
		"https://api.openai.com/v1/responses/input_tokens_stream": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("解析 %s: %v", raw, err)
		}
		if got := kongIsNonGeneratingUpstreamPath(u); got != want {
			t.Errorf("%s = %v, want %v", raw, got, want)
		}
	}
	if kongIsNonGeneratingUpstreamPath(nil) {
		t.Error("URL 为 nil 时不能当成非生成端点——判不出就按要票处理")
	}
}

// 受门控模型发往计 token 端点时：不要票、不注入，但仍返回 attempt，好让 AfterUpstream 收响应头里的
// 免费票（observed 票不消耗任何出口静默）。
func TestKongPrepareUpstreamSkipsTokenCounting(t *testing.T) {
	account := &Account{ID: 1, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeFull)}}
	g := NewKongTicketGateway(&KongTicketService{repo: newKongStubRepo()}, []string{"gpt-6-astra"})
	req, err := http.NewRequest(http.MethodPost,
		"https://api.openai.com/v1/responses/input_tokens",
		strings.NewReader(`{"model":"gpt-6-astra","input":"hi"}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	attempt, err := g.PrepareUpstream(context.Background(), req, account)
	if err != nil {
		t.Fatalf("计 token 端点不该被拒服，实得 %v", err)
	}
	if attempt == nil {
		t.Fatal("仍要返回 attempt，否则 AfterUpstream 不会收响应头里的免费票")
	}
	if attempt.Grant != nil {
		t.Error("计 token 端点不该注入票")
	}
	if req.Header.Get(openAICodexTurnStateHeader) != "" {
		t.Error("计 token 端点不该写票据请求头")
	}
}

// 原生 WS 首轮的票据拒服要能换账号；非账号级原因与后续轮次不行。
//
// 首轮是这条路上唯一可换号的时点：客户端还没收到任何字节、relay 还没启动、首帧也还没写上游。
// 后续轮次的会话状态活在那条上游连接里，换号等于把上下文丢掉，所以仍按策略关闭连接。
func TestKongWSFirstTurnDenyFailsOver(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)

	// 账号级：包成 failover 错误，上层据此换号。
	accountScoped := wrapOpenAIWSFirstTurnKongTicketError(c,
		&KongErrTicketDenied{Reason: KongDenyWindowClosed, RetryAfter: "2026-09-20T13:18:47Z"})
	var failoverErr *UpstreamFailoverError
	if !errors.As(accountScoped, &failoverErr) {
		t.Fatal("账号级拒服应当包成 failover 错误，否则首轮直接断连——那是无谓拒服")
	}
	if !failoverErr.ShouldRetryNextAccount() {
		t.Error("账号级拒服应当允许换号")
	}
	if _, ok := KongTicketDenyReasonOf(failoverErr); !ok {
		t.Error("耗尽呈现要认得出这是票据拒服")
	}
	var closeErr *OpenAIWSClientCloseError
	if errors.As(accountScoped, &closeErr) {
		t.Error("账号级拒服不该直接变成关闭连接")
	}
	// 恢复时刻要进请求级累计，耗尽关闭时的文案靠它。
	if wait := KongTicketDenyWaitFromContext(c); wait == nil || wait.Earliest == "" {
		t.Error("恢复时刻没有并进累计等待信息")
	}

	// 非账号级（帧语义歧义、注入失败、共享存储故障）：**同样统一包装**——身份、调度豁免、分类文案与
	// 看板归因都走同一条路，只是不换号（换号得不出别的结论）。只包"能换号的那部分"等于给其余拒服开一
	// 条绕过统一呈现的旁路。关闭码按作用域区分在 handler 侧（1008 而不是 1013）。
	for _, reason := range []string{
		"frame_undeterminable: duplicate_key:model",
		"inject_unverified: turn_state_mismatch",
		"ensure_failed: dial tcp: refused",
	} {
		err := wrapOpenAIWSFirstTurnKongTicketError(c, &KongErrTicketDenied{Reason: reason})
		var wrappedFailover *UpstreamFailoverError
		if !errors.As(err, &wrappedFailover) {
			t.Errorf("%s 也要统一包装，否则绕过统一呈现与归因", reason)
			continue
		}
		if wrappedFailover.ShouldRetryNextAccount() {
			t.Errorf("%s 不是账号级原因，不该换号", reason)
		}
		if !KongIsTicketDenied(err) {
			t.Errorf("%s 丢了原始拒服类型", reason)
		}
		if _, ok := KongTicketDenyReasonOf(wrappedFailover); !ok {
			t.Errorf("%s 的耗尽呈现认不出票据拒服", reason)
		}
	}

	// 非票据错误原样返回。
	plain := errors.New("普通错误")
	if got := wrapOpenAIWSFirstTurnKongTicketError(c, plain); !errors.Is(got, plain) {
		t.Error("非票据错误不该被改写")
	}
}

// 非账号级的拒服照样包成 failover 错误（身份、豁免、呈现三件事统一），但不换号。
func TestKongDenyNonAccountScopedStopsFailover(t *testing.T) {
	for reason, wantRetry := range map[string]bool{
		KongDenyWindowClosed:                true,
		KongDenyPreparing:                   true,
		"model_undeterminable: duplicate":   false,
		"ensure_failed: dial tcp: refused":  false,
		"inject_failed: sjson invalid path": false,
	} {
		wrapped := newKongTicketDenyFailover(&KongErrTicketDenied{Reason: reason}, false)
		var failoverErr *UpstreamFailoverError
		if !errors.As(wrapped, &failoverErr) {
			t.Fatalf("%s: 取不到 failover 错误", reason)
		}
		if got := failoverErr.ShouldRetryNextAccount(); got != wantRetry {
			t.Errorf("%s 的换号判定 = %v, want %v", reason, got, wantRetry)
		}
		// 不换号也要被认成票据拒服：否则调度健康度与客户端呈现又会走回上游故障那一套。
		if !KongIsTicketDeniedFailover(wrapped) {
			t.Errorf("%s: 不换号的拒服也必须保留票据身份", reason)
		}
	}
}

// 账号在调度之后被删掉时，拒服必须是**账号级**的，好让请求换到别的账号。
//
// 报成 `ensure_failed`（系统级）会带上 NextAccountStop，于是"A 已不存在、B 持有合格票"时整个请求在
// A 上终止——那是无谓拒服。共享存储故障仍保留系统级语义：换一个账号同样读不到。
func TestKongPrepareUpstreamMissingAccountIsAccountScoped(t *testing.T) {
	const model = "gpt-6-astra"
	repo := newKongStubRepo()
	// 账号存储里**没有**这个账号：模拟调度拿到它之后它被删掉。
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	// 直接构造而不复用 kongBatchServiceWith：那个 helper 在 `unit` tag 的文件里，而本文件不带 tag。
	bank, bankErr := KongFingerprintBankLoad()
	if bankErr != nil {
		t.Fatalf("加载校准资料: %v", bankErr)
	}
	svc := NewKongTicketService(repo, &kongStubUpstream{}, accounts, bank, KongDefaultTicketParams(),
		[]string{model}, false, false,
		KongTicketAccept{model: []string{model}}, KongStg0Accept{}, 0.9)
	g := NewKongTicketGateway(svc, []string{model})

	// PrepareUpstream 手里还握着调度给的那个 Account 对象，所以准入判定照常开始。
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses",
		strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	_, prepErr := g.PrepareUpstream(context.Background(), req, account)
	var denied *KongErrTicketDenied
	if !errors.As(prepErr, &denied) {
		t.Fatalf("应当是票据拒服，实得 %v", prepErr)
	}
	if denied.Reason != KongDenyAccountUnready {
		t.Errorf("拒服原因 = %q, want %q", denied.Reason, KongDenyAccountUnready)
	}
	if !kongDenyIsAccountScoped(denied.Reason) {
		t.Error("账号已不存在是账号级条件，必须允许换号")
	}
	wrapped := newKongTicketDenyFailover(denied, kongDenyWorthSameAccountWait(denied.Reason))
	var failoverErr *UpstreamFailoverError
	if !errors.As(wrapped, &failoverErr) || !failoverErr.ShouldRetryNextAccount() {
		t.Error("账号已不存在时必须换号，否则别的账号有票也用不上")
	}
}
