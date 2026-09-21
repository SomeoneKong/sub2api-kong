package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// 票据功能在转发链路上的接入点。设计见 DESIGN-codex-ticket.md §2.4。
//
// 上游发送分两类载体，各有自己的边界，**原生 WS 不经过 HTTP 那一处**：
//
//   - **HTTP**：接入点只有 `doOpenAIUpstream` 一处。那是全部 HTTP 上游发送的汇聚点（二十来个
//     调用点，含 Responses 透传、chat-completions 转 Responses、messages 转 Responses、
//     WS-HTTP bridge、alpha-search、images 桥接）。准入在它发送之前，交付判定在它拿到响应头
//     之后。按业务分支逐个去插，漏一条就等于开一个降智出口。
//   - **原生 WS**（ctx_pool 与 passthrough 两条路）：帧不流经 `doOpenAIUpstream`，所以两条路
//     各自在「客户端→上游」的帧出口做准入（`GuardWSFrame` + `PrepareWSTurn`）、在
//     「上游→客户端」的帧写点做交付判定（`GuardWSDownstream`）。
//
// 判定用的是**最终请求体里的模型**，不是客户端传来的别名：换个别名绕不过去。

// KongErrTicketDenied 表示本次请求拿不到合格票，必须拒服。
//
// **绝不降级放行**：这是整个功能存在的理由。
type KongErrTicketDenied struct {
	Reason     string
	RetryAfter string
}

func (e *KongErrTicketDenied) Error() string {
	if e.RetryAfter != "" {
		return fmt.Sprintf("codex ticket denied: %s (retry after %s)", e.Reason, e.RetryAfter)
	}
	return "codex ticket denied: " + e.Reason
}

// KongErrDeliveryBlocked 表示上游没有接受本次注入，输出不受合格票保障，不得交付。
type KongErrDeliveryBlocked struct {
	AccountID int64
	Model     string
}

func (e *KongErrDeliveryBlocked) Error() string {
	return fmt.Sprintf("codex ticket: upstream reissued turn-state for account %d model %s; refusing to deliver unprotected output", e.AccountID, e.Model)
}

// KongUpstreamAttempt 是一次上游发送的票据上下文。
//
// 它按**单次发送**而不是按客户端请求保存：failover 会在同一个客户端请求里换账号重发，而票是
// `(account, model)` 绑定的，用请求级的状态会把上一个账号的票算到下一个账号头上。
type KongUpstreamAttempt struct {
	// Model 是本次发送的最终上游模型。
	Model string
	// Grant 非空表示本次注入了票，交付前要判定上游是否接受。为空表示本次未受保护
	// （账号不是 full 模式），此时只收票、不判定。
	Grant *KongTicketGrant
	// Features 是这次发送的请求特征记录器（见 kong_ticket_request_feature.go）。
	//
	// 挂在 attempt 上而不是请求级的某个地方：它与票同为**单次发送**的属性，failover 换账号重发时
	// 上一次的特征不该跟着算到下一个账号的用量行上。
	Features *KongFeatureRecorder
	// ResponseID 是上游给这一轮分配的 id，由本轮第一个带 id 的下行事件绑上。
	//
	// 有了它才能判断一个终端或 error 事件到底属于哪一轮——否则一个针对上一轮的控制帧错误回包会被
	// 当成本轮结束，把保护提前摘掉。它只在绑定的那一刻写一次（WS 上用 CAS 换一份新副本），
	// 之后只读。
	ResponseID string
}

// FeatureSnapshot 取这次发送记下的请求特征。**attempt 为 nil 是常态**（非门控模型、功能未启用），
// 所以取值必须容得下它——调用点散在各协议的用量行组装处，那里漏一个 nil 判断就是一次 panic。
func (a *KongUpstreamAttempt) FeatureSnapshot() *KongRequestFeatures {
	if a == nil {
		return nil
	}
	return a.Features.Snapshot()
}

// KongTicketGateway 把编排服务接到转发链路上。
type KongTicketGateway struct {
	svc         *KongTicketService
	gatedModels map[string]bool
}

// NewKongTicketGateway 创建转发链路守卫。gatedModels 为空时整套保护不生效。
func NewKongTicketGateway(svc *KongTicketService, gatedModels []string) *KongTicketGateway {
	set := make(map[string]bool, len(gatedModels))
	for _, m := range gatedModels {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			set[trimmed] = true
		}
	}
	return &KongTicketGateway{svc: svc, gatedModels: set}
}

// Enabled 报告本功能是否生效。
func (g *KongTicketGateway) Enabled() bool {
	return g != nil && g.svc != nil && len(g.gatedModels) > 0
}

// IsGatedModel 判断一个**最终上游模型**是否受保护。
func (g *KongTicketGateway) IsGatedModel(model string) bool {
	if !g.Enabled() {
		return false
	}
	return g.gatedModels[strings.TrimSpace(model)]
}

// kongNewAttempt 建一次发送的 attempt，并把请求侧的特征一次记全。
//
// 三条通路共用它，所以"记哪些、怎么记"只有一份：漏在某一条通路上不会报错，只会让那条路的用量行
// 永远缺特征，而那正是最难发现的一种缺陷。
func kongNewAttempt(model string, grant *KongTicketGrant, clientState, outbound kongStateObservation) *KongUpstreamAttempt {
	ticketID := int64(0)
	if grant != nil {
		ticketID = grant.TicketID
	}
	recorder := &KongFeatureRecorder{}
	recorder.recordOutbound(clientState, outbound, ticketID)
	return &KongUpstreamAttempt{Model: model, Grant: grant, Features: recorder}
}

