//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 请求特征（逐请求的降智证据）。
//
// 这组的重点是**每一项都要对得上真实载体**：量错了不会报错，只会让排查照着一个不存在的事实走。
// 三条通路各有自己的载体，所以每条都要单独立住；nil 与 0 的区别也要钉住——0 是"带了个空串"。

func kongFeatureTestGateway(t *testing.T, mode KongTicketMode, state string) (*KongTicketGateway, *Account, *kongStubRepo) {
	t.Helper()
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, mode, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	repo.setCurrent(1, "gpt-6-astra", &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: state,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
		FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
		FingerprintProbs: kongTestAttr(0.99).Probs,
	})
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	return NewKongTicketGateway(svc, []string{"gpt-6-astra"}), account, repo
}

func TestKongRequestFeaturesRecordOutboundState(t *testing.T) {
	ticket := strings.Repeat("a", 292)

	t.Run("HTTP full：出站是我们的票，客户端那份单独留痕", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		clientOwn := strings.Repeat("z", 312)
		req.Header.Set(openAICodexTurnStateHeader, clientOwn)

		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil {
			t.Fatal("full 模式必须记下特征")
		}
		if f.StateLen == nil || *f.StateLen != len(ticket) || f.StateFP != kongStateFingerprint(ticket) {
			t.Errorf("出站那一份应当是注入的票：len=%v fp=%s", f.StateLen, f.StateFP)
		}
		// 客户端自己那张被我们替换掉了，它是"客户端手里是什么档"的唯一证据。
		if f.ClientStateLen == nil || *f.ClientStateLen != 312 || f.ClientStateFP != kongStateFingerprint(clientOwn) {
			t.Errorf("客户端自带的那一份没记对：len=%v fp=%s", f.ClientStateLen, f.ClientStateFP)
		}
		if f.TicketID == nil || *f.TicketID != 7 {
			t.Errorf("注入的票 id = %v, want 7", f.TicketID)
		}
		// 原值绝不能出现在特征里：state 是可注入的凭据。
		if strings.Contains(f.StateFP+f.ClientStateFP, ticket[:8]) {
			t.Error("指纹里出现了 state 的原文片段")
		}
	})

	t.Run("HTTP off：出站就是客户端自带的那一份，不重复记也没有票 id", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		clientOwn := strings.Repeat("z", 312)
		req.Header.Set(openAICodexTurnStateHeader, clientOwn)

		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("off 模式不该拒服: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 312 {
			t.Fatalf("off 模式应当记下客户端自带的那一份：%+v", f)
		}
		if f.ClientStateFP != "" || f.ClientStateLen != nil {
			t.Error("出站与客户端自带是同一份时不该记两遍——那看起来像发生过替换")
		}
		if f.TicketID != nil {
			t.Errorf("off 模式没有注入，不该有票 id：%v", *f.TicketID)
		}
	})

	t.Run("原生 WS：量的是注入之后的那一帧", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		payload := []byte(`{"type":"response.create","model":"gpt-6-astra"}`)
		_, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", payload)
		if err != nil || attempt == nil || attempt.Grant == nil {
			t.Fatalf("WS 准入应当注入了票：attempt=%v err=%v", attempt, err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != len(ticket) {
			t.Fatalf("出站长度没记对：%+v", f)
		}
		if f.TicketID == nil || *f.TicketID != 7 {
			t.Errorf("注入的票 id = %v, want 7", f.TicketID)
		}
	})

	t.Run("非门控模型不记特征", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-5.6-luna"}`)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("非门控不该报错: %v", err)
		}
		if attempt != nil {
			t.Errorf("非门控模型不该产生 attempt：%+v", attempt)
		}
	})
}

