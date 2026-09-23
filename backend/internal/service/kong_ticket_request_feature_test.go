//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

	// 非门控模型即使账号开着 full 也只观测：不注入、不拦截，照记收发两侧的 state 并收票。
	t.Run("非门控模型只观测", func(t *testing.T) {
		g, account, repo := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-sol"}`)
		clientOwn := strings.Repeat("z", 780)
		req.Header.Set(openAICodexTurnStateHeader, clientOwn)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("非门控不该报错: %v", err)
		}
		if attempt == nil || attempt.Grant != nil || attempt.Model != "gpt-6-sol" {
			t.Fatalf("非门控模型应当产生不带票的 attempt：%+v", attempt)
		}
		if got := req.Header.Get(openAICodexTurnStateHeader); got != clientOwn {
			t.Error("非门控模型的请求头被改写了")
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 780 || f.StateFP != kongStateFingerprint(clientOwn) {
			t.Fatalf("客户端回传的 state 没记下：%+v", f)
		}
		if f.ClientStateLen != nil || f.TicketID != nil {
			t.Errorf("没有注入，不该有客户端单列或票 id：%+v", f)
		}

		reissued := strings.Repeat("r", 780)
		resp := &http.Response{Header: http.Header{}}
		resp.Header.Set(openAICodexTurnStateHeader, reissued)
		if err := g.AfterUpstream(context.Background(), account, attempt, resp); err != nil {
			t.Fatalf("非门控模型上游回发 state 不该拦截: %v", err)
		}
		f = attempt.Features.Snapshot()
		if f.ReissuedLen == nil || *f.ReissuedLen != 780 || f.ReissuedTicketID == nil {
			t.Errorf("上游回发的 state 应当记下并入库：%+v", f)
		}
		if n := kongCountTickets(repo, 1, "gpt-6-sol"); n != 1 {
			t.Errorf("非门控模型收到的票应当按它自己的模型名入库，实际 %d 张", n)
		}
	})
}