// PrepareUpstream 在一次上游发送之前做准入并注入票。
//
// 返回的 attempt 必须交给 AfterUpstream。拿不到合格票时返回 *KongErrTicketDenied，
// 调用方必须据此放弃这次发送。
func (g *KongTicketGateway) PrepareUpstream(ctx context.Context, req *http.Request, account *Account) (*KongUpstreamAttempt, error) {
	if !g.Enabled() || req == nil || account == nil {
		return nil, nil
	}
	result, err := g.gatedModelOf(req)
	if err != nil {
		if !g.ProtectsAccount(account) {
			// 这个账号不在保护范围内，读不出模型也无所谓——它本来就不注入、不判定。
			return nil, nil
		}
		return nil, &KongErrTicketDenied{Reason: "model_undeterminable: " + err.Error()}
	}
	if !result.Determinable {
		// 判不出是否受门控。受保护账号必须拒服：放行等于给每种解析差异开一个出口。
		// 未受保护的账号照常服务——它的请求本来就不受票据保障。
		if !g.ProtectsAccount(account) {
			return nil, nil
		}
		return nil, &KongErrTicketDenied{Reason: "model_undeterminable: " + result.Reason}
	}
	model := result.Model
	if model == "" || !g.gatedModels[model] {
		return nil, nil
	}
	// 客户端自带的那个 state 要在注入**之前**取：注入会覆盖这个头，之后就再也读不到它了。
	// full 模式下它正是"客户端手里那张是什么档"的唯一证据。
	clientState := kongOutboundStateFromHeader(req.Header)
	attempt := func(grant *KongTicketGrant) *KongUpstreamAttempt {
		return kongNewAttempt(model, grant, clientState, kongOutboundStateFromHeader(req.Header))
	}
	// Live 这条通路**承载不了票据**：创建之后是 sideband 原始双向转发，既没有逐轮注入点，也没有
	// 「上游是否接受了这张票」的证据。按默认拒绝极性拒服，而不是放行一次不受保障的门控会话。
	// 非账号级原因（换号也是同一个结论），所以它带 NextAccountStop、立刻进入耗尽呈现。
	//
	// **只拦受保护的账号**。这一句不能省：`off` / `observe` / 未配置的账号本来就不受票据保障，拦它们
	// 是无谓拒服，还会让 `mode=off` 这个退出手段在 Live 上失效（§8 靠它一键退回）。这个分支排在
	// EnsureTicket 之前，所以拿不到下面 `grant.NotApplicable` 那道模式判定的保护。
	if kongIsLiveCallsPath(req.URL) {
		if !g.ProtectsAccount(account) {
			return nil, nil
		}
		return nil, &KongErrTicketDenied{Reason: KongDenyLiveUnsupported}
	}

	// **计 token 的端点不要票**：它不产出任何模型内容，没有可被降智的东西。门控它只会在无票时白拒一次
	// 计数请求（无谓拒服），而且那条路上的拒服还拿不到本节的补救——`/v1/responses/input_tokens` 的两个
	// 调用点自己写 ops 传输错误、不走 failover 也不走 503 呈现，于是拒服会被记成上游故障。
	//
	// 仍然返回一个 attempt（Grant 为 nil）而不是 nil：那样 AfterUpstream 照旧收响应头里的票
	// （observed 票是免费的，不消耗任何出口静默），只是不做"上游是否接受注入"的判定——本来也没注入。
	//
	// 这是当前唯一可达的非生成端点：embeddings / images / realtime 各有自己的模型白名单校验，受门控的
	// codex 文本模型到不了那里。
	if kongIsNonGeneratingUpstreamPath(req.URL) {
		return attempt(nil), nil
	}

	grant, err := g.svc.EnsureTicket(ctx, account.ID, model)
	if err != nil {
		// 准备阶段出错时也不放行：无法确认保障就等于没有保障。
		return nil, kongEnsureDenial(ctx, err)
	}
	if grant.NotApplicable {
		// 该账号没开 full。哪个账号要保护是人工按观察结果配的，没配的照常服务——
		// 只是这次输出不受票据保障，也不做交付判定。
		return attempt(nil), nil
	}
	if !grant.Allowed {
		denied := &KongErrTicketDenied{Reason: grant.DenyReason}
		if !grant.RetryAfter.IsZero() {
			denied.RetryAfter = grant.RetryAfter.UTC().Format("2006-01-02T15:04:05Z")
		}
		return nil, denied
	}
	req.Header.Set(openAICodexTurnStateHeader, grant.State)
	return attempt(grant), nil
}

// kongNonGeneratingUpstreamPathSuffix 是不产出模型内容的上游端点。
//
// 按后缀匹配而不是全等：API Key 账号可以配自定义 base URL，端点会长成 `/api/v1/responses/input_tokens`
// 这样。Anthropic 侧的 count_tokens 与 Responses 侧的 input_tokens 共用同一个上游端点，一条规则覆盖两处。
const kongNonGeneratingUpstreamPathSuffix = "/responses/input_tokens"

// kongIsNonGeneratingUpstreamPath 报告这次上游发送是否指向不产出模型内容的端点。
func kongIsNonGeneratingUpstreamPath(u *url.URL) bool {
	if u == nil {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), kongNonGeneratingUpstreamPathSuffix)
}

// AfterUpstream 在拿到响应头之后、任何业务正文交付之前判定这次注入是否被上游接受。
//
// 判据是**本轮证据**：响应头里又出现 state，说明上游没接受我们注入的那张票（它的规则是带有效
// 票就不下发）。此时这次输出不受合格票保障，必须在第一个业务分片离开之前停下。
//
// 放在这里而不是各个响应处理函数里，是因为响应头先于正文到达，而调用方拿不到 body 就无从
// 交付——流式、非流式、SSE 转 JSON、compact 合成 SSE 全部一并覆盖。
//
// 只记录不拦截等于没有保护：那一刻降智输出已经生成，照常转发就直接违反目标。
func (g *KongTicketGateway) AfterUpstream(ctx context.Context, account *Account, attempt *KongUpstreamAttempt, resp *http.Response) error {
	if !g.Enabled() || attempt == nil || account == nil || resp == nil {
		return nil
	}
	state := strings.TrimSpace(extractOpenAICodexTurnState(resp.Header))
	// 上游回发的 state 一律记进特征，**三种模式都记**：对 full 它等于"没接受我们注入的那张"，
	// 对 off / observe 它是这条请求上游给了哪一档的直接读数。收票在前，这样入库的 id 也能一并记上。
	if state != "" {
		// 三种模式都收票：模式切到 full 时缓存里已有票可用，observe 也靠它触发诊断探测。
		attempt.Features.RecordReissued(state, g.svc.ObserveState(ctx, account.ID, attempt.Model, state))
	}
	if attempt.Grant == nil || state == "" {
		return nil
	}
	// 撤销的必须是**本次实际使用的那张票**，不是「当前票」：并发下后者可能已被新票接替，
	// 按它撤销会作废无辜的新票，还白搭一段拒服。
	g.svc.RevokeUsedTicket(ctx, account.ID, attempt.Model, attempt.Grant.TicketID)
	return &KongErrDeliveryBlocked{AccountID: account.ID, Model: attempt.Model}
}

