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

// codex 票的请求特征：一轮里**实际发往上游**的票与上游在**这一轮响应里下发**的票，各自的长度与指纹，
// 落进 usage_logs 的 kong_request_features（JSONB）。设计见 DESIGN-codex-ticket.md。
//
// 票是上游的不透明 token，长度与指纹只作观测记录，不参与任何判定。**只存指纹不存原值**：原值只进票表
// （kong_codex_ticket_collector.go），用量表与界面都不放。

// kongStateFingerprintBytes 是指纹取的哈希字节数（hex 后是它的两倍）。
//
// 6 字节 / 12 个 hex 字符 ≈ 48 bit：足以区分相邻几轮是不是同一张票，也足以按内容在票表里找到它，
// 而它不可逆、不足以重建原值。
const kongStateFingerprintBytes = 6

// KongRequestFeatures 是一轮的特征快照。
//
// 全部字段 omitempty：**缺省即"没有这一项"**，不能用零值表达。长度用指针正是为此——`0` 是
// "带了个空串"，与"没带"是两件事。
type KongRequestFeatures struct {
	// StateFP / StateLen 是这一轮实际发往上游的票：上游真正收到的东西。
	StateFP  string `json:"state_fp,omitempty"`
	StateLen *int   `json:"state_len,omitempty"`
	// ReissuedFP / ReissuedLen 是上游在这一轮响应里下发的票；一轮里下发了多张时是最后一张。
	ReissuedFP  string `json:"reissued_fp,omitempty"`
	ReissuedLen *int   `json:"reissued_len,omitempty"`
}

// IsEmpty 报告这份特征一项都没有——那时不该往库里写一个空对象。
func (f *KongRequestFeatures) IsEmpty() bool {
	if f == nil {
		return true
	}
	return f.StateFP == "" && f.StateLen == nil && f.ReissuedFP == "" && f.ReissuedLen == nil
}

// KongFeatureRecorder 是一轮的特征容器，所有方法对 nil 接收者安全。
//
// 需要可变容器而不是结构体：发往上游的那一项在发送前就定了，而上游下发的那一项要等响应到达，下行处理
// 与用量行的组装可能在不同 goroutine（WS 尤其如此），所以带锁，并且只对外给不可变快照。**逐轮新建，
// 不跨轮复用**：沿用上一轮的记录器会让这一轮的用量行顶着上一轮的票。
type KongFeatureRecorder struct {
	mu sync.Mutex
	f  KongRequestFeatures
}

// recordOutbound 记下这一轮实际发往上游的票。
func (r *KongFeatureRecorder) recordOutbound(outbound kongStateObservation) {
	if r == nil || !outbound.present {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.f.StateFP = kongStateFingerprint(outbound.value)
	r.f.StateLen = kongIntPtr(len(outbound.value))
}

// RecordOutboundIfAbsent 在"这一轮上送了什么"还没记下时，补上连接级的那个值。
//
// 原生 WS 上客户端的票可以不在帧里，而在 WebSocket upgrade 的请求头上，由本服务放进与上游那条连接的
// 握手头。所以帧里没有时，本轮实际发往上游的是**那条连接握手时真正发出去的**值（池化通路取
// `lease.SentHandshakeTurnState()`）。不能传客户端本次带来的那一份：连接池复用时本轮根本不握手，那个值
// 一个字节都没上送。
//
// **只在缺的时候补**：帧内有值时那才是本轮生效的那一个。连接级的空串按缺省处理——upgrade 头带空值与
// 不带，上游看到的是同一件事。
func (r *KongFeatureRecorder) RecordOutboundIfAbsent(state string) {
	if r == nil || state == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f.StateLen != nil || r.f.StateFP != "" {
		return
	}
	r.f.StateFP = kongStateFingerprint(state)
	r.f.StateLen = kongIntPtr(len(state))
}

// RecordReissued 记下上游在这一轮响应里下发的票。同一轮下发多张时后一张覆盖前一张。
func (r *KongFeatureRecorder) RecordReissued(state string) {
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
}

// Snapshot 返回当前特征的副本。一项都没有时返回 nil——用量行那一列就该留空，而不是存个空对象。
//
// 快照是副本：用量行组装之后再到达的事件改的是记录器，不会改动已经落定的那一行。
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

// kongStateFingerprint 给一张票算指纹。空串返回空——"没带"与"带了空串"靠长度 0 区分，指纹对它没有意义。
func kongStateFingerprint(state string) string {
	if state == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:kongStateFingerprintBytes])
}

// kongStateObservation 是一次"载体上有没有票、是什么"的观测。
//
// **必须带存在性**：光看字符串分不开"没带这个字段"与"带了个空串"。
type kongStateObservation struct {
	value   string
	present bool
}

// kongOutboundStateFromHeader 取 HTTP 出站请求头里的票。多个同名头时取第一个如实记下。
func kongOutboundStateFromHeader(header http.Header) kongStateObservation {
	if header == nil {
		return kongStateObservation{}
	}
	values := header.Values(openAICodexTurnStateHeader)
	if len(values) == 0 {
		return kongStateObservation{}
	}
	return kongStateObservation{value: values[0], present: true}
}

// kongWSClientMetadataStatePath 是 WS 帧里票所在的 gjson 路径。
const kongWSClientMetadataStatePath = "client_metadata." + openAIWSTurnStateMetadataKey

// kongOutboundStateFromWSFrame 取原生 WS 上送帧里的票。
func kongOutboundStateFromWSFrame(payload []byte) kongStateObservation {
	if len(payload) == 0 {
		return kongStateObservation{}
	}
	value := gjson.GetBytes(payload, kongWSClientMetadataStatePath)
	if !value.Exists() {
		return kongStateObservation{}
	}
	return kongStateObservation{value: value.String(), present: true}
}

// kongOutboundStateFromWSMap 取 HTTP→WS 那条路上送的 payload（map 形态）里的票。
func kongOutboundStateFromWSMap(payload map[string]any) kongStateObservation {
	meta, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		return kongStateObservation{}
	}
	raw, ok := meta[openAIWSTurnStateMetadataKey]
	if !ok {
		return kongStateObservation{}
	}
	state, ok := raw.(string)
	if !ok {
		// 值存在但不是字符串：上游读不出票，等同于没带。
		return kongStateObservation{}
	}
	return kongStateObservation{value: state, present: true}
}

// KongNewWSFrameFeatures 为原生 WS 的一轮建记录器，发往上游的那一项取这一帧 client_metadata 里的票。
// 调用方传入的必须是跨账号剥离之后、实际上送的那一帧。
func KongNewWSFrameFeatures(payload []byte) *KongFeatureRecorder {
	r := &KongFeatureRecorder{}
	r.recordOutbound(kongOutboundStateFromWSFrame(payload))
	return r
}

// KongNewWSMapFeatures 与 KongNewWSFrameFeatures 相同，用于 HTTP→WS 那条路的 map 形态 payload。
func KongNewWSMapFeatures(payload map[string]any) *KongFeatureRecorder {
	r := &KongFeatureRecorder{}
	r.recordOutbound(kongOutboundStateFromWSMap(payload))
	return r
}

// kongFeatureContextKey 是特征记录器在**出站请求 context** 里的键。
type kongFeatureContextKey struct{}

// kongWithFeatures 把记录器挂到出站请求的 context 上。
//
// HTTP 这条路上，测量发生在 doOpenAIUpstream（所有 HTTP 上游发送的汇聚点），而用量行在更外层由各协议
// 各自组装——两处之间唯一共有的东西就是**那个出站 request 对象**。
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

func kongIntPtr(v int) *int { return &v }