// 上游回发的 state 要连同它入库后的 id 一起记下。
//
// 入库了就给 id（凭它能查到这张票后来验成了什么）；入库失败就只有指纹与长度——那时库里根本
// 没有这张票，给个 id 就是假的。
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

	t.Run("回发的票入库失败：有指纹长度，没有 id", func(t *testing.T) {
		g, account, repo := kongFeatureTestGateway(t, KongTicketModeOff, ticket)
		req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)
		attempt, err := g.PrepareUpstream(context.Background(), req, account)
		if err != nil {
			t.Fatalf("准入失败: %v", err)
		}
		repo.insertTicketErr = errors.New("票表写不进去")
		unstored := strings.Repeat("d", 780)
		if err := g.AfterUpstream(context.Background(), account, attempt, newResp(unstored)); err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		f := attempt.Features.Snapshot()
		if f == nil || f.ReissuedLen == nil || *f.ReissuedLen != 780 || f.ReissuedFP != kongStateFingerprint(unstored) {
			t.Fatalf("入库失败时指纹与长度也要如实记下：%+v", f)
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

// 一轮里先收到能入库的票、再收到一张入库失败的票时，回发这一组三项必须整体替换。
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

	// 第二张入库失败——库里没有它。
	repo.insertTicketErr = errors.New("票表写不进去")
	unstored := strings.Repeat("d", 780)
	resp2 := &http.Response{Header: http.Header{}}
	resp2.Header.Set(openAICodexTurnStateHeader, unstored)
	if err := g.AfterUpstream(context.Background(), account, attempt, resp2); err != nil {
		t.Fatalf("off 模式不该报错: %v", err)
	}
	second := attempt.Features.Snapshot()
	if second == nil || second.ReissuedLen == nil || *second.ReissuedLen != 780 {
		t.Fatalf("第二张回发票的长度没更新：%+v", second)
	}
	if second.ReissuedFP != kongStateFingerprint(unstored) {
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
// 原生 WS 上游发的载体事件带 codex. 前缀。只认不带前缀的拼写，WS 这条路一张票也收不到——
// 票就在帧里，三处调用点却都静默 return，收票、交付判定、溯源登记同时失效且无任何信号。
func TestKongObserveWSDownstreamAcceptsCodexPrefixedEvent(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	payload := []byte(`{"type":"response.create","model":"gpt-6-astra"}`)
	_, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", payload)
	if err != nil || attempt == nil {
		t.Fatalf("WS 准入失败：attempt=%v err=%v", attempt, err)
	}

	reissued := strings.Repeat("r", 292)
	event := []byte(`{"type":"codex.response.metadata","headers":{` +
		`"x-codex-safety-buffering-enabled":"true",` +
		`"x-codex-turn-state":"` + reissued + `","x-models-etag":"W/\"abc\""}}`)
	g.ObserveWSDownstream(context.Background(), account, attempt, event)

	f := attempt.Features.Snapshot()
	if f == nil || f.ReissuedFP != kongStateFingerprint(reissued) {
		t.Fatalf("带前缀的载体事件没被收：%+v", f)
	}
	if f.ReissuedTicketID == nil {
		t.Fatal("回发票应入库并带 id")
	}
	if got := repo.ticketByID(*f.ReissuedTicketID); got == nil || got.State != reissued {
		t.Errorf("id 指向的不是那张回发票：%+v", got)
	}
}

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

// 出站请求对象是 HTTP 那条路的特征载体：准入时把记录器挂上去，结果构造时从它取。
//
// 这一对函数是 Responses 直通/转换两条路共用的唯一通道，取不回来就等于那条路的用量行永远没特征
// ——上线后曾整天全空，靠的是翻库才发现。
func TestKongFeaturesTravelOnOutboundRequest(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	req := kongTestGatedRequest(t, `{"model":"gpt-6-astra"}`)

	attempt, err := g.PrepareUpstream(context.Background(), req, account)
	if err != nil {
		t.Fatalf("准入失败: %v", err)
	}
	// 挂之前取不到：这正是"这条通路没接采集"时的表现。
	if got := KongFeaturesFromRequest(req); got != nil {
		t.Fatalf("还没挂就取到了特征：%+v", got)
	}
	// doOpenAIUpstream 就地换 context 的等价动作。
	*req = *req.WithContext(kongWithFeatures(req.Context(), attempt.Features))

	got := KongFeaturesFromRequest(req)
	if got == nil || got.StateLen == nil || *got.StateLen != len(ticket) {
		t.Fatalf("从出站请求取回的特征不对：%+v", got)
	}
	if got.TicketID == nil || *got.TicketID != 7 {
		t.Errorf("注入的票 id = %v, want 7", got.TicketID)
	}

	// 交付判定之后再取，要能看到上游回发的那一份（同一个记录器，不是快照）。
	resp := &http.Response{Header: http.Header{}}
	reissued := strings.Repeat("r", 292)
	resp.Header.Set(openAICodexTurnStateHeader, reissued)
	_ = g.AfterUpstream(context.Background(), account, attempt, resp)
	if after := KongFeaturesFromRequest(req); after == nil || after.ReissuedLen == nil || *after.ReissuedLen != 292 {
		t.Fatalf("交付判定后应当能取到回发的 state：%+v", after)
	}

	if KongFeaturesFromRequest(nil) != nil {
		t.Error("没有请求对象时应当是 nil")
	}
}

// 原生 WS 的 ctx_pool / passthrough 通路上，客户端的 state **不在帧里**：它由客户端放在 WebSocket
// upgrade 的请求头上，再由本服务放到与上游那条连接的握手头上。逐帧去量必然量不到——kong.11 上线后
// ctx_pool 的特征全空正是这个原因，靠 WarnMissingRequestFeatures 那条守卫喊出来才发现。
func TestKongRecordOutboundIfAbsentCoversConnectionLevelState(t *testing.T) {
	connState := strings.Repeat("c", 292)

	t.Run("帧里没有就取连接级那一个", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.recordOutbound(kongStateObservation{}, kongStateObservation{}, 0)
		if r.Snapshot() != nil {
			t.Fatal("帧里没有 state 时本该什么都没记")
		}
		r.RecordOutboundIfAbsent(connState)
		f := r.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 292 || f.StateFP != kongStateFingerprint(connState) {
			t.Fatalf("连接级 state 没补上：%+v", f)
		}
	})

	t.Run("帧里有值就不许被连接级盖掉", func(t *testing.T) {
		// 我们注入的票写在帧内 client_metadata，那才是本轮生效的那一个。
		ticket := strings.Repeat("a", 292)
		r := &KongFeatureRecorder{}
		r.recordOutbound(kongStateObservation{}, kongObservedState(ticket), 7)
		r.RecordOutboundIfAbsent(connState)
		f := r.Snapshot()
		if f.StateFP != kongStateFingerprint(ticket) {
			t.Errorf("帧内的值被连接级覆盖了：%q", f.StateFP)
		}
	})

	t.Run("带空串上送也算已记，连接级不再补", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.recordOutbound(kongStateObservation{}, kongObservedState(""), 0)
		r.RecordOutboundIfAbsent(connState)
		f := r.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 0 {
			t.Fatalf("空串上送应当保持长度 0，实得 %+v", f)
		}
	})

	t.Run("空值与 nil 接收者都不炸", func(t *testing.T) {
		var nilRecorder *KongFeatureRecorder
		nilRecorder.RecordOutboundIfAbsent(connState)
		var nilAttempt *KongUpstreamAttempt
		nilAttempt.RecordOutboundIfAbsentOnFeatures(connState)
		r := &KongFeatureRecorder{}
		r.RecordOutboundIfAbsent("")
		if r.Snapshot() != nil {
			t.Error("空 state 不该记出东西")
		}
	})
}

// 原生 WS 的握手响应头里上游也会给一张 state，它同样要进票缓存。
//
// 这条覆盖三个口径：收进来的票能查到；非门控模型一张都不许收（否则会替一个我们并不判档的模型攒
// 票）；空 state 不收。
func TestKongObserveHandshakeStateStoresTicket(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongFeatureTestGateway(t, KongTicketModeObserve, ticket)

	handshake := strings.Repeat("h", 292)
	g.ObserveHandshakeState(context.Background(), account, "gpt-6-astra", handshake)

	found := false
	for _, tk := range repo.poolOf(1, "gpt-6-astra") {
		if tk.State == handshake {
			found = true
		}
	}
	if !found {
		t.Fatal("握手响应头里的 state 没进票缓存")
	}

	t.Run("非门控模型不收票", func(t *testing.T) {
		g.ObserveHandshakeState(context.Background(), account, "gpt-4o", strings.Repeat("x", 292))
		if got := repo.poolOf(1, "gpt-4o"); len(got) != 0 {
			t.Errorf("非门控模型的票池必须是空的，实得 %d 张", len(got))
		}
	})

	t.Run("空 state 不收票", func(t *testing.T) {
		before := len(repo.poolOf(1, "gpt-6-astra"))
		g.ObserveHandshakeState(context.Background(), account, "gpt-6-astra", "   ")
		if after := len(repo.poolOf(1, "gpt-6-astra")); after != before {
			t.Errorf("空 state 不该收票：%d → %d", before, after)
		}
	})
}

// 消费握手票是一次性的，所以"该不该消费"必须先判门控。
//
// 非门控轮若先去消费再由 ObserveHandshakeState 丢弃，票就白白没了——同一条物理连接完全可以接着服务
// 门控模型（连接兼容键里没有模型），那一轮再想收就收不到。三条 WS 通路都用这个判据守在消费之前。
func TestKongShouldObserveHandshakeStateGatesBeforeConsuming(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve, KongTicketModeFull} {
		g, _, _ := kongFeatureTestGateway(t, mode, ticket)
		// off / observe 也要收票（样本靠它攒），所以判据只看门控模型、不看模式。
		if !g.ShouldObserveHandshakeState("gpt-6-astra") {
			t.Errorf("mode=%s：门控模型必须允许消费握手票", mode)
		}
		if g.ShouldObserveHandshakeState("gpt-4o") {
			t.Errorf("mode=%s：非门控模型不许消费那张一次性的票", mode)
		}
		if g.ShouldObserveHandshakeState("  ") {
			t.Errorf("mode=%s：空模型不许消费", mode)
		}
	}
	var nilGateway *KongTicketGateway
	if nilGateway.ShouldObserveHandshakeState("gpt-6-astra") {
		t.Error("网关未启用时不该去消费")
	}
}