// ---------------------------------------------------------------------------
// WebSocket 原生路径
//
// WS 与 HTTP 的票据通道是对称的，只是载体不同（口径取自 codex 客户端源码）：
//
//	                客户端 → 上游                              上游 → 客户端
//	HTTP   `x-codex-turn-state` 请求头                        响应头
//	WS     payload 的 `client_metadata["x-codex-turn-state"]`  带内 `response.metadata` 事件的 headers
//
// 两者都是**逐轮**的：codex 的 `ModelClientSession` 每轮新建，令牌在 turn start 收到、只在本轮内
// 重放（重试/增量追加/续接），跨轮重放按官方注释是违反契约的。所以连接复用不妨碍本轮判定——
// 本轮证据就在本轮的下行事件里，且 `response.metadata` 在 turn start 就到，先于任何内容分片。
// ---------------------------------------------------------------------------

// kongWSTurnStateMetadataKey 是 WS 上回送票据的 client_metadata 键。
const kongWSTurnStateMetadataKey = "x-codex-turn-state"

// kongWSDecisionKeys 是「读成哪个值会改变判定结果」的顶层键。
//
//   - `model` 决定这一帧是否受门控；
//   - `type` 决定这一帧是否被当作 `response.create`（不是的话整条准入都不会被触发）；
//   - `client_metadata` 是票的载体，重复时注入写进哪一个决定上游用的是谁的票。
var kongWSDecisionKeys = map[string]bool{"model": true, "type": true, "client_metadata": true}

// kongWSFrameAmbiguity 报告一帧的顶层是否存在「两个解码器会读出不同值」的歧义。
//
// 与 HTTP 侧 `kongExtractTopLevelModel` 第 2 条同源：gjson 取第一个重复键、`encoding/json` 取
// 最后一个，上游按哪个解释我们不知道。WS 上这个分歧比 HTTP 更宽——它还能改变帧分类，
// `{"type":"session.update","type":"response.create"}` 在我们眼里根本不是一轮生成。
//
// 非 JSON 或顶层不是对象时返回空：上游拿到的是同一份字节，它也读不出任何字段，构不成分歧
// （passthrough 允许的二进制帧正落在这里，不会被误拒）。
func kongWSFrameAmbiguity(raw []byte) string {
	if !gjson.ValidBytes(raw) {
		return ""
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return ""
	}
	seen := make(map[string]bool, 8)
	dup := ""
	root.ForEach(func(key, _ gjson.Result) bool {
		k := key.String()
		if !kongWSDecisionKeys[k] {
			return true
		}
		if seen[k] {
			dup = k
			return false
		}
		seen[k] = true
		return true
	})
	if dup != "" {
		return "duplicate_key:" + dup
	}
	return ""
}

// GuardWSFrame 在一帧「客户端→上游」离开之前挡住语义歧义的帧。
//
// 它与 PrepareWSTurn 分开是因为时机不同：帧分类本身就依赖 `type`，而分类错了 PrepareWSTurn
// 根本不会被调用——所以歧义必须在分类之前判。只对受保护账号生效，其余账号保持原业务语义。
func (g *KongTicketGateway) GuardWSFrame(account *Account, payload []byte) error {
	if !g.Enabled() || account == nil || !g.ProtectsAccount(account) {
		return nil
	}
	if reason := kongWSFrameAmbiguity(payload); reason != "" {
		return &KongErrTicketDenied{Reason: "frame_undeterminable: " + reason}
	}
	return nil
}

// kongWSVerifyInjectedState 确认改写后的 payload 里，票**唯一**且就是我们给的那张。
//
// sjson 只改第一处：客户端自带 `client_metadata` 或自带票据键时，写完可能仍留着它那一份，
// 而按末键语义解码出来的是客户端的票。attempt 却宣称注入了合格票，后置守卫也就永远不会报错。
func kongWSVerifyInjectedState(raw []byte, want string) string {
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return "payload_not_object"
	}
	metaCount := 0
	var meta gjson.Result
	root.ForEach(func(key, item gjson.Result) bool {
		if key.String() != "client_metadata" {
			return true
		}
		metaCount++
		meta = item
		return metaCount < 2
	})
	if metaCount != 1 {
		return fmt.Sprintf("client_metadata_count=%d", metaCount)
	}
	if !meta.IsObject() {
		return "client_metadata_not_object"
	}
	stateCount := 0
	got := ""
	meta.ForEach(func(key, item gjson.Result) bool {
		if key.String() != kongWSTurnStateMetadataKey {
			return true
		}
		stateCount++
		got = item.String()
		return stateCount < 2
	})
	if stateCount != 1 {
		return fmt.Sprintf("turn_state_count=%d", stateCount)
	}
	if got != want {
		return "turn_state_mismatch"
	}
	return ""
}

// PrepareWSTurn 在一帧 `response.create` 上送之前做准入，并把票写进 `client_metadata`。
//
// 返回改写后的 payload。未受门控或账号不在保护范围内时原样返回、attempt 为 nil。
// 拿不到合格票时返回 *KongErrTicketDenied，调用方必须据此终止这一轮。
func (g *KongTicketGateway) PrepareWSTurn(ctx context.Context, account *Account, model string, payload []byte) ([]byte, *KongUpstreamAttempt, error) {
	if account == nil || !g.Enabled() {
		return payload, nil, nil
	}
	// 帧内的 model 也要过一遍歧义判定，不能只信调用方用 gjson 取出的那个值：调用方取第一个、
	// 上游可能取最后一个，`{"model":"sol","model":"astra"}` 正落在分歧里——照调用方的值判会
	// 得出「非门控」。这条与 HTTP 侧同源，落点是受保护账号拒服。
	if err := g.GuardWSFrame(account, payload); err != nil {
		return payload, nil, err
	}
	// 判定用**实际出站字段**里的模型，不是调用方给的那个。
	//
	// 两者可以不一致：调用方的值可能取自规范化之前的 payload，而规范化会重建 JSON（合并重复键、
	// 改写字段）。上游只看最终字段，所以按调用方的值判会得出「非门控」而实际上送的是门控模型。
	// 字段缺失时才回落到调用方的值——Realtime 允许后续帧省略 model，那时沿用会话级模型。
	effective := model
	if res := kongExtractTopLevelModel(payload); res.Determinable && res.Model != "" {
		effective = res.Model
	}
	if !g.IsGatedModel(effective) {
		return payload, nil, nil
	}
	model = effective
	// 原生 WS：**不交接**。这条路上的拒服落点是按策略关闭连接，没有 failover 可走（见
	// EnsureTicketNoHandoff）。
	grant, err := g.svc.EnsureTicketNoHandoff(ctx, account.ID, model)
	if err != nil {
		return payload, nil, kongEnsureDenial(ctx, err)
	}
	clientState := kongOutboundStateFromWSFrame(payload)
	if grant.NotApplicable {
		// 该账号没开 full，照常服务：不注入、不判定。
		return payload, kongNewAttempt(model, nil, clientState, clientState), nil
	}
	if !grant.Allowed {
		denied := &KongErrTicketDenied{Reason: grant.DenyReason}
		if !grant.RetryAfter.IsZero() {
			denied.RetryAfter = grant.RetryAfter.UTC().Format("2006-01-02T15:04:05Z")
		}
		return payload, nil, denied
	}
	next, err := sjson.SetBytes(payload, "client_metadata."+kongWSTurnStateMetadataKey, grant.State)
	if err != nil {
		// 写不进去就等于没注入。放行会让这一轮不受保障，所以拒。
		return payload, nil, &KongErrTicketDenied{Reason: "inject_failed: " + err.Error()}
	}
	// 写完再验一遍实际出站字节：setter 没报错不等于上游会读到我们这张票。
	if reason := kongWSVerifyInjectedState(next, grant.State); reason != "" {
		return payload, nil, &KongErrTicketDenied{Reason: "inject_unverified: " + reason}
	}
	return next, kongNewAttempt(model, grant, clientState, kongOutboundStateFromWSFrame(next)), nil
}

