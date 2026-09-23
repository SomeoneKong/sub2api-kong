package handler

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
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
// 票据拒服落到通用兜底会呈现成 `502 / upstream_error / "Upstream request failed"`，而实际原因是本系统
// 按策略拒服（如 window_closed，有确定恢复时刻）、一个字节都没发给上游。那个呈现同时带偏三处：排查
// 方向指向上游、供应商质量统计被记一笔、客户端按"网关故障"重试而不是按给定时刻等待。
//
// 挂载点必须是这两个 handler 而不是通用兜底：票据拒服会被包成 failover 错误交给换号框架，终点在
// 这里。挂在 `ensureForwardErrorResponse` 上的话单测照样全绿，真实请求却走不到它。
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

// 票据拒服在错误看板上不得记成上游的锅。
//
// 生产上那 300 条被记成 `error_owner = provider` / `error_source = upstream_http`，而同一行的
// `upstream_status_code` 是 NULL——三者自相矛盾，后果是供应商质量统计被污染、排查方向被带向上游。
// 归因由响应本身决定（502 upstream_error → provider），所以呈现改成 503 之后这条自然就对了；这里
// 钉住它，免得以后改文案或错误类型时静默回归。phase 钉成 routing 而不是 internal：后者指向"网关
// 自己有 bug"，同样是误导。
func TestKongTicketDenyOpsAttributionIsNotProvider(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		name := "responses"
		if anthropic {
			name = "messages"
		}
		t.Run(name, func(t *testing.T) {
			w, c := kongDenyTestContext()
			service.MarkKongTicketDenied(c, time.Now().Add(20*time.Minute).UTC().Format(time.RFC3339))
			h := &OpenAIGatewayHandler{}
			if anthropic {
				h.handleAnthropicFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyWindowClosed), false)
			} else {
				h.handleFailoverExhausted(c, kongDenyFailoverErr(service.KongDenyWindowClosed), false)
			}
			parsed := parseOpsErrorBody(t, w.Body.Bytes())
			phase, _, owner, source := classifyOpsErrorLog(c, parsed.errType, parsed.message, "", w.Code)
			if owner != "platform" || source != "gateway" {
				t.Errorf("归因 = %s/%s, want platform/gateway", owner, source)
			}
			if phase != "routing" {
				t.Errorf("phase = %s, want routing（拿不到合格票等于此刻没有可用账号）", phase)
			}
		})
	}
	// 反面：真实上游故障仍然记在供应商头上，本次改动不得把它一起洗白。
	w, c := kongDenyTestContext()
	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 502}, false)
	parsed := parseOpsErrorBody(t, w.Body.Bytes())
	_, _, owner, source := classifyOpsErrorLog(c, parsed.errType, parsed.message, "", w.Code)
	if owner != "provider" || source != "upstream_http" {
		t.Errorf("真实上游故障的归因 = %s/%s, want provider/upstream_http", owner, source)
	}
}

type opsErrorBody struct {
	errType string
	message string
}

// parseOpsErrorBody 按看板真实的取法从响应体里读错误类型与文案——归因链的输入就是这两个值。
func parseOpsErrorBody(t *testing.T, body []byte) opsErrorBody {
	t.Helper()
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析响应体: %v（%s）", err, body)
	}
	return opsErrorBody{errType: normalizeOpsErrorType(payload.Error.Type, ""), message: payload.Error.Message}
}