// 握手那张票**不许**被记成"本轮上游又下发了一张"。
//
// `reissued_*` 是交付判定的依据（上游本轮回发了什么档），而握手发生在任何一轮之前。用真实的准入 +
// 收票链路走一遍，而不是另起一个 recorder——后者证明不了调用点没污染这三个字段。
func TestKongHandshakeStateDoesNotTouchReissuedFeatures(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
	payload := []byte(`{"type":"response.create","model":"gpt-6-astra"}`)
	_, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", payload)
	if err != nil || attempt == nil {
		t.Fatalf("WS 准入失败：attempt=%v err=%v", attempt, err)
	}

	handshake := strings.Repeat("h", 292)
	g.ObserveHandshakeState(context.Background(), account, "gpt-6-astra", handshake)
	attempt.RecordOutboundIfAbsentOnFeatures(handshake)

	f := attempt.Features.Snapshot()
	if f == nil {
		t.Fatal("full 模式必须有特征")
	}
	if f.ReissuedFP != "" || f.ReissuedLen != nil || f.ReissuedTicketID != nil {
		t.Errorf("握手票不该出现在 reissued_*：%+v", f)
	}
	// 出站那一份是注入的票（帧内有值），连接级不能盖掉它。
	if f.StateFP != kongStateFingerprint(ticket) {
		t.Errorf("出站 state 被连接级的值盖掉了：%s", f.StateFP)
	}
}