// GuardWSDownstream 在一帧下行事件交付客户端之前判定本轮注入是否被接受。
//
// 只看 `response.metadata`：那是上游在 turn start 下发路由令牌的事件，也是 WS 上唯一属于**本轮**
// 的证据。它带 state 就说明上游没有接受我们注入的票（规则同 HTTP：带有效票就不下发），这一轮的
// 输出不受合格票保障，必须在任何内容分片离开之前停下。
func (g *KongTicketGateway) GuardWSDownstream(ctx context.Context, account *Account, attempt *KongUpstreamAttempt, payload []byte) error {
	if !g.Enabled() || account == nil || len(payload) == 0 {
		return nil
	}
	eventType, _, _ := parseOpenAIWSEventEnvelope(payload)
	if eventType != "response.metadata" {
		return nil
	}
	state := kongWSTurnStateFromEvent(payload)
	if state == "" {
		return nil
	}
	g.observeWSReissued(ctx, account, attempt, state)
	if attempt == nil || attempt.Grant == nil {
		return nil
	}
	model := attempt.Model
	g.svc.RevokeUsedTicket(ctx, account.ID, model, attempt.Grant.TicketID)
	return &KongErrDeliveryBlocked{AccountID: account.ID, Model: model}
}

// ObserveWSDownstream 只做**收票与记特征**，不做交付判定。
//
// 它存在的理由是 ctx_pool 那条路上有一个死角：下行帧的处理整块挂在"还能写客户端"这个条件下
// （见 openai_ws_forwarder_ingress.go 的 clientDisconnected 分支），客户端中途断开后，上游后续发来的
// `response.metadata` 仍被读到、用量行照常结算，却既不收票也不记特征。收票是免费样本、特征是那条
// 用量行唯一的降智证据，两者都不该取决于客户端还连不连着。
//
// **刻意不含交付判定**：断连之后没有任何业务输出会送出去，此时再产出 KongErrDeliveryBlocked 只会
// 给调用方凭空加一条错误路径。判定仍然只在写客户端那条路上做（GuardWSDownstream）。
func (g *KongTicketGateway) ObserveWSDownstream(ctx context.Context, account *Account, attempt *KongUpstreamAttempt, payload []byte) {
	if !g.Enabled() || account == nil || len(payload) == 0 {
		return
	}
	if eventType, _, _ := parseOpenAIWSEventEnvelope(payload); eventType != "response.metadata" {
		return
	}
	state := kongWSTurnStateFromEvent(payload)
	if state == "" {
		return
	}
	g.observeWSReissued(ctx, account, attempt, state)
}

// observeWSReissued 是两条 WS 下行入口共用的收票 + 记特征。
//
// 三种模式都收票：模式切到 full 时缓存里已有票可用，observe 也靠它触发诊断探测。这条是原生 WS
// 路径上唯一的收票入口——只走 HTTP 的话，纯 WS 业务的账号永远采不到样本。与 HTTP 侧同一条：
// 回发的 state 连同它入库的 id 一起记进特征。
func (g *KongTicketGateway) observeWSReissued(ctx context.Context, account *Account, attempt *KongUpstreamAttempt, state string) {
	if attempt == nil || attempt.Model == "" {
		return
	}
	attempt.Features.RecordReissued(state, g.svc.ObserveState(ctx, account.ID, attempt.Model, state))
}

// kongWSTurnStateFromEvent 从 `response.metadata` 事件里取票。
//
// 两处都要看且键名大小写不敏感：codex 先读 `response.headers`、再回落到顶层 `headers`
// （codex-rs/codex-api/src/sse/responses.rs）。只认一处会在另一种形态下漏判成「上游接受了」。
func kongWSTurnStateFromEvent(payload []byte) string {
	for _, path := range []string{"response.headers", "headers"} {
		headers := gjson.GetBytes(payload, path)
		if !headers.IsObject() {
			continue
		}
		found := ""
		headers.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), kongWSTurnStateMetadataKey) {
				found = strings.TrimSpace(value.String())
				return false
			}
			return true
		})
		if found != "" {
			return found
		}
	}
	return ""
}

// ProtectsAccount 报告这个账号是否在保护范围内（模式为 full）。
//
// 哪些账号要保护是人工按观察结果配的（观察到高概率降智才开 full），所以「没配」是常态。
// 它决定两件事：要不要注入票，以及**判不出模型时该拒服还是放行**。
func (g *KongTicketGateway) ProtectsAccount(account *Account) bool {
	if !g.Enabled() || account == nil {
		return false
	}
	cfg, _ := ParseKongTicketConfig(account.Extra)
	return cfg.Mode == KongTicketModeFull
}

// kongGatedModelResult 是一次门控判定的结果。
type kongGatedModelResult struct {
	// Model 是顶层 model 的值；为空表示这份请求体没有顶层 model。
	Model string
	// Determinable 为假表示**判不出**这次是否受门控：请求体畸形、顶层出现两个 model 键、
	// model 不是字符串。这与「确定没有门控模型」是两件事——前者必须按受保护账号拒服处理。
	Determinable bool
	// Reason 解释不可判定的原因，进拒服事件。
	Reason string
}