// 上游回发的 state 要连同它入库后的 id 一起记下。
//
// 入库了就给 id（凭它能查到这张票后来验成了什么）；按规则没入库（312 在长度黑名单里）就只有指纹
// 与长度——那时库里根本没有这张票，给个 id 就是假的。
func TestKongRequestFeaturesRecordReissuedState(t *testing.T) {
	ticket := strings.Repeat("a", 292)

	newResp := func(state string) *http.Response {
		resp := &http.Response{Header: http.Header{}}
		if state != "" {
			resp.Header.Set(openAICodexTurnStateHeader, state)
		}
		return resp
	}

	t.Run("回发的票入库，特征带上它的 id", func(t *testing.T) {
		g, account, repo := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		reissued := strings.Repeat("r", 292)
		if err := g.AfterUpstream(context.Background(), account, attempt, newResp(reissued)); err != nil {
			t.Fatalf("off 模式不做交付判定，不该报错: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.ReissuedLen == nil || *f.ReissuedLen != 292 || f.ReissuedFP != kongStateFingerprint(reissued) {
			t.Fatalf("回发的 state 没记对：%+v", f)
		}
		if f.ReissuedTicketID == nil {
			t.Fatal("入库成功时必须带上票 id")
		}
		if got := repo.ticketByID(*f.ReissuedTicketID); got == nil || got.State != reissued {
			t.Errorf("特征里的票 id 指向的不是那张回发的票：%+v", got)
		}
	})

	t.Run("回发的是降智档（长度黑名单）：有指纹长度，没有 id", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		denylisted := strings.Repeat("d", 312)
		if err := g.AfterUpstream(context.Background(), account, attempt, newResp(denylisted)); err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.ReissuedLen == nil || *f.ReissuedLen != 312 {
			t.Fatalf("黑名单长度也要如实记下：%+v", f)
		}
		if f.ReissuedTicketID != nil {
			t.Errorf("没入库就不该有 id：%v", *f.ReissuedTicketID)
		}
	})

	t.Run("上游没回发就不记这几项", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		if err := g.AfterUpstream(context.Background(), account, attempt, newResp("")); err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f != nil && (f.ReissuedFP != "" || f.ReissuedLen != nil || f.ReissuedTicketID != nil) {
			t.Errorf("没有回发时这几项必须缺省：%+v", f)
		}
	})
}

// 指纹与空值的边界：两者都直接决定页面上那一行说的是不是事实。
func TestKongStateFingerprintAndEmptySnapshot(t *testing.T) {
	a := kongStateFingerprint("state-a")
	if len(a) != kongStateFingerprintBytes*2 {
		t.Errorf("指纹长度 = %d, want %d", len(a), kongStateFingerprintBytes*2)
	}
	if a != kongStateFingerprint("state-a") {
		t.Error("同一个 state 的指纹必须稳定")
	}
	if a == kongStateFingerprint("state-b") {
		t.Error("不同 state 的指纹不该相同")
	}
	if kongStateFingerprint("") != "" {
		t.Error("空串没有指纹——它由长度 0 表达")
	}

	var nilRecorder *KongFeatureRecorder
	if nilRecorder.Snapshot() != nil {
		t.Error("没有记录器时快照必须是 nil")
	}
	if (&KongFeatureRecorder{}).Snapshot() != nil {
		t.Error("一项都没记时快照必须是 nil——写个空对象会让「没采到」看起来像「采了但都是空」")
	}
	// 两个载体都"不存在"时不该产出特征。
	r := &KongFeatureRecorder{}
	r.recordOutbound(kongStateObservation{}, kongStateObservation{}, 0)
	if r.Snapshot() != nil {
		t.Error("什么都没有时不该产出特征")
	}
}

