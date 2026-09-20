package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	// ResponseID 是上游给这一轮分配的 id，由本轮第一个带 id 的下行事件绑上。
	//
	// 有了它才能判断一个终端或 error 事件到底属于哪一轮——否则一个针对上一轮的控制帧错误回包会被
	// 当成本轮结束，把保护提前摘掉。它只在绑定的那一刻写一次（WS 上用 CAS 换一份新副本），
	// 之后只读。
	ResponseID string
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

	grant, err := g.svc.EnsureTicket(ctx, account.ID, model)
	if err != nil {
		// 准备阶段出错时也不放行：无法确认保障就等于没有保障。
		return nil, &KongErrTicketDenied{Reason: "ensure_failed: " + err.Error()}
	}
	if grant.NotApplicable {
		// 该账号没开 full。哪个账号要保护是人工按观察结果配的，没配的照常服务——
		// 只是这次输出不受票据保障，也不做交付判定。
		return &KongUpstreamAttempt{Model: model}, nil
	}
	if !grant.Allowed {
		denied := &KongErrTicketDenied{Reason: grant.DenyReason}
		if !grant.RetryAfter.IsZero() {
			denied.RetryAfter = grant.RetryAfter.UTC().Format("2006-01-02T15:04:05Z")
		}
		return nil, denied
	}
	req.Header.Set(openAICodexTurnStateHeader, grant.State)
	return &KongUpstreamAttempt{Model: model, Grant: grant}, nil
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
	if state != "" {
		// 三种模式都收票：模式切到 full 时缓存里已有票可用，observe 也靠它触发诊断探测。
		g.svc.ObserveState(ctx, account.ID, attempt.Model, state)
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
		return payload, nil, &KongErrTicketDenied{Reason: "ensure_failed: " + err.Error()}
	}
	if grant.NotApplicable {
		// 该账号没开 full，照常服务：不注入、不判定。
		return payload, &KongUpstreamAttempt{Model: model}, nil
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
	return next, &KongUpstreamAttempt{Model: model, Grant: grant}, nil
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
	model := ""
	if attempt != nil {
		model = attempt.Model
	}
	if model != "" {
		// 三种模式都收票：模式切到 full 时缓存里已有票可用，observe 也靠它触发诊断探测。
		// 这条是原生 WS 路径上唯一的收票入口——只走 HTTP 的话，纯 WS 业务的账号永远采不到样本。
		g.svc.ObserveState(ctx, account.ID, model, state)
	}
	if attempt == nil || attempt.Grant == nil {
		return nil
	}
	g.svc.RevokeUsedTicket(ctx, account.ID, model, attempt.Grant.TicketID)
	return &KongErrDeliveryBlocked{AccountID: account.ID, Model: model}
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
	return kongExtractTopLevelModelString(sb.String()), nil
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

// KongTicketPreparing 报告这次拒服是否为「票据正在准备」，以及该不该在同一账号上先等一等。
//
// 这是**唯一可以 failover 的票据拒服**：本账号的票据任务已在后台跑，而别的账号此刻有可用票，换过去
// 比等几十秒更快。其余拒服（静默未满、出口不可用、真降档）换号救不了——那些情况下换号只是把同一个
// 结论在每个账号上重演一遍，还会把每个账号都记进失败列表。
//
// 第二个返回值是"是否先在同账号重试"。**注意它不阻止换号**：框架会先在同账号重试若干次（每次有
// 递增延时、封顶几秒），耗尽后照样换号。所以它真正的含义是"先给本账号一点时间"。
//
// 判据本应是"这个请求有没有绑定粘性会话"：绑定了先等（换号会让上下文缓存失效、粘性计费被强制），
// 没绑定就直接换（换号对它没有代价）。但**当前只有部分入口会把绑定事实写进 context**——通用 Gateway
// 与 Gemini handler 写 `WithPrefetchedStickySession`，而 OpenAI Responses/Messages 主路径不写，它的
// 绑定命中只体现在调度决策的 `StickySessionHit` / `StickyPreviousHit` 里。
//
// 所以这里**读不到就按"可能绑定了"处理**（返回 true），两种猜错的后果不对称：
//
//   - 该换却先等 → 多花几秒重试延时，但一定能服务；
//   - 该等却直接换 → 会话连续性被破坏，而且可能换到一个同样没票的账号、最终拒服。
//
// 代价是"新请求本可以立刻换号"这个优化暂时拿不到。要拿到它，得让 OpenAI 调度把真实绑定事实写进
// 当前 attempt 的 context——见 DESIGN §4.6 的遗留。
func KongTicketPreparing(ctx context.Context, err error) (preparing bool, retrySameAccount bool) {
	var denied *KongErrTicketDenied
	if !errors.As(err, &denied) || denied.Reason != KongDenyPreparing {
		return false, false
	}
	if boundID, ok := PrefetchedStickyAccountIDFromContext(ctx); ok && boundID > 0 {
		return true, true
	}
	// 读不到绑定信息：保守地先等本号。见上面的不对称说明。
	return true, true
}

// KongIsPreparingFailover 报告这个 failover 错误是不是「票据正在准备」那一种。
//
// 调度上报要用它：那不是账号的故障，账号本身是好的、只是这一刻没票，几十秒后就补上了。计进错误率
// EWMA 会让"票越缺、账号越被判坏"，与事实相反。
func KongIsPreparingFailover(err error) bool {
	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) || failoverErr == nil {
		return false
	}
	return failoverErr.Reason == GatewayFailureReason(KongDenyPreparing)
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