// gatedModelOf 判定这次发送是否受门控。
func (g *KongTicketGateway) gatedModelOf(req *http.Request) (kongGatedModelResult, error) {
	if req.Body == nil || req.Body == http.NoBody {
		// 没有请求体就指定不了模型。
		return kongGatedModelResult{Determinable: true}, nil
	}
	if req.GetBody == nil {
		// 读了就无法还原。
		return kongGatedModelResult{}, errors.New("请求体不可重读（GetBody 为空）")
	}
	body, err := req.GetBody()
	if err != nil {
		return kongGatedModelResult{}, fmt.Errorf("重读请求体: %w", err)
	}
	defer func() { _ = body.Close() }()
	// 读进 strings.Builder 而不是 io.ReadAll + gjson.ParseBytes：后者要付三份请求体的代价
	// ——ReadAll 的倍增扩容、它返回的那份，加上 `ParseBytes` 内部 `string(json)` 的整份复制
	// （gjson v1.18 的实现就是 `Parse(string(json))`）。Builder 按 Content-Length 预留一次，
	// `String()` 直接交出自己的缓冲区不再复制，总开销降到一份。门控功能一开，非门控与
	// off/observe 的请求也全部经过这里，这份放大对每个请求都要付。
	var sb strings.Builder
	if req.ContentLength > 0 {
		sb.Grow(int(req.ContentLength))
	}
	if _, err := io.Copy(&sb, body); err != nil {
		return kongGatedModelResult{}, fmt.Errorf("读请求体: %w", err)
	}
	raw := sb.String()
	// Live（realtime）创建请求把模型放在 **session.model**，顶层压根没有 `model`。按顶层判会得出
	// "非门控"，于是受保护账号会在门控模型上发出一次**无票上送**——那是放行未受保障的输出，本项目最
	// 重的一类缺陷。按出站 URL 识别这条端点，从嵌套对象里取。
	if kongIsLiveCallsPath(req.URL) {
		return kongExtractLiveSessionModel(raw), nil
	}
	return kongExtractTopLevelModelString(raw), nil
}

// kongLiveCallsPathSuffix 是 Live（realtime）创建端点的路径后缀。
const kongLiveCallsPathSuffix = "/realtime/calls"

// kongIsLiveCallsPath 报告这次上送是否指向 Live 创建端点。
func kongIsLiveCallsPath(u *url.URL) bool {
	if u == nil {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), kongLiveCallsPathSuffix)
}

// kongExtractLiveSessionModel 取 Live 创建请求里 `session.model`。
//
// 重复键的判定与顶层同源（见 kongExtractTopLevelModelString）：gjson 取第一个、encoding/json 取最后
// 一个，分歧即不可判定。`session` 自身重复同样不可判定——上游读哪一个我们说不准。
func kongExtractLiveSessionModel(raw string) kongGatedModelResult {
	root := gjson.Parse(raw)
	if !root.IsObject() {
		return kongGatedModelResult{Reason: "body_not_object"}
	}
	var session gjson.Result
	seen := 0
	root.ForEach(func(key, item gjson.Result) bool {
		if key.String() != "session" {
			return true
		}
		seen++
		session = item
		return seen < 2
	})
	switch {
	case seen == 0:
		// 没有 session：不是 Live 创建的形态，按"未指定模型"处理（与顶层缺 model 同口径）。
		return kongGatedModelResult{Determinable: true}
	case seen > 1:
		return kongGatedModelResult{Reason: "duplicate_session_key"}
	case !session.IsObject():
		return kongGatedModelResult{Reason: "session_not_an_object"}
	}
	return kongExtractTopLevelModelString(session.Raw)
}

// LiveFrameGuard 为一条 Live sideband 连接生成逐帧守卫。
//
// sideband 是原始双向转发，没有逐轮守卫，所以**票据无从注入、交付也无从判定**。创建端点已经挡住了
// 门控模型，但协议允许会话中途改模型（`session.update`），不挡这一步等于留一条"先建非门控会话、再切
// 到门控模型"的绕行。
//
// **账号只在建连时读一次**：sideband 上音频帧可以每秒几十个，逐帧读库会把数据库打满。返回的闭包对
// 未受保护的账号是空操作，调用方不必分两种写法。
//
// **读不到账号就返回错误，不能返回空守卫**（fail-closed）。"读失败"与"确认不受保护"是两件事，合并
// 成放行等于让一次瞬时读库故障把整条连接的保护关掉——而创建端点补不上这个缺口：会话可以先用非门控
// 模型建好，再用一帧 `session.update` 切过去。这条通路上守卫是唯一的保护点。
func (g *KongTicketGateway) LiveFrameGuard(ctx context.Context, accountID int64) (func([]byte) error, error) {
	noop := func([]byte) error { return nil }
	if !g.Enabled() {
		return noop, nil
	}
	account, err := g.svc.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("读账号 %d 以装配 Live 逐帧守卫: %w", accountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %d 不存在，无法装配 Live 逐帧守卫", accountID)
	}
	if !g.ProtectsAccount(account) {
		return noop, nil
	}
	return g.guardLiveFrame, nil
}

// guardLiveFrame 判一帧「客户端→上游」的 sideband 帧是否声明了门控模型。纯判定，不读库。
func (g *KongTicketGateway) guardLiveFrame(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	for _, path := range []string{"session", "response"} {
		res := kongExtractLiveNestedModel(payload, path)
		if !res.Determinable {
			return &KongErrTicketDenied{Reason: KongDenyLiveUnsupported}
		}
		if res.Model != "" && g.gatedModels[res.Model] {
			return &KongErrTicketDenied{Reason: KongDenyLiveUnsupported}
		}
	}
	return nil
}

// kongExtractLiveNestedModel 取一帧里 `<path>.model`。path 不存在时视为"未指定模型"。
func kongExtractLiveNestedModel(payload []byte, path string) kongGatedModelResult {
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		// 非对象帧（二进制音频等）不声明模型。
		return kongGatedModelResult{Determinable: true}
	}
	var nested gjson.Result
	seen := 0
	root.ForEach(func(key, item gjson.Result) bool {
		if key.String() != path {
			return true
		}
		seen++
		nested = item
		return seen < 2
	})
	switch {
	case seen == 0:
		return kongGatedModelResult{Determinable: true}
	case seen > 1:
		return kongGatedModelResult{Reason: "duplicate_" + path + "_key"}
	case !nested.IsObject():
		return kongGatedModelResult{Determinable: true}
	}
	return kongExtractTopLevelModelString(nested.Raw)
}