// 连接级的客户端 state：注入替换掉它时必须留痕，没替换时不留。
func TestKongRecordClientStateIfAbsent(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	clientOwn := strings.Repeat("z", 312)

	t.Run("被票替换掉时记下客户端那份", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.RecordOutboundIfAbsent(ticket)
		rec.RecordClientStateIfAbsent(clientOwn)
		f := rec.Snapshot()
		if f == nil || f.ClientStateLen == nil || *f.ClientStateLen != 312 || f.ClientStateFP != kongStateFingerprint(clientOwn) {
			t.Fatalf("客户端那份没记对：%+v", f)
		}
	})

	t.Run("与出站相同则不记", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.RecordOutboundIfAbsent(clientOwn)
		rec.RecordClientStateIfAbsent(clientOwn)
		if f := rec.Snapshot(); f == nil || f.ClientStateFP != "" || f.ClientStateLen != nil {
			t.Errorf("没发生替换却记了客户端那份：%+v", f)
		}
	})

	t.Run("本轮压根没有出站 state 时不记", func(t *testing.T) {
		// off 模式 + 复用连接就是这种形态：没有任何 state 上送，单独摆一个 client_state 出来，
		// 管理页读起来就是"发生过一次并不存在的注入替换"。
		rec := &KongFeatureRecorder{}
		rec.RecordClientStateIfAbsent(clientOwn)
		if f := rec.Snapshot(); f != nil {
			t.Errorf("无出站 state 不该产出任何特征：%+v", f)
		}
	})

	t.Run("出站是空串（带了个空 state）时照常记", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.recordOutbound(kongStateObservation{}, kongObservedState(""), 0)
		rec.RecordClientStateIfAbsent(clientOwn)
		f := rec.Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 0 {
			t.Fatalf("空串出站要能表达成 state_len=0：%+v", f)
		}
		if f.ClientStateFP != kongStateFingerprint(clientOwn) {
			t.Errorf("出站为空串≠没有出站，客户端那份该记：%+v", f)
		}
	})

	t.Run("帧内已有客户端 state 且原样上送时不许补记连接头", func(t *testing.T) {
		// 这是最容易误判的一种：off 模式原样上送帧内那张，recordOutbound 因"与出站相同"刻意不记
		// 客户端那份；此时若把"刻意没记"当成"还没看过"，就会拿连接头补出一次并不存在的替换。
		frameState := strings.Repeat("b", 292)
		connHeaderState := strings.Repeat("c", 292)
		rec := &KongFeatureRecorder{}
		rec.recordOutbound(kongObservedState(frameState), kongObservedState(frameState), 0)
		rec.RecordClientStateIfAbsent(connHeaderState)
		f := rec.Snapshot()
		if f == nil || f.StateFP != kongStateFingerprint(frameState) {
			t.Fatalf("出站应当是帧内那张：%+v", f)
		}
		if f.ClientStateFP != "" || f.ClientStateLen != nil {
			t.Errorf("原样上送就是没发生替换，不该补出 client_state：%+v", f)
		}
	})

	t.Run("准入已经记过就不覆盖", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		first := strings.Repeat("b", 292)
		rec.recordOutbound(kongObservedState(first), kongObservedState(ticket), 0)
		rec.RecordClientStateIfAbsent(clientOwn)
		f := rec.Snapshot()
		if f == nil || f.ClientStateFP != kongStateFingerprint(first) {
			t.Errorf("准入记下的客户端那份被覆盖了：%+v", f)
		}
	})
}