// WS 耗尽关闭的码按原因作用域分：账号级"稍后再来"（1013），请求级/系统级用 1008——对一个语义歧义的
// 帧建议客户端稍后重试是错的。两者都要拿到票据分类与看板归因标记。
func TestKongWSTicketDenyCloseStatusByScope(t *testing.T) {
	cases := map[string]coderws.StatusCode{
		service.KongDenyWindowClosed:       coderws.StatusTryAgainLater,
		service.KongDenyNoTicketSource:     coderws.StatusTryAgainLater,
		"frame_undeterminable: duplicate":  coderws.StatusPolicyViolation,
		"ensure_failed: dial tcp: refused": coderws.StatusPolicyViolation,
		service.KongDenyLiveUnsupported:    coderws.StatusPolicyViolation,
	}
	for reason, want := range cases {
		_, c := kongDenyTestContext()
		intendedStatus := 502
		errorType, errorCode, message := "upstream_error", "upstream_ws_failover_exhausted", "upstream websocket proxy failed"
		closeStatus := coderws.StatusInternalError
		ok := kongApplyWSTicketDenyClose(c, kongDenyFailoverErr(reason),
			&intendedStatus, &errorType, &errorCode, &message, &closeStatus)
		if !ok {
			t.Fatalf("%s 应当被认成票据拒服", reason)
		}
		if closeStatus != want {
			t.Errorf("%s 的关闭码 = %v, want %v", reason, closeStatus, want)
		}
		if intendedStatus != 503 {
			t.Errorf("%s 的意图状态码 = %d, want 503", reason, intendedStatus)
		}
		if served, has := service.KongTicketDenyServedReason(c); !has || served == "" {
			t.Errorf("%s 没有记下「最终以票据拒服收场」，看板归因会落回上游", reason)
		}
	}
	// 真实上游故障不走这条路。
	_, c := kongDenyTestContext()
	intendedStatus := 502
	errorType, errorCode, message := "upstream_error", "code", "msg"
	closeStatus := coderws.StatusInternalError
	if kongApplyWSTicketDenyClose(c, &service.UpstreamFailoverError{StatusCode: 500},
		&intendedStatus, &errorType, &errorCode, &message, &closeStatus) {
		t.Error("普通上游故障被当成票据拒服")
	}
}

// Live 的两个终点不经过 failover 耗尽 handler，所以统一呈现要在那里各接一次。
//
// 不接的话：创建落成 `502 / api_error / "Live upstream request failed"`（把本地策略决定说成上游挂了），
// sideband 落成 1011「live sideband closed」（让人去查网关自己）。两处都拿不到票据分类与看板归因标记。
func TestKongLiveTicketDenyPresentation(t *testing.T) {
	h := &OpenAIGatewayHandler{}

	// 创建路径：拒服已被包成 failover 错误，但终点是 writeLiveCreateError。
	w, c := kongDenyTestContext()
	denied := service.KongDenyLiveUnsupported
	h.writeLiveCreateError(c, kongDenyFailoverErr(denied))
	if w.Code != 503 {
		t.Errorf("状态码 = %d, want 503", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, denied) {
		t.Errorf("文案要带分类，实得 %q", body)
	}
	if strings.Contains(body, "Live upstream request failed") {
		t.Errorf("不该再出现那句会引人查上游的文案，实得 %q", body)
	}
	if served, ok := service.KongTicketDenyServedReason(c); !ok || served != denied {
		t.Errorf("没有记下「最终以票据拒服收场」，看板归因会落回上游，实得 %q/%v", served, ok)
	}
	// live_unsupported 没有恢复时刻，不该编一个 Retry-After。
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("这条通路等多久都一样，不该给 Retry-After，实得 %q", got)
	}

	// 反面：真实上游故障仍走原来的映射。
	w2, c2 := kongDenyTestContext()
	h.writeLiveCreateError(c2, &service.UpstreamFailoverError{StatusCode: 500})
	if w2.Code != 502 {
		t.Errorf("普通上游故障要保持原来的 502 映射，实得 %d", w2.Code)
	}

	// sideband 路径：按策略关闭而不是 1011。
	_, c3 := kongDenyTestContext()
	status, reason, ok := kongLiveSidebandDenyClose(c3, &service.KongErrTicketDenied{Reason: denied})
	if !ok {
		t.Fatal("sideband 上的票据拒服要被认出来")
	}
	if status != coderws.StatusPolicyViolation {
		t.Errorf("关闭码 = %v, want 1008", status)
	}
	if !strings.Contains(reason, denied) {
		t.Errorf("关闭文案要带分类，实得 %q", reason)
	}
	if served, has := service.KongTicketDenyServedReason(c3); !has || served != denied {
		t.Errorf("sideband 也要记下归因标记，实得 %q/%v", served, has)
	}
	// 升级之后 HTTP 状态是 101，看板只认带内失败标记。
	if streamErr, has := service.GetOpsStreamError(c3); !has || streamErr.IntendedStatus != 503 ||
		!strings.Contains(streamErr.Code, denied) {
		t.Errorf("sideband 拒服要登记一条带内失败，实得 %+v/%v", streamErr, has)
	}
	if _, _, ok := kongLiveSidebandDenyClose(c3, errors.New("普通错误")); ok {
		t.Error("普通错误不该被当成票据拒服")
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