// 空串与"没带"必须分开：前者是上游/客户端确实发了个空值。
//
// 这一条是对文档口径的兑现检查——注释与设计文档都写明 `state_len: 0` 表示"带了个空串"，如果三个
// 载体读取函数把"不存在"与"空串"合并，那个值永远产不出来，而页面上"这条请求没带 state"的说法就
// 成了对一次空值上送的错误陈述。
func TestKongRequestFeaturesDistinguishEmptyStateFromAbsent(t *testing.T) {
	t.Run("HTTP 头", func(t *testing.T) {
		req, err := http.NewRequest("POST", "https://example.com/v1/responses", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		if got := kongOutboundStateFromHeader(req.Header); got.present {
			t.Error("没带这个头时必须是不存在")
		}
		req.Header.Set(openAICodexTurnStateHeader, "")
		got := kongOutboundStateFromHeader(req.Header)
		if !got.present || got.value != "" {
			t.Errorf("带空串时应当是「存在且为空」，实得 %+v", got)
		}
	})

	t.Run("WS 帧", func(t *testing.T) {
		if got := kongOutboundStateFromWSFrame([]byte(`{"client_metadata":{}}`)); got.present {
			t.Error("client_metadata 里没有这个键时必须是不存在")
		}
		got := kongOutboundStateFromWSFrame([]byte(`{"client_metadata":{"x-codex-turn-state":""}}`))
		if !got.present || got.value != "" {
			t.Errorf("键存在且值为空串时应当是「存在且为空」，实得 %+v", got)
		}
	})

	t.Run("WS map", func(t *testing.T) {
		if got := kongOutboundStateFromWSMap(map[string]any{"client_metadata": map[string]any{}}); got.present {
			t.Error("键不存在时必须是不存在")
		}
		got := kongOutboundStateFromWSMap(map[string]any{
			"client_metadata": map[string]any{kongWSTurnStateMetadataKey: ""},
		})
		if !got.present || got.value != "" {
			t.Errorf("键存在且值为空串时应当是「存在且为空」，实得 %+v", got)
		}
		// 值不是字符串时上游读不出票，等同没带——不能当成空串。
		if got := kongOutboundStateFromWSMap(map[string]any{
			"client_metadata": map[string]any{kongWSTurnStateMetadataKey: 42},
		}); got.present {
			t.Error("值不是字符串时应当是不存在")
		}
	})

	t.Run("空串上送记成长度 0", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.recordOutbound(kongStateObservation{}, kongObservedState(""), 0)
		f := r.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 0 {
			t.Fatalf("带空串上送应当记成长度 0，实得 %+v", f)
		}
		if f.StateFP != "" {
			t.Errorf("空串没有指纹，实得 %q", f.StateFP)
		}
	})

	t.Run("客户端带空串、被我们替换掉：两份都要留痕", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.recordOutbound(kongObservedState(""), kongObservedState("ticket"), 7)
		f := r.Snapshot()
		if f == nil || f.ClientStateLen == nil || *f.ClientStateLen != 0 {
			t.Fatalf("客户端那份空串要记成长度 0，实得 %+v", f)
		}
		if f.StateLen == nil || *f.StateLen != len("ticket") {
			t.Errorf("出站长度没记对：%+v", f.StateLen)
		}
	})

	t.Run("HTTP full 模式下客户端带空串会被记下来", func(t *testing.T) {
		ticket := strings.Repeat("a", 292)
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		req.Header.Set(openAICodexTurnStateHeader, "")
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.ClientStateLen == nil || *f.ClientStateLen != 0 {
			t.Fatalf("客户端那个空串头应当留下长度 0，实得 %+v", f)
		}
	})
}

// 一轮里先收到能入库的票、再收到被拒收的票时，回发这一组三项必须整体替换。
//
// 只写 id 不清旧值会让新的指纹与长度配上上一张票的 id——那是一条指向别的票的假线索，比缺 id 严重。
func TestKongRequestFeaturesReissuedReplacesAsGroup(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
	req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
	attempt, err := g.PrepareUpstream(context.Background(), req, account)
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}

	stored := strings.Repeat("r", 292)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set(openAICodexTurnStateHeader, stored)
	if err := g.AfterUpstream(context.Background(), account, attempt, resp); err != nil {
		t.Fatalf("off 模式不该报错: %v", err)
	}
	first := attempt.Features.Snapshot()
	if first == nil || first.ReissuedTicketID == nil {
		t.Fatalf("第一张回发票入库后应当带 id：%+v", first)
	}
	if got := repo.ticketByID(*first.ReissuedTicketID); got == nil || got.State != stored {
		t.Fatalf("id 指向的不是第一张回发票：%+v", got)
	}

	// 第二张是降智档，按长度黑名单拒收——库里没有它。
	denylisted := strings.Repeat("d", 312)
	resp2 := &http.Response{Header: http.Header{}}
	resp2.Header.Set(openAICodexTurnStateHeader, denylisted)
	if err := g.AfterUpstream(context.Background(), account, attempt, resp2); err != nil {
		t.Fatalf("off 模式不该报错: %v", err)
	}
	second := attempt.Features.Snapshot()
	if second == nil || second.ReissuedLen == nil || *second.ReissuedLen != 312 {
		t.Fatalf("第二张回发票的长度没更新：%+v", second)
	}
	if second.ReissuedFP != kongStateFingerprint(denylisted) {
		t.Errorf("指纹没更新到第二张：%q", second.ReissuedFP)
	}
	if second.ReissuedTicketID != nil {
		t.Errorf("第二张没入库，旧 id 必须清掉，实得 %d", *second.ReissuedTicketID)
	}
	// 先前取过的快照是副本，不受后续更新影响。
	if first.ReissuedLen == nil || *first.ReissuedLen != 292 {
		t.Error("已取出的快照被后续更新改动了")
	}
}