// 两个 nil-safe 转发既要容忍 nil attempt，也要真的转发下去。
func TestKongAttemptFeatureWrappersDelegate(t *testing.T) {
	var nilAttempt *KongUpstreamAttempt
	nilAttempt.RecordOutboundIfAbsentOnFeatures("x")
	nilAttempt.RecordClientStateIfAbsentOnFeatures("y")
	if nilAttempt.FeatureSnapshot() != nil {
		t.Error("nil attempt 不该产出快照")
	}

	ticket := strings.Repeat("a", 292)
	clientOwn := strings.Repeat("z", 312)
	attempt := &KongUpstreamAttempt{Model: "gpt-6-astra", Features: &KongFeatureRecorder{}}
	attempt.RecordOutboundIfAbsentOnFeatures(ticket)
	attempt.RecordClientStateIfAbsentOnFeatures(clientOwn)
	f := attempt.FeatureSnapshot()
	if f == nil || f.StateFP != kongStateFingerprint(ticket) {
		t.Fatalf("出站没转发下去：%+v", f)
	}
	if f.ClientStateFP != kongStateFingerprint(clientOwn) {
		t.Errorf("客户端那份没转发下去：%+v", f)
	}
}

// kongCtxAwareRepo 让 InsertTicket 尊重 context 取消——真实仓储走 QueryRowContext，ctx 一取消就落不了库。
// stub 默认忽略 ctx，所以"断连后还能不能入库"这件事只有换掉它才测得出来。
type kongCtxAwareRepo struct {
	*kongStubRepo
}

func (r *kongCtxAwareRepo) InsertTicket(ctx context.Context, t *KongTicket) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return r.kongStubRepo.InsertTicket(ctx, t)
}

