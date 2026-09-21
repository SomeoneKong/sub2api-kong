package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// 请求级特征：一条业务请求本身带了什么可用于降智判断的东西。设计见 DESIGN-codex-ticket.md §6.2。
//
// 它与票据页面上的统计回答的是不同问题：那边按 (账号, 模型) 聚合，对不到某一条请求上；一张票服务
// 几十次请求，而 off / observe 模式下客户端自带的 state 根本不进我们的库。
//
// **一律不存原值，只存指纹**：state 是可注入的凭据，管理端点绝不下发它（与票据接口同一条铁律）。

// kongStateFingerprintBytes 是指纹取的哈希字节数（hex 后是它的两倍）。
//
// 6 字节 / 12 个 hex 字符 ≈ 48 bit：一天几千条请求里区分"是不是同一个 state"绰绰有余，
// 而它不可逆、也不足以用来重建原值。
const kongStateFingerprintBytes = 6

// KongRequestFeatures 是一条请求的特征快照，落进 usage_logs 的 kong_request_features（JSONB）。
//
// 全部字段 omitempty：**缺省即"没有这一项"**，不能用零值表达。长度用指针正是为此——`0` 是
// "带了个空串"，与"没带"是两件事。
type KongRequestFeatures struct {
	// StateFP / StateLen 是**实际发往上游**那个 state 的指纹与长度：上游真正收到的东西。
	// full 模式下它是我们注入的票，off / observe 下是客户端自带的那一个。
	StateFP  string `json:"state_fp,omitempty"`
	StateLen *int   `json:"state_len,omitempty"`
	// ClientStateFP / ClientStateLen 是**客户端自己带来**的那个。只在与出站不同（即我们替换掉了它）
	// 时才记——相同的话两份就是同一件事，记两遍只会让人以为发生过替换。
	ClientStateFP  string `json:"client_state_fp,omitempty"`
	ClientStateLen *int   `json:"client_state_len,omitempty"`
	// TicketID 是我们注入的那张票（kong_ticket_cache.id）。票全量入库，所以这里能给精确身份而不是
	// 指纹——凭它可以直接查到那张票的验证状态、采到它的出口与时刻。缺省表示这次没注入。
	TicketID *int64 `json:"ticket_id,omitempty"`
	// ReissuedFP / ReissuedLen / ReissuedTicketID 是上游**在响应里又下发**的 state。
	//
	// 两重意义：对 full 模式，它等于"上游没接受我们注入的那张"（交付会被拦下）；对所有模式，它是
	// 逐请求可见的**上游当前档位读数**（292 正常档 / 312 降智档），比聚合统计更早暴露投放切换。
	//
	// 回发的票会被顺手入库（被动收票），入库了就带上它的 id——凭它能查到这张票后来验成了什么。
	// **入库失败或按规则被拒（如 312 在长度黑名单里）时只有指纹与长度**：那时库里根本没有这张票，
	// 给个 id 就是假的。
	ReissuedFP       string `json:"reissued_fp,omitempty"`
	ReissuedLen      *int   `json:"reissued_len,omitempty"`
	ReissuedTicketID *int64 `json:"reissued_ticket_id,omitempty"`
}

// IsEmpty 报告这份特征一项都没有——那时不该往库里写一个空对象。
func (f *KongRequestFeatures) IsEmpty() bool {
	if f == nil {
		return true
	}
	return f.StateFP == "" && f.StateLen == nil &&
		f.ClientStateFP == "" && f.ClientStateLen == nil &&
		f.TicketID == nil &&
		f.ReissuedFP == "" && f.ReissuedLen == nil && f.ReissuedTicketID == nil
}

// KongFeatureRecorder 是一次上游发送的特征容器。
//
// 需要一个可变容器而不是直接用结构体：请求侧的特征在发送前就定了，而**上游回发的 state 要等响应
// 到达**（AfterUpstream / GuardWSDownstream），两者之间隔着一次上游往返。下行判定与用量行的组装
// 可能在不同 goroutine（WS 尤其如此），所以带锁，并且只对外给不可变快照。
type KongFeatureRecorder struct {
	mu sync.Mutex
	f  KongRequestFeatures
}

// recordOutbound 记下这次实际发往上游的 state 与它的来源。
//
// clientState 是客户端自带的那一个（可能为空）；outbound 是最终上送的那一个。两者相同就只记一份。
func (r *KongFeatureRecorder) recordOutbound(clientState, outbound kongStateObservation, ticketID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if outbound.present {
		r.f.StateFP = kongStateFingerprint(outbound.value)
		r.f.StateLen = kongIntPtr(len(outbound.value))
	}
	// 只在**确实被替换**时才记客户端那份：与出站相同就是同一件事，记两遍会让人以为发生过替换。
	if clientState.present && clientState.value != outbound.value {
		r.f.ClientStateFP = kongStateFingerprint(clientState.value)
		r.f.ClientStateLen = kongIntPtr(len(clientState.value))
	}
	if ticketID > 0 {
		id := ticketID
		r.f.TicketID = &id
	}
}