// 客户端断连之后，上游回发的 state 仍要被收走并记进特征——那条用量行照常结算，缺了它就没有证据。
//
// 这条覆盖的是"只收票不判定"的入口：断连后没有业务输出会送出，交付判定无从谈起，也不该凭空产出
// 一条拒服错误。
func TestKongObserveWSDownstreamCollectsWithoutJudging(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	payload := []byte(`{"type":"response.create","model":"gpt-6-astra"}`)
	_, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", payload)
	if err != nil || attempt == nil || attempt.Grant == nil {
		t.Fatalf("WS 准入应当注入了票：attempt=%v err=%v", attempt, err)
	}

	reissued := strings.Repeat("r", 292)
	event := []byte(`{"type":"response.metadata","response":{"headers":{"x-codex-turn-state":"` + reissued + `"}}}`)
	// 注意：这里刻意**不**调 GuardWSDownstream。断连路径上只该收票。
	g.ObserveWSDownstream(context.Background(), account, attempt, event)

	f := attempt.Features.Snapshot()
	if f == nil || f.ReissuedLen == nil || *f.ReissuedLen != 292 || f.ReissuedFP != kongStateFingerprint(reissued) {
		t.Fatalf("断连后回发的 state 没记下：%+v", f)
	}
	if f.ReissuedTicketID == nil {
		t.Fatal("回发票入库后要带 id")
	}
	if got := repo.ticketByID(*f.ReissuedTicketID); got == nil || got.State != reissued {
		t.Errorf("id 指向的不是那张回发票：%+v", got)
	}
	// 非 metadata 事件不该被当成回发。
	before := attempt.Features.Snapshot().ReissuedFP
	g.ObserveWSDownstream(context.Background(), account, attempt, []byte(`{"type":"response.completed"}`))
	if attempt.Features.Snapshot().ReissuedFP != before {
		t.Error("非 response.metadata 事件不该改动回发特征")
	}
}

// HTTP→WS 那条路上，注入**不得改动调用方那份请求体**。
//
// 调用方（forwardOpenAIWSV2）用 buildOpenAIWSCreatePayload 只做顶层浅拷贝，`client_metadata` 与
// 原始请求体是同一个对象，而 WS 重试循环复用那份请求体。就地写会让第二次准入把上一次注入的票
// 当成"客户端自带票"（换账号重试时更糟：上一个账号的票会跟着发给下一个账号）。
func TestKongPrepareWSMapPayloadDoesNotMutateCallerMetadata(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	clientOwn := strings.Repeat("z", 312)

	// 调用方的请求体：重试循环里复用的就是它。
	reqBody := map[string]any{
		"model":           "gpt-6-astra",
		"client_metadata": map[string]any{kongWSTurnStateMetadataKey: clientOwn, "session_id": "s"},
	}
	// 照 buildOpenAIWSCreatePayload 的做法：只拷顶层。
	shallow := func() map[string]any {
		out := make(map[string]any, len(reqBody))
		for k, v := range reqBody {
			out[k] = v
		}
		return out
	}

	for attempt := 1; attempt <= 2; attempt++ {
		payload := shallow()
		got, err := g.PrepareWSMapPayload(context.Background(), account, "gpt-6-astra", payload)
		if err != nil || got == nil || got.Grant == nil {
			t.Fatalf("第 %d 次准入应当注入了票：attempt=%v err=%v", attempt, got, err)
		}
		f := got.FeatureSnapshot()
		if f == nil || f.ClientStateLen == nil || *f.ClientStateLen != 312 ||
			f.ClientStateFP != kongStateFingerprint(clientOwn) {
			t.Fatalf("第 %d 次准入读到的客户端自带票不对：%+v", attempt, f)
		}
		if f.StateLen == nil || *f.StateLen != len(ticket) {
			t.Fatalf("第 %d 次出站长度不对：%+v", attempt, f.StateLen)
		}
		// 上送的那份必须带我们的票。
		if state := kongOutboundStateFromWSMap(payload); !state.present || state.value != ticket {
			t.Fatalf("第 %d 次上送的 payload 里没有我们的票：%+v", attempt, state)
		}
		// 调用方那份请求体必须原样不动。
		meta := reqBody["client_metadata"].(map[string]any)
		if meta[kongWSTurnStateMetadataKey] != clientOwn {
			t.Fatalf("第 %d 次准入改动了调用方的请求体：%v", attempt, meta[kongWSTurnStateMetadataKey])
		}
		if meta["session_id"] != "s" {
			t.Errorf("第 %d 次准入弄丢了 client_metadata 里的其它键", attempt)
		}
	}
}