// 客户端断连后的收票**不能**跟着请求 ctx 一起死。
//
// 三条 WS 通路都是在客户端已经走掉之后才走到 ObserveWSDownstream，那时请求 ctx 往往已经取消。若把它
// 直接传下去，票就落不了库：指纹和长度照样记下，`reissued_ticket_id` 却空着，observe 的诊断也起不来。
func kongCtxAwareTestGateway(t *testing.T, ticket string) (*KongTicketGateway, *Account, *kongStubRepo) {
	t.Helper()
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	repo.setCurrent(1, "gpt-6-astra", &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: ticket,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
		FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
		FingerprintProbs: kongTestAttr(0.99).Probs,
	})
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	svc := NewKongTicketService(
		&kongCtxAwareRepo{kongStubRepo: repo},
		&kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}},
		accounts, bank, KongDefaultTicketParams(),
		[]string{"gpt-6-astra"}, false, false,
		KongTicketAccept{"gpt-6-astra": []string{"gpt-6-astra"}}, KongStg0Accept{}, 0.9,
	)
	return NewKongTicketGateway(svc, []string{"gpt-6-astra"}), account, repo
}

func TestKongObserveWSDownstreamStoresTicketAfterRequestCancel(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongCtxAwareTestGateway(t, ticket)

	ctx, cancel := context.WithCancel(context.Background())
	_, attempt, err := g.PrepareWSTurn(ctx, account, "gpt-6-astra", []byte(`{"type":"response.create","model":"gpt-6-astra"}`))
	if err != nil || attempt == nil {
		t.Fatalf("WS 准入失败：attempt=%v err=%v", attempt, err)
	}
	// 客户端走了：请求 ctx 取消，上游仍在发，用量行照常结算。
	cancel()

	reissued := strings.Repeat("r", 292)
	event := []byte(`{"type":"response.metadata","response":{"headers":{"x-codex-turn-state":"` + reissued + `"}}}`)
	g.ObserveWSDownstream(ctx, account, attempt, event)

	f := attempt.Features.Snapshot()
	if f == nil || f.ReissuedFP != kongStateFingerprint(reissued) {
		t.Fatalf("回发的 state 没记下：%+v", f)
	}
	if f.ReissuedTicketID == nil {
		t.Fatal("请求 ctx 已取消，票仍必须入库并带回 id")
	}
	if got := repo.ticketByID(*f.ReissuedTicketID); got == nil || got.State != reissued {
		t.Errorf("id 指向的不是那张回发票：%+v", got)
	}
}

// 交付判定那条路上的收票同样不能跟着请求 ctx 一起死。
//
// 判定本身就是一次同步落库，客户端完全可以正好在这期间取消请求。隔离下沉到两条入口共用的
// observeWSReissued 里，正是为了不让任何一条入口漏掉它——也免得事后补收一次（补收会让同一帧产出第二条
// 观察事件、多一条 duplicate_state，还可能在第二次落库失败时把首次拿到的 id 清空）。
func TestKongGuardWSDownstreamStoresTicketAfterRequestCancel(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	g, account, repo := kongCtxAwareTestGateway(t, ticket)

	ctx, cancel := context.WithCancel(context.Background())
	_, attempt, err := g.PrepareWSTurn(ctx, account, "gpt-6-astra", []byte(`{"type":"response.create","model":"gpt-6-astra"}`))
	if err != nil || attempt == nil || attempt.Grant == nil {
		t.Fatalf("WS 准入应当注入了票：attempt=%v err=%v", attempt, err)
	}
	// 客户端在判定期间走掉。
	cancel()

	reissued := strings.Repeat("r", 292)
	event := []byte(`{"type":"response.metadata","response":{"headers":{"x-codex-turn-state":"` + reissued + `"}}}`)
	// 客户端还"在线"（调用方尚未察觉断连），所以走的是判定那条路：它照常拒交付。
	if guardErr := g.GuardWSDownstream(ctx, account, attempt, event); !KongIsDeliveryBlocked(guardErr) {
		t.Fatalf("上游回发了 state，判定必须拒交付，实得 %v", guardErr)
	}

	f := attempt.Features.Snapshot()
	if f == nil || f.ReissuedFP != kongStateFingerprint(reissued) {
		t.Fatalf("回发的 state 没记下：%+v", f)
	}
	if f.ReissuedTicketID == nil {
		t.Fatal("请求 ctx 已取消，判定路径上的票仍必须入库并带回 id")
	}
	if got := repo.ticketByID(*f.ReissuedTicketID); got == nil || got.State != reissued {
		t.Errorf("id 指向的不是那张回发票：%+v", got)
	}
}

