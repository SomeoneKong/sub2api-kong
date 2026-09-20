package handler

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func kongDenyTestContext() (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return w, c
}

func kongDenyFailoverErr(reason string) *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:             0,
		RequestScopedTransient: true,
		Scope:                  service.GatewayFailureScopeAccount,
		Reason:                 service.KongTicketDenyFailoverReason(reason),
	}
}

// 票据拒服必须在**真实的 failover 耗尽路径**上被呈现成 503。
//
// 生产上这条路径产出过 300 条 `502 / upstream_error / "Upstream request failed"`，而实际原因是本系统
// 按策略拒服（window_closed，有确定恢复时刻）、一个字节都没发给上游。那个呈现同时带偏三处：排查方向
// 指向上游、供应商质量统计被记一笔、客户端按"网关故障"重试而不是按给定时刻等待。
//
// 挂载点必须是这两个 handler 而不是通用兜底：票据拒服会被包成 failover 错误交给换号框架，终点在
// 这里。上一版挂在 `ensureForwardErrorResponse` 上，单测全绿而生产照旧 502。
func TestKongTicketDenyPresentedAtRealExhaustion(t *testing.T) {
	retryAt := time.Now().Add(27 * time.Minute).UTC().Format(time.RFC3339)
	for _, anthropic := range []bool{false, true} {
		name := "responses"
		if anthropic {
			name = "messages"
		}
		t.Run(name, func(t *testing.T) {
			w, c := kongDenyTestContext()
			service.MarkKongTicketDenied(c, retryAt)
			h := &OpenAIGatewayHandler{}
			if anthropic {
				h.handleAnthropicFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyWindowClosed), false)
			} else {
				h.handleFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyWindowClosed), false)
			}
			// 503 而不是 502：502 的语义是"上游返回了无效响应"，而这里没有上游交互。
			if w.Code != 503 {
				t.Errorf("状态码 = %d, want 503", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, "Upstream request failed") {
				t.Errorf("不该再出现那句会引人查上游的文案，实得 %q", body)
			}
			if !strings.Contains(body, service.KongDenyWindowClosed) {
				t.Errorf("消息里要带 deny_reason，实得 %q", body)
			}
			if !strings.Contains(body, retryAt) {
				t.Errorf("消息里要带最早可重试时刻，实得 %q", body)
			}
			secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
			if err != nil {
				t.Fatalf("Retry-After 应当是秒数，实得 %q", w.Header().Get("Retry-After"))
			}
			if secs < 60 || secs > 27*60 {
				t.Errorf("Retry-After = %ds，与 27 分钟后的恢复时刻不符", secs)
			}
		})
	}
}

// 带自由文本的拒服原因（`ensure_failed: <err>`）也要被认出来，且**错误详情不得进客户端文案**。
func TestKongTicketDenyFreeTextReasonNormalized(t *testing.T) {
	w, c := kongDenyTestContext()
	service.MarkKongTicketDenied(c, "")
	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, kongDenyFailoverErr("ensure_failed: dial tcp 10.0.0.1:5432: connect: refused"), false)
	if w.Code != 503 {
		t.Errorf("状态码 = %d, want 503", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "ensure_failed") {
		t.Errorf("分类要保留，实得 %q", body)
	}
	if strings.Contains(body, "10.0.0.1") {
		t.Errorf("错误详情不得流进客户端文案，实得 %q", body)
	}
}

// 前一个账号的拒服不得解释后一个账号的真实故障——这是本次修复的镜像方向。
func TestKongTicketDenyDoesNotHijackOrdinaryFailure(t *testing.T) {
	w, c := kongDenyTestContext()
	service.MarkKongTicketDenied(c, time.Now().Add(20*time.Minute).UTC().Format(time.RFC3339))
	h := &OpenAIGatewayHandler{}
	// 换到下一个账号后是真实的上游 500，终止错误与票据无关。
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 500}, false)
	if w.Code == 503 || strings.Contains(w.Body.String(), service.KongDenyWindowClosed) {
		t.Errorf("真实上游故障被呈现成票据拒服：status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "" {
		t.Errorf("不该带上一个账号的等待时间，实得 %q", w.Header().Get("Retry-After"))
	}
}

// 整池等待取**已尝试账号中最早**的恢复时刻，不是最后访问那个的。
func TestKongTicketDenyWaitTakesEarliest(t *testing.T) {
	early := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	late := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	w, c := kongDenyTestContext()
	service.MarkKongTicketDenied(c, early)
	service.MarkKongTicketDenied(c, late)
	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyWindowClosed), false)
	if !strings.Contains(w.Body.String(), early) {
		t.Errorf("应当给最早的恢复时刻 %s，实得 %q", early, w.Body.String())
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs > 120 {
		t.Errorf("Retry-After 应当对应一分钟后，实得 %q", w.Header().Get("Retry-After"))
	}
}

// 任一被拒账号恢复时刻未知时不给 Retry-After：未知不等于很久，拿别人的长等待挡客户端会把一个
// 几十秒后就有票的池子搁置二十分钟。
func TestKongTicketDenyUnknownRecoverySuppressesRetryAfter(t *testing.T) {
	w, c := kongDenyTestContext()
	service.MarkKongTicketDenied(c, time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339))
	service.MarkKongTicketDenied(c, "")
	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyPreparing), false)
	if w.Code != 503 {
		t.Errorf("状态码 = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("恢复时刻未知时不该设 Retry-After，实得 %q", got)
	}
	if strings.Contains(w.Body.String(), "earliest retry") {
		t.Errorf("文案不该给一个不成立的等待时间，实得 %q", w.Body.String())
	}
}

// 恢复时刻缺失或已经过去时不设 Retry-After：给一个 0 或负数会让客户端立刻重试，而那一定再失败一次。
func TestKongRetryAfterSecondsEdgeCases(t *testing.T) {
	cases := map[string]string{
		"空值":       "",
		"解析不了":     "not-a-timestamp",
		"已经过去":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"恰好现在（边界）": time.Now().UTC().Format(time.RFC3339),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := kongRetryAfterSeconds(in); got != "" {
				t.Errorf("应当不设 Retry-After，实得 %q", got)
			}
		})
	}
	if got := kongRetryAfterSeconds(time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339)); got == "" {
		t.Error("将来的恢复时刻应当换算成秒数")
	}
}

// 没有票据拒服标记时，兜底仍走原来那条路——本次改动不得影响真实的上游故障。
func TestKongTicketDenyWaitAbsent(t *testing.T) {
	if wait := service.KongTicketDenyWaitFromContext(nil); wait != nil {
		t.Error("nil context 不该返回等待信息")
	}
}