// kongExtractTopLevelModel 取出 JSON 请求体顶层的 `model`。
//
// 三条必须守住的语义，每一条都对应一条绕过面：
//
//  1. **按解码后的值判定**。`{"model":"gpt-6-\u0061stra"}` 的原始字节里找不到模型名，解码后
//     却正是那个门控模型——任何形式的字节子串匹配都能被一行转义绕开。键名同理会被转义
//     （`{"\u006dodel":...}`）。
//  2. **顶层出现两个 `model` 键时判为不可判定**。gjson 取第一个、`encoding/json` 取最后一个，
//     上游按哪个解释我们不知道——`{"model":"sol","model":"astra"}` 正好落在这个分歧里。
//  3. **「判不出」不等于「非门控」**。畸形请求体、非字符串 model 都归入不可判定，由调用方按
//     账号是否受保护决定拒服还是放行。把它们当成非门控放行，等于给每种解析差异开一个出口。
//
// 顶层不是 JSON 对象时返回「确定无门控模型」：上游拿到的是同一份请求体，它也读不出模型，所以
// 这种请求不可能是门控的 Responses 调用。
func kongExtractTopLevelModel(raw []byte) kongGatedModelResult {
	return kongExtractTopLevelModelString(string(raw))
}

// kongExtractTopLevelModelString 是上面那个函数的零复制形态，供已经持有字符串的调用方使用。
func kongExtractTopLevelModelString(raw string) kongGatedModelResult {
	// 先做一次完整性校验。gjson 对畸形输入是宽容的，会给出局部结果——不先验一遍，
	// 一份截断的请求体可能被读成「没有 model」而放行。Valid 只扫不分配。
	if !gjson.Valid(raw) {
		return kongGatedModelResult{Reason: "malformed_json"}
	}
	root := gjson.Parse(raw)
	if !root.IsObject() {
		return kongGatedModelResult{Determinable: true}
	}

	var value gjson.Result
	seen := 0
	root.ForEach(func(key, item gjson.Result) bool {
		if key.String() != "model" {
			return true
		}
		seen++
		value = item
		// 数到第二个就够了：重复即不可判定，不必继续扫。
		return seen < 2
	})

	switch {
	case seen == 0:
		return kongGatedModelResult{Determinable: true}
	case seen > 1:
		return kongGatedModelResult{Reason: "duplicate_model_key"}
	case value.Type != gjson.String:
		return kongGatedModelResult{Reason: "model_not_a_string"}
	}
	return kongGatedModelResult{Model: strings.TrimSpace(value.String()), Determinable: true}
}

// KongTicketFailover 判定一次票据拒服能否换账号，以及该不该先在同一账号上等一等。
//
// **几乎所有票据拒服都能换号**，因为它们全是**账号级**的条件：
//
//   - `window_closed`——静默与冷却是按**该账号的票据出口**累积的，与别的账号的出口无关；
//   - `egress_unusable` / `egress_busy` / `no_ticket_source`——就是这个账号自己的出口配置或占用；
//   - `account_unready`——本来就是单账号不可调度；
//   - `task_no_ticket` / `wait_timeout`——这个账号的任务没拿到票，别的账号有自己的票；
//   - 真降档——账号 A 被降智不说明账号 B 也被降智。
//
// ⚠️ 这里曾经只放行 `preparing`，理由是"其余换号只是把同一个结论在每个账号上重演"。**那个推理是
// 错的**，它把账号级条件当成了全局条件。生产实测（2026-09-20）：一个账号因 window_closed 拒服期间，
// 同分组另一个账号已经有合格票，64 个请求仍被直接拒掉、一个都没切过去——那是无谓拒服，与放行降智
// 同级。真正全局的情况（所有账号都没票）由 failover 框架自然耗尽，不需要在票据层提前判定。
//
// 第二个返回值是"是否先在同账号重试"，**它不阻止换号**：框架先在同账号重试若干次（递增延时、封顶
// 几秒），耗尽后照样换。所以它的含义是"先给本账号一点时间"，只在**短期内状态可能变**时才值得：
//
//   - 值得等：`preparing`（本账号的任务正在跑，产物就是本账号要的票）、`other_model_task`
//     （本账号的在途任务在给别的模型跑；它可能是候选验证——走 kongVerifyOnlySlot、不碰票据出口
//     ——完成后本模型确实可能可取）；
//   - 不值得等：`window_closed`（静默要到某个时刻才满，几秒钟不会变）、`egress_unusable` /
//     `no_ticket_source`（配置问题，等不会变）、`task_no_ticket` / `wait_timeout`（任务已结束且
//     没拿到票）、`egress_busy`（见下）。对这些先等本号纯属浪费重试次数，还会延后真正能服务的
//     那次换号。
//
// ⚠️ `egress_busy` 曾被算作"值得等"，理由是"别的任务占着，马上释放"。**那个理由不成立**：占着
// 出口的是**另一个账号**的任务，它产出的票属于那个账号；更要紧的是那次取票会 `noteEgressUse` 把
// 这条**共享**出口的静默清零（见 fetchStore），于是它一释放，本账号立刻需要等满整个 min_idle
// ——状态从 `egress_busy` 变成 `window_closed`，而那正是"不值得等"的那一类。
//
// 值得等的那几种还要看**这个请求有没有绑定粘性会话**：绑定了先等（换号会让上下文缓存失效、粘性
// 计费被强制），没绑定直接换。但当前只有部分入口把绑定事实写进 context（通用 Gateway 与 Gemini
// 写 `WithPrefetchedStickySession`，OpenAI Responses/Messages 主路径不写），所以**读不到就按"可能
// 绑定了"处理**——两种猜错的后果不对称：该换却先等只多花几秒重试延时，该等却直接换会破坏会话。
func KongTicketFailover(ctx context.Context, err error) (canFailover bool, retrySameAccount bool) {
	var denied *KongErrTicketDenied
	if !errors.As(err, &denied) || denied == nil {
		return false, false
	}
	return true, kongDenyWorthSameAccountWait(denied.Reason)
}

// kongDenyWorthSameAccountWait 报告这个拒服原因在**几秒之内**是否可能自行好转。
//
// 只有会好转的才值得先在同账号重试。其余的先等等于把重试次数花在一个确定不会变的状态上，而那些
// 次数本该用来换到一个真正有票的账号。
func kongDenyWorthSameAccountWait(reason string) bool {
	switch reason {
	case KongDenyPreparing, KongDenyOtherModelTask:
		return true
	default:
		return false
	}
}