// 排空发起方给的**绝对截止时刻**必须穿过脱钩活下来。
//
// `context.WithoutCancel` 会把父 context 的 deadline 一并丢掉。只要落库这一步重新套一个完整的单次上限，
// 排空里的多帧就各拿一次完整预算，累计起来能把 relay 的排空窗口耗光——窗口一到就取消上游并等这条
// reader 返回，排在后面的终帧连同 usage 一起没了。所以两者取更早的那个。
func TestKongPersistCtxKeepsCallerDeadline(t *testing.T) {
	t.Run("调用方的截止时刻更早时用它", func(t *testing.T) {
		caller, cancelCaller := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancelCaller()
		ctx, cancel := kongPersistCtx(caller)
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("落库 ctx 必须有期限")
		}
		if remaining := time.Until(deadline); remaining > 100*time.Millisecond {
			t.Errorf("调用方给的更早截止时刻被丢掉了，剩余 %v", remaining)
		}
	})

	t.Run("调用方没给或更晚时用单次上限", func(t *testing.T) {
		ctx, cancel := kongPersistCtx(context.Background())
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("落库 ctx 必须有期限")
		}
		if remaining := time.Until(deadline); remaining > kongObserveDetachedTimeout+50*time.Millisecond {
			t.Errorf("单次上限没生效，剩余 %v", remaining)
		}
	})

	t.Run("父 context 取消不影响落库", func(t *testing.T) {
		caller, cancelCaller := context.WithCancel(context.Background())
		ctx, cancel := kongPersistCtx(caller)
		defer cancel()
		cancelCaller()
		if err := ctx.Err(); err != nil {
			t.Errorf("客户端取消请求不该把落库一起掐掉：%v", err)
		}
	})
}

// 被剥掉的客户端原值必须活到出站那一项补齐为止。
//
// 跨账号剥离发生在票据准入之前，出站那一项却常常是结算时才由连接级补上的。若在剥离那一刻按"还没有
// 出站"直接丢掉被剥值，等出站补上时 clientObserved 又会把补记挡住——这条证据就永久没了，管理页从此
// 看不到"客户端手里那张是什么档"。
func TestKongStrippedClientStateSurvivesLateOutbound(t *testing.T) {
	clientOwn := strings.Repeat("a", 312)
	handshakeSent := strings.Repeat("b", 292)

	t.Run("先剥客户端那份，出站稍后才补上", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.RecordStrippedClientState(clientOwn)
		// 此刻还没有出站 state，不该摆一个会被读成"发生过替换"的孤值。
		require.Nil(t, rec.Snapshot(), "只有客户端那份时不产出特征")

		rec.RecordOutboundIfAbsent(handshakeSent)
		f := rec.Snapshot()
		require.NotNil(t, f)
		require.Equal(t, kongStateFingerprint(handshakeSent), f.StateFP)
		require.Equal(t, kongStateFingerprint(clientOwn), f.ClientStateFP, "被剥的客户端原值必须还在")
		require.NotNil(t, f.ClientStateLen)
		require.Equal(t, 312, *f.ClientStateLen)
	})

	t.Run("出站已在时立刻落定", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.RecordOutboundIfAbsent(handshakeSent)
		rec.RecordStrippedClientState(clientOwn)
		f := rec.Snapshot()
		require.NotNil(t, f)
		require.Equal(t, kongStateFingerprint(clientOwn), f.ClientStateFP)
	})

	t.Run("剥掉的与出站相同则不记", func(t *testing.T) {
		rec := &KongFeatureRecorder{}
		rec.RecordStrippedClientState(handshakeSent)
		rec.RecordOutboundIfAbsent(handshakeSent)
		f := rec.Snapshot()
		require.NotNil(t, f)
		require.Empty(t, f.ClientStateFP, "与出站相同就是同一件事，记两遍会被读成发生过替换")
	})

	t.Run("已观测之后，连接级的旧头不得顶替它", func(t *testing.T) {
		staleHeader := strings.Repeat("c", 292)
		rec := &KongFeatureRecorder{}
		rec.RecordStrippedClientState(clientOwn)
		rec.RecordOutboundIfAbsent(handshakeSent)
		rec.RecordClientStateIfAbsent(staleHeader)
		f := rec.Snapshot()
		require.NotNil(t, f)
		require.Equal(t, kongStateFingerprint(clientOwn), f.ClientStateFP, "帧内被剥的那份才是本轮的客户端值")
	})
}

