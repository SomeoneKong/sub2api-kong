package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 票据拒服的客户端呈现。放在 fork 自有文件里，上游那两个耗尽 handler 只留一个四行的挂载点。
//
// 为什么必须挂在**耗尽 handler** 上而不是通用兜底（`ensureForwardErrorResponse`）：票据拒服会被包成
// `*UpstreamFailoverError` 交给 failover 框架换号，所以它的终点是 `handleFailoverExhausted` /
// `handleAnthropicFailoverExhausted`——那两条路压根不经过通用兜底。挂错地方的净效果是"单测全绿、
// 生产照旧返回 502"。

// kongWriteTicketDenyExhausted 在 failover 耗尽（所有账号都没票）时写票据拒服的响应。
//
// 返回 false 表示这次终止与票据无关，调用方继续走原来的上游错误映射。识别只看**当前终止错误**：
// 换号过程中前一个账号的拒服不得用来解释后一个账号的真实故障。
func (h *OpenAIGatewayHandler) kongWriteTicketDenyExhausted(
	c *gin.Context,
	failoverErr *service.UpstreamFailoverError,
	streamStarted bool,
	anthropic bool,
) bool {
	reason, ok := service.KongTicketDenyReasonOf(failoverErr)
	if !ok {
		return false
	}
	status, errType, msg, retryAfter := kongTicketDenyResponse(reason, service.KongTicketDenyWaitFromContext(c))
	if retryAfter != "" && !streamStarted {
		// Retry-After 只能在头还没发出去时设。流已开始时这条信息进错误消息文本。
		c.Header("Retry-After", retryAfter)
	}
	// 刻意不调 SetOpsUpstreamError：没有上游响应可记，记了只是在错误看板上多一条指向上游的假证据。
	if anthropic {
		h.anthropicStreamingAwareError(c, status, "api_error", msg, streamStarted)
		return true
	}
	h.handleStreamingAwareError(c, status, errType, msg, streamStarted)
	return true
}

// kongTicketDenyResponse 把一次票据拒服翻成给客户端的响应。
//
// **503 而不是 502**：502 的语义是"上游网关返回了无效响应"，而这里压根没有上游交互；503 是
// "服务暂时不可用"，配 Retry-After 正是这个场景的标准表达，客户端的通用重试逻辑也认它。
//
// 文案说的是"最早可再来"，**不是"保证那时恢复"**：恢复时刻只表示静默/冷却在那一刻期满，届时能否
// 取到合格票仍取决于上游。承诺恢复会让客户端把一次仍然失败的重试当成系统失信。
func kongTicketDenyResponse(reason string, wait *service.KongTicketDenyWait) (status int, errType, msg, retryAfter string) {
	msg = "Model access is temporarily gated: no verified upstream ticket is available in this account pool"
	// 只有**全部**被拒账号都给出了恢复时刻，才敢把最早那个当整池的等待下限。有一个未知就不给
	// ——未知不等于很久，拿别人的长等待去挡客户端会把一个几十秒后就有票的池子搁置二十分钟。
	earliest := ""
	if wait != nil && !wait.Unknown {
		earliest = wait.Earliest
	}
	if earliest != "" {
		msg += " (earliest retry " + earliest + ")"
	}
	// reason 一并给出，便于对着错误看板与票据页排查；它是规范化后的分类，不含凭据与错误详情。
	if reason != "" {
		msg += " [reason: " + reason + "]"
	}
	return http.StatusServiceUnavailable, "service_unavailable", msg, kongRetryAfterSeconds(earliest)
}

// kongRetryAfterSeconds 把 RFC3339 的恢复时刻换成 Retry-After 的秒数。
//
// 按 HTTP 规范给秒数而不是日期：日期形式要求客户端与服务端时钟一致，而两者可以差好几分钟——那会让
// 客户端算出一个已经过去或过分靠后的等待。解析不了或已经过去时返回空串，让调用方不设这个头
// （给 0 或负数会让客户端立刻重试、必然再失败一次）。
func kongRetryAfterSeconds(retryAfter string) string {
	if retryAfter == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, retryAfter)
	if err != nil {
		return ""
	}
	secs := int(time.Until(at).Seconds())
	if secs <= 0 {
		return ""
	}
	return strconv.Itoa(secs)
}