// kongEnsureDenial 把 Ensure* 的失败翻成一次拒服。
//
// **调用方 context 已经中断时算账号级 `wait_timeout`**，不是系统级 `ensure_failed`。准入这一路上到处
// 都在读库（账号、当前票、候选、出口活动、冷却，任务完成后还要再读一次票），HTTP 侧的首输出守卫一
// 取消，其中任何一处都会返回 context 错误。按 `ensure_failed` 归类会带上 `NextAccountStop`，于是
// "这个账号没来得及、别的账号有票"变成整个请求终止——那是无谓拒服。**只补某一个数据库调用没有用**，
// 落点太多，所以判在这个共同边界上。
//
// 不按"是谁取消的"区分：守卫用 `WithCancel`，拿到的是 `Canceled`，与客户端断开不可分。客户端真走了
// 那一路另有判据——上层在换号之前先查 `failoverClientGone`。
func kongEnsureDenial(ctx context.Context, err error) *KongErrTicketDenied {
	if ctx.Err() != nil {
		return &KongErrTicketDenied{Reason: KongDenyWaitTimeout}
	}
	return &KongErrTicketDenied{Reason: "ensure_failed: " + err.Error()}
}

// kongTicketDenyReasonPrefix 是票据拒服在 failover 错误 `Reason` 上占的命名空间。
//
// **身份不能靠枚举一张原因清单**：`*KongErrTicketDenied` 有十几个构造点，其中六种原因带自由文本
// （`ensure_failed: <err>` 这类），维护一张反查表必然漏项——漏一项就是把一次本地拒服当成账号故障
// 计入健康度 EWMA（实测 `ensure_failed` 让 EWMA 从 0 跳到 0.20）。前缀是本 fork 自己控制的命名
// 空间，新增拒服原因不需要同步任何清单。
const kongTicketDenyReasonPrefix = "kong_ticket_denied:"

// KongTicketDenyFailoverReason 把一个拒服原因翻成 failover 错误上带命名空间的 `Reason`。
//
// 导出是因为呈现在 handler 包、构造在本包，而两侧必须用同一个编码——各写一份字面量就等于把跨包
// 契约拆成两处，改一处不报错、只是票据拒服又变回 502。
func KongTicketDenyFailoverReason(denyReason string) GatewayFailureReason {
	return GatewayFailureReason(kongTicketDenyReasonPrefix + kongTicketDenyCategory(denyReason))
}

// kongTicketDenyCategory 把拒服原因规范成稳定分类：自由文本形态取冒号前那一段。
//
// 除了稳定，这一步还挡住一条泄漏：`ensure_failed: ` 后面接的是 `err.Error()`，可能带库连接串、
// 代理地址这类细节，而 `Reason` 会一路流进给客户端的文案。**拒服原因是给人看的分类，不是错误详情
// 的搬运通道**——详情留在原始错误与日志里。
func kongTicketDenyCategory(reason string) string {
	if idx := strings.IndexByte(reason, ':'); idx >= 0 {
		return strings.TrimSpace(reason[:idx])
	}
	return strings.TrimSpace(reason)
}

// kongTicketDenyFailover 把一次票据拒服包成 failover 错误，同时**保留原始拒服错误**。
//
// 多值 Unwrap 让 `errors.As` 两侧都取得到：handler 取 `*UpstreamFailoverError` 去换号，调度上报取
// `*KongErrTicketDenied` 去豁免健康度。转换时丢掉原类型正是上一版的缺陷——那时豁免只能靠 reason
// 字符串反查，带自由文本的原因一律漏掉。
type kongTicketDenyFailover struct {
	failover *UpstreamFailoverError
	denied   *KongErrTicketDenied
}

func (e *kongTicketDenyFailover) Error() string { return e.denied.Error() }

func (e *kongTicketDenyFailover) Unwrap() []error { return []error{e.failover, e.denied} }

// kongDenyIsAccountScoped 报告这个拒服原因是否只反映**这个账号此刻的状态**。
//
// 只有账号级的才值得换号。请求级的技术原因（帧语义歧义、注入写不进去、模型判不出）在每个账号上都会
// 得出同一个结论，换号只是把它在每个账号上重演一遍——还白占别的账号的槽位、把真正能服务的那次重试
// 推到重试预算之外。
//
// 正向列举、默认 false：漏一项的后果是"本可换号却没换"（少拿一点收益），而反过来默认 true 时漏判
// 会让一个必然失败的请求把整个账号池走一遍。两个方向不对称，所以取安全的那一侧。
// 与 KongIsTicketDeniedFailover 的判据不同不是疏漏：那里认的是"是不是票据拒服"（漏判会造成归因
// 错误，所以必须用类型），这里判的是"换号有没有用"。
// KongDenyIsAccountScoped 导出给 handler 侧选关闭码/呈现用：账号级原因值得让客户端稍后再来，
// 请求级或系统级原因重试也是同一个结论。
func KongDenyIsAccountScoped(reason string) bool {
	return kongDenyIsAccountScoped(reason)
}

func kongDenyIsAccountScoped(reason string) bool {
	switch reason {
	case KongDenyAccountUnready, KongDenyEgressUnusable, KongDenyNoTicketSource,
		KongDenyWindowClosed, KongDenyEgressBusy, KongDenyWaitTimeout,
		KongDenyPreparing, KongDenyOtherModelTask, KongDenyTaskNoTicket:
		return true
	default:
		return false
	}
}

// newKongTicketDenyFailover 构造票据拒服的 failover 错误。
//
//   - RequestScopedTransient：**不得据此临时封禁账号**。账号本身是好的，只是这一刻没票可注入。
//   - Scope=account：换一个账号确实有帮助——票是按账号持有的。
//   - NextAccountStop（仅非账号级原因）：**照样包成 failover 错误**，但不换号。包装是为了让身份、
//     调度豁免与 503 呈现三件事在所有拒服原因上走同一条路；不换号是因为换了也是同一个结论。
func newKongTicketDenyFailover(denied *KongErrTicketDenied, retrySameAccount bool) error {
	failover := &UpstreamFailoverError{
		StatusCode:             0,
		RequestScopedTransient: true,
		RetryableOnSameAccount: retrySameAccount,
		Scope:                  GatewayFailureScopeAccount,
		Reason:                 KongTicketDenyFailoverReason(denied.Reason),
	}
	if !kongDenyIsAccountScoped(denied.Reason) {
		failover.NextAccountAction = NextAccountStop
	}
	return &kongTicketDenyFailover{failover: failover, denied: denied}
}

