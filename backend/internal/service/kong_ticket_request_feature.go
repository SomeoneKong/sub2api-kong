package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
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
	// ReissuedFP / ReissuedLen / ReissuedTicketID 是上游**在响应里又下发**的 state。ReissuedTicketID
	// 缺省只表示"没拿到可确认的库内 id"（拒收 / 落库失败 / 落库结果未知），不代表库里没有这张票。
	//
	// 对 full 模式，它等于"上游没接受我们注入的那张"（交付会被拦下）；对所有模式，它是这一轮上游
	// 下发了新票的逐请求证据。**长度只作观测记录，不代表档位**：上游 state 是不透明 token，档位只能
	// 靠指纹验证判定。
	//
	// 回发的票会被顺手入库（被动收票），入库了就带上它的 id——凭它能查到这张票后来验成了什么。
	// **入库失败时只有指纹与长度**：那时库里根本没有这张票，
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
	// client 是准入时**看到的**客户端那一份（present 为真即看过，哪怕它与出站相同因而刻意没记）。
	//
	// 少了它，"刻意没记"与"还没看过"在字段上长得一样，连接级补记就会把前者当后者：客户端 upgrade
	// 头带 A、帧内带 B 且原样上送 B 时，会补出 state=B / client_state=A——管理页读起来是一次并不存在
	// 的替换。原值留着给 MergeFrameClientState 用。不进 KongRequestFeatures：它只是采集过程的记账，
	// 不该落库。
	client kongStateObservation
	// strippedClient 是被跨账号剥离拿掉的那个客户端原值，暂存到取快照时按显示规则落定
	// （见 RecordStrippedClientState）。不进 KongRequestFeatures：那里只放要落库的字段。
	strippedClient string
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
	if clientState.present && !r.client.present {
		r.client = clientState
	}
	if clientState.present && clientState.value != outbound.value {
		r.f.ClientStateFP = kongStateFingerprint(clientState.value)
		r.f.ClientStateLen = kongIntPtr(len(clientState.value))
	}
	if ticketID > 0 {
		id := ticketID
		r.f.TicketID = &id
	}
}

// RecordOutboundIfAbsent 在"这次上送到底带了什么"还没记下来时补一个值。
//
// 用途只有一个：**原生 WS 上客户端的 state 不在帧里**。它由客户端放在 WebSocket upgrade 的请求头
// 上，再由本服务放到与上游那条连接的握手头上。所以那条路上"本轮实际发往上游的 state"是**连接级**
// 的，逐帧去量必然量不到——kong.11 上线后 ctx_pool 的特征全空就是这个原因。
//
// 传进来的必须是**那条上游连接握手时真正发出去的**那个值（池化通路取
// `lease.SentHandshakeTurnState()`）。不能传客户端本次带来的那一份：连接池复用时本轮根本不发生
// 握手，客户端那个值一个字节都没上送，记成出站就是伪造证据。它属于 RecordClientStateIfAbsent。
//
// **只在缺的时候补**：帧内 client_metadata 里有值时那才是本轮生效的那一个（我们注入就写在那里），
// 连接级的值不能盖掉它。
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

// RecordClientStateIfAbsent 补记**客户端手里那张**，用于连接级载体。
//
// 原生 WS 的客户端把自己那张 state 放在 upgrade 请求头上，帧里没有；而准入只看帧。所以 full 模式
// 在那条路上注入了票之后，"我们替换掉的是客户端哪一张"这条证据会静默丢失——它恰恰是判断客户端
// 手里是正常档还是降级档的唯一读数。
//
// 与 recordOutbound 同一条口径：**只在确实被替换时记**。与出站相同就是同一件事，记两遍会让人以为
// 发生过替换。已经记过就不覆盖（准入那次若已比出差异，它才是权威）。
//
// **出站那一项还空着时也不记**：那说明本轮压根没有 state 上送（off 模式 + 复用连接就是这种形态），
// 此时单独摆一个 client_state 出来，管理页读起来就是"发生过一次并不存在的注入替换"。
//
// **准入已经看过客户端那一份时也不记**，哪怕它当时刻意没记（与出站相同）。帧内才是逐轮的载体，
// 它有值时连接级那个头是上一次握手留下的旧值，补上去等于凭空造出一次替换。
func (r *KongFeatureRecorder) RecordClientStateIfAbsent(state string) {
	if r == nil || state == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client.present || r.f.ClientStateLen != nil || r.f.ClientStateFP != "" {
		return
	}
	if r.f.StateLen == nil && r.f.StateFP == "" {
		return
	}
	fp := kongStateFingerprint(state)
	if r.f.StateFP == fp {
		return
	}
	r.f.ClientStateFP = fp
	r.f.ClientStateLen = kongIntPtr(len(state))
}