// WS 的两条入口对非门控模型同样只观测：帧原样放行、不注入，回发的 state 照记不拦。
func TestKongNonGatedModelObservedOverWS(t *testing.T) {
	ticket := strings.Repeat("a", 292)
	clientOwn := strings.Repeat("z", 780)
	reissued := strings.Repeat("r", 780)
	metadata := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + reissued + `"}}`)

	t.Run("原生 WS 帧", func(t *testing.T) {
		g, account, repo := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		payload := []byte(`{"type":"response.create","model":"gpt-6-sol","client_metadata":{"x-codex-turn-state":"` + clientOwn + `"}}`)
		out, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-sol", payload)
		if err != nil {
			t.Fatalf("非门控不该报错: %v", err)
		}
		if attempt == nil || attempt.Grant != nil || string(out) != string(payload) {
			t.Fatalf("非门控模型应当原样放行并建不带票的 attempt：%+v", attempt)
		}
		if f := attempt.Features.Snapshot(); f == nil || f.StateFP != kongStateFingerprint(clientOwn) {
			t.Fatalf("帧里客户端回传的 state 没记下：%+v", f)
		}
		if err := g.GuardWSDownstream(context.Background(), account, attempt, metadata); err != nil {
			t.Fatalf("非门控模型上游回发 state 不该拦截: %v", err)
		}
		if f := attempt.Features.Snapshot(); f.ReissuedFP != kongStateFingerprint(reissued) || f.ReissuedTicketID == nil {
			t.Errorf("回发的 state 应当记下并入库：%+v", f)
		}
		if n := kongCountTickets(repo, 1, "gpt-6-sol"); n != 1 {
			t.Errorf("非门控模型收到的票应当入库，实际 %d 张", n)
		}
	})

	t.Run("HTTP→WS 的 map 载荷", func(t *testing.T) {
		g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, ticket)
		payload := map[string]any{
			"model":           "gpt-6-sol",
			"client_metadata": map[string]any{kongWSTurnStateMetadataKey: clientOwn},
		}
		attempt, err := g.PrepareWSMapPayload(context.Background(), account, "gpt-6-sol", payload)
		if err != nil {
			t.Fatalf("非门控不该报错: %v", err)
		}
		if attempt == nil || attempt.Grant != nil {
			t.Fatalf("非门控模型应当建不带票的 attempt：%+v", attempt)
		}
		if got := payload["client_metadata"].(map[string]any)[kongWSTurnStateMetadataKey]; got != clientOwn {
			t.Error("非门控模型的载荷被改写了")
		}
		if f := attempt.Features.Snapshot(); f == nil || f.StateFP != kongStateFingerprint(clientOwn) {
			t.Fatalf("载荷里客户端回传的 state 没记下：%+v", f)
		}
	})
}