// KongTicketDenyReasonOf 从一个**已经摘出来的** failover 错误反查票据拒服分类。
//
// 耗尽呈现那一层只拿得到 `*UpstreamFailoverError`（`errors.As` 已经把它从错误链里摘出来了），所以
// 身份判定只能落在这个结构上。这样识别就**绑定当前终止错误**：换号过程中前一个账号的拒服不能用来
// 解释后一个账号的真实故障——反过来记同样是归因错误，只是方向相反。
func KongTicketDenyReasonOf(failoverErr *UpstreamFailoverError) (string, bool) {
	if failoverErr == nil {
		return "", false
	}
	reason := string(failoverErr.Reason)
	if !strings.HasPrefix(reason, kongTicketDenyReasonPrefix) {
		return "", false
	}
	return strings.TrimPrefix(reason, kongTicketDenyReasonPrefix), true
}

// KongIsTicketDeniedFailover 报告这个错误是否为「已转成 failover 的票据拒服」。
//
// 调度上报要用它：票据拒服**都不是账号的故障**，账号本身是好的、只是此刻没票可注入。计进错误率
// EWMA 会让"票越缺、账号越被判坏"，与事实相反；临时封禁更糟——那会把一个几十秒后就补上票的账号
// 推出调度。
//
// **两种传参形态都要认**：多值 `Unwrap` 只能由外向内查找，所以一旦调用方已经用 `errors.As` 把
// `*UpstreamFailoverError` 摘出来、再把那个指针单独传过来（WS 换号上报就是这样），从它反查不到并列的
// `*KongErrTicketDenied`。那时靠 failover 错误自己的受控命名空间识别。
func KongIsTicketDeniedFailover(err error) bool {
	var wrapped *kongTicketDenyFailover
	if errors.As(err, &wrapped) {
		return true
	}
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		_, ok := KongTicketDenyReasonOf(failoverErr)
		return ok
	}
	return false
}

// KongIsTicketDenied 报告错误是否为票据拒服，便于调用方映射状态码。
func KongIsTicketDenied(err error) bool {
	var denied *KongErrTicketDenied
	return errors.As(err, &denied)
}

// KongIsDeliveryBlocked 报告错误是否为交付被拦。
func KongIsDeliveryBlocked(err error) bool {
	var blocked *KongErrDeliveryBlocked
	return errors.As(err, &blocked)
}

// kongTicketDenyWaitKey 是「本次请求的票据恢复信息」的 context 键。
const kongTicketDenyWaitKey = "kong_ticket_deny_wait"

// KongTicketDenyWait 汇总**本次请求已尝试过的全部账号**的票据恢复信息。
//
// 按请求累计而不是只留最后一次：failover 会依次问好几个账号，最后那个不一定是最早能恢复的。拿它
// 的恢复时刻当整池等待，客户端可能白等到最晚那个账号——A 一分钟后可重试、B 三十分钟后可重试，最后
// 访问 B 就会让客户端多等二十九分钟。
type KongTicketDenyWait struct {
	// Earliest 是已尝试账号中**最早**的恢复时刻（RFC3339）。
	Earliest string
	// Unknown 为真表示至少有一个被拒账号没给出恢复时刻。
	//
	// **恢复时刻未知不等于很久**：那时不能拿别人的长等待当整池等待，宁可不给 Retry-After，让客户端
	// 用自己的退避——给一个过长的值会让一个几十秒后就有票的池子被搁置二十分钟。
	Unknown bool
}

// MarkKongTicketDenied 把这次拒服的恢复时刻并进本请求的累计等待信息。
//
// 只记恢复时刻、不记 reason：呈现时的 reason 取自**当前终止错误**（见 KongTicketDenyReasonOf），
// 请求级地留一份 reason 只会给"上一个账号的拒服解释下一个账号的故障"开出口。
func MarkKongTicketDenied(c *gin.Context, retryAfter string) {
	if c == nil {
		return
	}
	next := &KongTicketDenyWait{}
	if prev := KongTicketDenyWaitFromContext(c); prev != nil {
		*next = *prev
	}
	retryAfter = strings.TrimSpace(retryAfter)
	at, err := time.Parse(time.RFC3339, retryAfter)
	if retryAfter == "" || err != nil {
		// 空值与读不懂的值都算"不知道"：不能当成"无需等待"，也不能拿去比较。
		next.Unknown = true
		c.Set(kongTicketDenyWaitKey, next)
		return
	}
	if prevAt, perr := time.Parse(time.RFC3339, next.Earliest); next.Earliest == "" || perr != nil || at.Before(prevAt) {
		next.Earliest = retryAfter
	}
	c.Set(kongTicketDenyWaitKey, next)
}

// kongTicketDenyServedKey 记「本次请求最终返回的就是票据拒服」。
const kongTicketDenyServedKey = "kong_ticket_deny_served"

// MarkKongTicketDenyServed 在票据拒服的响应**真的写出去之后**记一笔，供错误看板归因。
//
// 与 KongTicketDenyWait 分开是刻意的：那个是"本请求期间有账号拒过服"，换号成功或后一个账号真的
// 故障时它照样在；而看板归因必须只看**最终发生了什么**。拿前者做归因就会把一次真实的上游故障记成
// 本地拒服——正是本次要修的那类归因错误的反方向。
func MarkKongTicketDenyServed(c *gin.Context, reason string) {
	if c == nil {
		return
	}
	c.Set(kongTicketDenyServedKey, reason)
}

// KongTicketDenyServedReason 报告本次请求最终是否以票据拒服收场，以及拒服分类。
func KongTicketDenyServedReason(c *gin.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	if raw, ok := c.Get(kongTicketDenyServedKey); ok {
		if reason, ok := raw.(string); ok {
			return reason, true
		}
	}
	return "", false
}

// KongTicketDenyWaitFromContext 取本次请求累计的票据恢复信息。没有则返回 nil。
func KongTicketDenyWaitFromContext(c *gin.Context) *KongTicketDenyWait {
	if c == nil {
		return nil
	}
	if raw, ok := c.Get(kongTicketDenyWaitKey); ok {
		if wait, ok := raw.(*KongTicketDenyWait); ok {
			return wait
		}
	}
	return nil
}
