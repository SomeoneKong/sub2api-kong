package service

import (
	"strings"

	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// wrapOpenAIWSKongTicketError 把票据错误映射成带明确原因的客户端关闭错误。
//
// 不包的话它会落到通用的 1011「upstream websocket proxy failed」：那是上游故障的说法，而票据
// 拒服与交付拦截都是本机的决定，客户端据此重试只会一直撞同一面墙。用 1008（策略违规）并带上
// 原因，客户端与运维都能一眼看出这是票据门而不是上游挂了。
func wrapOpenAIWSKongTicketError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case KongIsTicketDenied(err):
		return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "codex ticket unavailable for this model", err)
	case KongIsDeliveryBlocked(err):
		return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "codex ticket: refusing unprotected output", err)
	}
	return err
}

// kongWSTurnAction 是一个下行事件对「本轮票据上下文」的处置。
type kongWSTurnAction int

const (
	// kongWSTurnKeep：什么都不做。
	kongWSTurnKeep kongWSTurnAction = iota
	// kongWSTurnBind：把事件里的 response id 记到本轮上下文上，之后才能判断哪些事件属于它。
	kongWSTurnBind
	// kongWSTurnClear：本轮已结束，释放上下文。
	kongWSTurnClear
)

// kongWSResponseIDOf 取一个下行事件所属的**响应 id**。
//
// 只认 `response.id` 与顶层 `response_id` 两种形态（官方客户端两种都发过）。**不能退回顶层 `id`**
// ——很多事件的顶层 `id` 是条目 id（输出项、内容分片）而不是响应 id，拿它去绑定会把本轮钉在一个
// 与终端事件永不相符的值上，于是这一轮永远不被释放，后续非门控轮全被交接判定拒掉。
func kongWSResponseIDOf(payload []byte) string {
	for _, path := range []string{"response.id", "response_id"} {
		if v := strings.TrimSpace(gjson.GetBytes(payload, path).String()); v != "" {
			return v
		}
	}
	return ""
}

// kongWSTurnEventAction 判定一个下行事件该怎么处置本轮的票据上下文。
//
// 两个方向的错都要避免：
//
//   - **释放得太随便**：把任意 `error` 当成本轮结束，则「对上一轮发 response.cancel → 新一轮准入
//     → 取消帧的错误回包到达」这条时序会摘掉**新一轮**的保护，上游随后重发 state 时守卫看到 nil
//     就放行了。
//   - **释放得太吝啬**：只在「已绑定 id 且相符」时释放，则「metadata 不带 id、completed 首次带
//     id」这条**正常**时序下本轮永远不被释放，下一次正常的非门控请求被交接判定拒掉——那是无谓拒服。
//
// 所以规则是：任何带 id 的事件（**含终端**）都先把本轮钉到那个 id 上；终端与 error 只在 id 与本轮
// 相符时释放。`released` 是本连接上已经释放过的响应 id 集合，用来把**迟到或重复**的旧轮终端挡掉
// ——它们的 id 已经在集合里，不会再影响新一轮。完全没有归属信息时（事件无 id、本轮也还没绑上），
// 只有终端类型才释放：那类事件只出现在一轮生成的末尾，而纯 error 可能来自任何一个控制帧。
func kongWSTurnEventAction(attempt *KongUpstreamAttempt, eventType, eventResponseID string, released map[string]bool) kongWSTurnAction {
	if attempt == nil {
		return kongWSTurnKeep
	}
	eventType = strings.TrimSpace(eventType)
	terminal := isOpenAIWSTerminalEvent(eventType)
	ending := terminal || eventType == "error"
	// 已经释放过的那一轮的事件，一概不再影响当前轮。
	if eventResponseID != "" && released[eventResponseID] {
		return kongWSTurnKeep
	}
	if !ending {
		if eventResponseID != "" && attempt.ResponseID == "" {
			return kongWSTurnBind
		}
		return kongWSTurnKeep
	}
	switch {
	case eventResponseID != "" && attempt.ResponseID == eventResponseID:
		return kongWSTurnClear
	case eventResponseID != "" && attempt.ResponseID == "" && terminal:
		// 本轮还没见过任何带 id 的事件，而一个终端事件带着 id 到了。它极可能就是本轮的终端
		// （metadata 不带 id 的那条正常时序）；旧轮的终端会被上面的 released 拦住。
		return kongWSTurnClear
	case eventResponseID == "" && attempt.ResponseID == "" && terminal:
		return kongWSTurnClear
	default:
		return kongWSTurnKeep
	}
}

// GuardWSTurnHandoff 在用新一轮的票据上下文覆盖上一轮之前判定能否交接。
//
// 上一轮受保护、且它的结束事件还没到，说明这条连接在我们看来仍有一轮未落定（例如上游重发了
// 更早一轮的终端事件，把生命周期提前释放了）。此时若用一个**不带保障**的新轮覆盖它，那一轮
// 迟到的 `response.metadata` 就会被拿去跟 nil 比对而放行——本该拦住的输出就交付了。
//
// 新一轮自带合格票时允许覆盖：判定落到新票身上仍是收紧方向（带 state 的 metadata 会撤销新票并
// 拦住交付），不会放过任何一轮。
func (g *KongTicketGateway) GuardWSTurnHandoff(prev, next *KongUpstreamAttempt) error {
	if !g.Enabled() || prev == nil || prev.Grant == nil {
		return nil
	}
	if next != nil && next.Grant != nil {
		return nil
	}
	return &KongErrTicketDenied{Reason: "protected_turn_unresolved"}
}