// RecordReissued 记下上游在响应里又下发的 state 及它入库后的 id（0 表示没入库）。
//
// 两条下行判定（HTTP 响应头 / WS 的 response.metadata 事件）都经这里，口径因此只有一份。
//
// **三项作为一组整体替换**：一轮里可以先收到一张能入库的票、再收到一张按规则拒收的（如 312 落在
// 长度黑名单里）。只在 ticketID > 0 时写 id、却不清旧值，会让新的指纹与长度配上**上一张票的 id**
// ——那是一条指向别的票的假线索，比缺 id 严重。
func (r *KongFeatureRecorder) RecordReissued(state string, ticketID int64) {
	if r == nil {
		return
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.f.ReissuedFP = kongStateFingerprint(state)
	r.f.ReissuedLen = kongIntPtr(len(state))
	r.f.ReissuedTicketID = nil
	if ticketID > 0 {
		id := ticketID
		r.f.ReissuedTicketID = &id
	}
}

// Snapshot 返回当前特征的副本。一项都没有时返回 nil——用量行那一列就该留空，而不是存个空对象。
func (r *KongFeatureRecorder) Snapshot() *KongRequestFeatures {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f.IsEmpty() {
		return nil
	}
	out := r.f
	return &out
}

// kongStateFingerprint 给一个 state 算指纹。空串返回空——"没带"与"带了空串"要能分开，后者靠长度 0
// 表达，指纹对它没有意义。
func kongStateFingerprint(state string) string {
	if state == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:kongStateFingerprintBytes])
}

// kongStateObservation 是一次"载体上有没有 state、是什么"的观测。
//
// **必须带存在性**：光看字符串分不开"没带这个字段"与"带了个空串"，而这两件事的含义完全不同
// （后者是上游/客户端确实发了个空值）。只返回字符串时，约定好的 `state_len: 0` 永远产不出来——
// 那等于代码悄悄背弃了文档与注释里给出的口径。
type kongStateObservation struct {
	value   string
	present bool
}

// kongObservedState 造一个"存在"的观测，值可以是空串。
func kongObservedState(value string) kongStateObservation {
	return kongStateObservation{value: value, present: true}
}

// kongOutboundStateFromHeader 取 HTTP 出站请求头里的 state。
func kongOutboundStateFromHeader(header http.Header) kongStateObservation {
	if header == nil {
		return kongStateObservation{}
	}
	// 多个同名头是歧义状态，准入那侧会拒服（见 kong_ticket_gateway.go）。观测面不做二次判定，
	// 取第一个如实记下。
	values := header.Values(openAICodexTurnStateHeader)
	if len(values) == 0 {
		return kongStateObservation{}
	}
	return kongObservedState(values[0])
}

// kongWSClientMetadataStatePath 是 WS 帧里票所在的 gjson 路径，与注入时的写入路径同源
// （kong_ticket_gateway.go 的 sjson.SetBytes）。
const kongWSClientMetadataStatePath = "client_metadata." + kongWSTurnStateMetadataKey

// kongOutboundStateFromWSFrame 取原生 WS 上送帧里的 state。
func kongOutboundStateFromWSFrame(payload []byte) kongStateObservation {
	if len(payload) == 0 {
		return kongStateObservation{}
	}
	value := gjson.GetBytes(payload, kongWSClientMetadataStatePath)
	if !value.Exists() {
		return kongStateObservation{}
	}
	return kongObservedState(value.String())
}

// kongOutboundStateFromWSMap 取 HTTP→WS 那条路上送的 payload（map 形态）里的 state。
func kongOutboundStateFromWSMap(payload map[string]any) kongStateObservation {
	meta, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		return kongStateObservation{}
	}
	raw, ok := meta[kongWSTurnStateMetadataKey]
	if !ok {
		return kongStateObservation{}
	}
	state, ok := raw.(string)
	if !ok {
		// 值存在但不是字符串：上游读不出票，等同于没带。如实记成"不存在"而不是空串。
		return kongStateObservation{}
	}
	return kongObservedState(state)
}

// kongFeatureContextKey 是特征记录器在**出站请求 context** 里的键。
type kongFeatureContextKey struct{}

// kongWithFeatures 把记录器挂到出站请求的 context 上。
//
// HTTP 这条路上，准入发生在 doOpenAIUpstream（所有上游发送的汇聚点），而用量行在更外层由各协议
// 各自组装——两处之间唯一共有的东西就是**那个出站 request 对象**。挂在它身上，任何一条 HTTP 通路
// 想取特征都只要有 request 就行，不必逐条去接一个新参数（漏接不会报错，只会让那条路永远没特征）。
func kongWithFeatures(ctx context.Context, recorder *KongFeatureRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, kongFeatureContextKey{}, recorder)
}

// KongFeaturesFromRequest 取这次上送记下的请求特征；没有就返回 nil。
func KongFeaturesFromRequest(req *http.Request) *KongRequestFeatures {
	if req == nil {
		return nil
	}
	recorder, _ := req.Context().Value(kongFeatureContextKey{}).(*KongFeatureRecorder)
	return recorder.Snapshot()
}
