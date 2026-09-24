package service

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// codex 票的被动观测在转发链路上的接入点。设计见 DESIGN-codex-ticket.md。
//
// 两件事：记下每一轮发往上游与上游下发的票（请求特征，kong_request_features.go），以及把上游下发的票
// 收进票表（kong_codex_ticket_collector.go）。**只观测、不改变请求**：不注入、不拒服、不拦截交付，任何
// 一步失败都不影响转发。所有方法对 nil 接收者安全：未装配时请求特征与收票一起停掉。
//
// 接入点：
//
//   - HTTP：只有 doOpenAIUpstream 一处（BeforeHTTP / AfterHTTP），那是全部 HTTP 上游发送的汇聚点；
//   - 原生 WS（ctx_pool、HTTP→WS、passthrough 三条通路）：帧不经过那里，各自在上送帧处建记录器、在读到
//     上游事件处调 ObserveWSEvent、在拿到连接后调 CollectHandshake。

// KongTicketObserver 是被动观测的入口。
type KongTicketObserver struct {
	collector *KongTicketCollector
	now       func() time.Time
}

// NewKongTicketObserver 创建观测入口。collector 为 nil 时只记请求特征、不收票。
func NewKongTicketObserver(collector *KongTicketCollector) *KongTicketObserver {
	return &KongTicketObserver{collector: collector, now: time.Now}
}

// SetKongTicketObserver 注入 codex 票的被动观测（可选依赖）。装配后调用一次；传 nil 等于不启用。
func (s *OpenAIGatewayService) SetKongTicketObserver(o *KongTicketObserver) {
	s.kongTicketObserver = o
}

// kongHTTPObservation 是一次 HTTP 上游发送的观测。nil 表示不观测。
type kongHTTPObservation struct {
	o        *KongTicketObserver
	account  *Account
	recorder *KongFeatureRecorder
	// getBody 在发送之前取下：发送路径可能就地替换请求体，收票时要从发送前的明文里读模型。
	getBody func() (io.ReadCloser, error)
}

// BeforeHTTP 在一次 HTTP 上游发送之前记下出站的票，并把记录器挂到出站请求上，调用方据它组装用量行。
//
// 就地换 context 而不是返回一个新请求：调用方持有的就是这个指针，换掉整个对象会让它手里那份失去
// 记录器，而发送尚未开始，此刻没有并发读者。
func (o *KongTicketObserver) BeforeHTTP(req *http.Request, account *Account) *kongHTTPObservation {
	if o == nil || req == nil {
		return nil
	}
	recorder := &KongFeatureRecorder{}
	recorder.recordOutbound(kongOutboundStateFromHeader(req.Header))
	*req = *req.WithContext(kongWithFeatures(req.Context(), recorder))
	return &kongHTTPObservation{o: o, account: account, recorder: recorder, getBody: req.GetBody}
}

// AfterHTTP 在拿到上游响应头之后记下上游下发的票并收票。不看状态码：错误响应里带回的票同样收。
func (h *kongHTTPObservation) AfterHTTP(resp *http.Response) {
	if h == nil || resp == nil {
		return
	}
	state := strings.TrimSpace(extractOpenAICodexTurnState(resp.Header))
	if state == "" {
		return
	}
	h.recorder.RecordReissued(state)
	// 只在真的收到票时才读请求体取模型：多数响应不带票，没必要为每个请求再读一遍请求体。
	h.o.collect(h.account, kongModelFromRequestBody(h.getBody), state)
}

// ObserveWSEvent 处理一帧上游 WS 事件：带内 metadata 带着上游下发的票时，记进这一轮的特征并收票。
//
// 记到**这一帧到达时正在转发的那一轮**（recorder）；recorder 为 nil（归属不明）时只收票。客户端断连后
// 上游照样在发事件、用量照样结算，调用方要在不写客户端的那条路上也调它。
func (o *KongTicketObserver) ObserveWSEvent(account *Account, model string, recorder *KongFeatureRecorder, eventType string, payload []byte) {
	if o == nil || len(payload) == 0 || !isOpenAIWSTurnStateMetadataEvent(eventType) {
		return
	}
	state := strings.TrimSpace(openAIWSTurnStateFromEvent(payload))
	if state == "" {
		return
	}
	recorder.RecordReissued(state)
	o.collect(account, model, state)
}

// CollectHandshake 收一条 WS 连接握手响应里上游给的票：只进票表，不记进任何一轮的特征——它属于整条
// 连接，连接复用时对不上具体哪一轮。池化通路传 `lease.ConsumeHandshakeTurnState()`，一条物理连接只
// 交出一次。
func (o *KongTicketObserver) CollectHandshake(account *Account, model, state string) {
	o.collect(account, model, strings.TrimSpace(state))
}

// collect 把一张上游下发的票交给后台写入。只收走 codex 协议的 OpenAI 账号（OAuth 与 setup token）。
func (o *KongTicketObserver) collect(account *Account, model, state string) {
	if o == nil || o.collector == nil || state == "" || !account.IsOpenAIOAuthLike() {
		return
	}
	o.collector.Enqueue(KongObservedTicket{
		AccountID:  account.ID,
		Model:      strings.TrimSpace(model),
		State:      state,
		CapturedAt: o.now(),
	})
}

// kongModelFromRequestBody 取出站请求体顶层的 model；取不到时返回空串（收票照收，模型记空）。
func kongModelFromRequestBody(getBody func() (io.ReadCloser, error)) string {
	if getBody == nil {
		return ""
	}
	body, err := getBody()
	if err != nil || body == nil {
		return ""
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(raw, "model").String())
}

// kongStringValue 取一个可空字符串指针的值。
func kongStringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// StopKongTicketObserver 在进程退出时停止收票的写入协程。
func (s *OpenAIGatewayService) StopKongTicketObserver() {
	if s != nil && s.kongTicketObserver != nil {
		s.kongTicketObserver.collector.Stop()
	}
}