// RecordStrippedClientState 记下"客户端确实带了这一份，但被我们剥掉了"（跨账号回放拦截）。
//
// 跨账号剥离发生在票据准入**之前**（不然异账号的 blob 会先被上送），于是准入再去采集"客户端自带那份"时
// 已经什么都看不到了。缺了这一笔有两个后果：full 模式下"客户端手里是什么档"这条读数彻底消失；而 upgrade
// 头里若还留着一个更早的旧值，连接级补记会把它当成"被替换掉的客户端那份"——管理页显示的来源就是错的。
//
// 所以这里**无条件把客户端那份标记为已观测**（我们确实看到了它）。要不要落到 client_state_* 仍按同一条
// 显示规则（本轮有出站 state 才记），但**判定不能在这一刻做完就算数**：出站那一项常常是后补的
// （连接级的握手 state 要等结算时才由 RecordOutboundIfAbsent 补上），此刻按"没有出站"直接丢掉被剥值，
// 等出站补上时"已看过客户端"又把补记挡住，这份证据就永久没了。所以先把它存着，取快照时再按规则落定。
func (r *KongFeatureRecorder) RecordStrippedClientState(state string) {
	if r == nil || state == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.client.present {
		r.client = kongObservedState(state)
	}
	if r.strippedClient == "" {
		r.strippedClient = state
	}
	r.materializeStrippedClientLocked()
}

// materializeStrippedClientLocked 在出站那一项已经就位时，把暂存的被剥值落到 client_state_*。
//
// 规则与 recordOutbound 一致：与出站相同就是同一件事，不记；已经记过就不覆盖。调用方必须持锁。
func (r *KongFeatureRecorder) materializeStrippedClientLocked() {
	if r.strippedClient == "" {
		return
	}
	if r.f.ClientStateLen != nil || r.f.ClientStateFP != "" {
		return
	}
	if r.f.StateLen == nil && r.f.StateFP == "" {
		return
	}
	fp := kongStateFingerprint(r.strippedClient)
	if r.f.StateFP == fp {
		return
	}
	r.f.ClientStateFP = fp
	r.f.ClientStateLen = kongIntPtr(len(r.strippedClient))
}

// MergeFrameClientState 把本记录器（一轮 WS 帧的准入）看到的客户端 state 并进 features，返回合并后的副本。
//
// 给 WS→HTTP bridge 用：那一轮真正上送的是 bridge 另建的 HTTP 请求，它的记录器只看得到出站请求头，而
// 那个头是连接级的 turn-state（首轮取客户端 upgrade 头里本账号那份，后续取上一轮响应带回的），帧内
// client_metadata 才是客户端逐轮带来的那一份。帧里看到过客户端 state 时，client_state_* 以它为准，并按
// 最终出站那一份判"是否被替换"；帧里没看到时保留 HTTP 那侧的观测。出站、注入票与回发三项仍以最终那次
// HTTP 上送为准。
func (r *KongFeatureRecorder) MergeFrameClientState(features *KongRequestFeatures) *KongRequestFeatures {
	if r == nil {
		return features
	}
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if !client.present {
		return features
	}
	var out KongRequestFeatures
	if features != nil {
		out = *features
	}
	out.ClientStateFP, out.ClientStateLen = "", nil
	// 口径与 recordOutbound 一致：本轮没有出站 state 就不单独摆一个 client_state；与出站相同就是同一件事。
	if (out.StateFP != "" || out.StateLen != nil) && kongStateFingerprint(client.value) != out.StateFP {
		out.ClientStateFP = kongStateFingerprint(client.value)
		out.ClientStateLen = kongIntPtr(len(client.value))
	}
	if out.IsEmpty() {
		return nil
	}
	return &out
}

// RecordReissued 记下上游在响应里又下发的 state 及它入库后的 id（0 表示没入库）。
//
// 两条下行判定（HTTP 响应头 / WS 的带内 metadata 事件）都经这里，口径因此只有一份。
//
// **三项作为一组整体替换**：一轮里可以先收到一张能入库的票、再收到一张入库失败的。只在
// ticketID > 0 时写 id、却不清旧值，会让新的指纹与长度配上**上一张票的 id**
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
	// 出站那一项可能是刚刚才补上的（连接级），此刻才轮得到被剥值按规则落定。
	r.materializeStrippedClientLocked()
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
const kongWSClientMetadataStatePath = "client_metadata." + openAIWSTurnStateMetadataKey

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
	raw, ok := meta[openAIWSTurnStateMetadataKey]
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

// WarnMissingRequestFeatures 在一条**门控请求**的用量行没带特征时喊一声。
//
// 这条守卫的由来是一次真实的失手：采集接在了 Responses 的转换路径上，而线上 codex 流量走的是
// 同一个端点的**直通**路径，于是那一列上线后整天全空——而这件事**不会以任何形式报错**，只能靠
// 人去翻库才发现。门控模型有没有 state 这件事是确定的（要么我们注入、要么客户端自带、要么上游
// 回发），所以"门控请求 + 零特征"几乎一定意味着那条通路没接采集，值得一条 Warn。
//
// 只在门控模型上喊：非门控模型只观测，客户端与上游都没给 state 时特征本来就是空的，对它们喊会把
// 日志淹掉、这条守卫也就废了。不产出内容的端点（kongNonGeneratingPathSuffixes）同理：那条路上 state
// 本来就不流转，"必有 state"的前提不成立。
func (g *KongTicketGateway) WarnMissingRequestFeatures(model, inboundEndpoint string, wsMode bool, features *KongRequestFeatures) {
	if g == nil || !g.Enabled() || !features.IsEmpty() {
		return
	}
	if !g.IsGatedModel(model) || kongIsNonGeneratingPath(inboundEndpoint) {
		return
	}
	slog.Warn("kong request features: 门控请求没有采到任何特征，这条通路可能没接采集",
		"model", model, "inbound_endpoint", inboundEndpoint, "ws_mode", wsMode)
}
