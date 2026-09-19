package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
