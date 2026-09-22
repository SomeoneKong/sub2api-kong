//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 握手是**物理连接**一生一次的事，而一条连接会被多个会话/请求反复租用。
//
// 上游在握手响应里给的那张票因此只许被收走一次：每次租用读到的都是同一个值，反复收会把同一张票
// 反复入库（noise），同连接换门控模型时还会把旧握手票记到另一个模型名下——连接兼容键里没有模型，
// 复用完全允许跨模型。
func TestOpenAIWSConnConsumeHandshakeTurnStateOnce(t *testing.T) {
	conn := newOpenAIWSConn("kong_handshake_once", 1, &openAIWSFakeConn{}, http.Header{
		http.CanonicalHeaderKey(openAIWSTurnStateHeader): []string{"  upstream-state  "},
	})
	lease := &openAIWSConnLease{conn: conn, reused: false}

	if got := lease.ConsumeHandshakeTurnState(); got != "upstream-state" {
		t.Fatalf("第一次租用应当收到握手票，实得 %q", got)
	}
	// 同一条物理连接的后续租用（复用、换模型都走这里）不得再收。
	reusedLease := &openAIWSConnLease{conn: conn, reused: true}
	if got := reusedLease.ConsumeHandshakeTurnState(); got != "" {
		t.Errorf("同一条连接重复收票了：%q", got)
	}
	// 读取器仍然能看到那个头，只是不再作为"待收样本"。
	if got := lease.HandshakeHeader(openAIWSTurnStateHeader); got != "upstream-state" {
		t.Errorf("消费不该改动握手响应头：%q", got)
	}

	nilLease := (*openAIWSConnLease)(nil)
	if got := nilLease.ConsumeHandshakeTurnState(); got != "" {
		t.Errorf("nil lease 必须返回空：%q", got)
	}
}

// 「本轮实际发往上游的 state」只能是拨号时真正放进 upgrade 请求头的那一个。
//
// 这条**走真实的 Acquire → dialConn**，不手工给字段赋值：赋值点在生产代码里，手工造 conn 等于把它
// 从测试覆盖里摘出去。断言三件事——发出去的那份取自 HeadersFactory **之后**的头（per-dial 凭据就靠
// 它）；复用连接仍报告建连时那份（本轮没握手，客户端本次带来的值一个字节都没上送）；上游握手响应
// 里的票只在第一次租用时收得到。
func TestOpenAIWSPoolRecordsSentHandshakeTurnState(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
	pool := newOpenAIWSConnPool(cfg)
	// 构造池会起后台 worker，Release 只归还租约、不停 worker；不关池的话每次跑都会攒 goroutine。
	t.Cleanup(pool.Close)
	dialer := &openAIWSCaptureDialer{
		conn: &openAIWSCaptureConn{},
		handshake: http.Header{
			http.CanonicalHeaderKey(openAIWSTurnStateHeader): []string{"upstream-state"},
		},
	}
	pool.setClientDialerForTest(dialer)

	account := &Account{ID: 4242, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	req := openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.com/v1/responses",
		Headers: http.Header{
			http.CanonicalHeaderKey(openAIWSTurnStateHeader): []string{"stale-template-state"},
		},
		// per-dial 改写：真正发出去的是这一份。
		HeadersFactory: func(_ context.Context, h http.Header) (http.Header, error) {
			out := cloneHeader(h)
			out.Set(openAIWSTurnStateHeader, "sent-on-dial")
			return out, nil
		},
	}

	first, err := pool.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("首次取连接失败: %v", err)
	}
	if got := first.SentHandshakeTurnState(); got != "sent-on-dial" {
		t.Errorf("发出去的那份应取自 HeadersFactory 之后的头，实得 %q", got)
	}
	if got := first.ConsumeHandshakeTurnState(); got != "upstream-state" {
		t.Errorf("首次租用应收到上游握手票，实得 %q", got)
	}
	firstConnID := first.ConnID()
	first.Release()

	second, err := pool.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("二次取连接失败: %v", err)
	}
	defer second.Release()
	if second.ConnID() != firstConnID || !second.Reused() {
		t.Fatalf("这一轮本该复用同一条连接：reused=%v conn=%s→%s", second.Reused(), firstConnID, second.ConnID())
	}
	if got := second.SentHandshakeTurnState(); got != "sent-on-dial" {
		t.Errorf("复用连接仍应报告建连时发出去的那份，实得 %q", got)
	}
	if got := second.ConsumeHandshakeTurnState(); got != "" {
		t.Errorf("同一条物理连接重复收票了：%q", got)
	}
	if n := dialer.DialCount(); n != 1 {
		t.Errorf("本用例只该拨号一次，实得 %d", n)
	}
}

// 没发过 state 的连接必须报告空，nil lease 同样。
func TestOpenAIWSConnSentHandshakeTurnStateAbsent(t *testing.T) {
	empty := &openAIWSConnLease{conn: newOpenAIWSConn("kong_handshake_none", 1, &openAIWSFakeConn{}, nil)}
	if got := empty.SentHandshakeTurnState(); got != "" {
		t.Errorf("没发过 state 的连接必须返回空：%q", got)
	}
	nilLease := (*openAIWSConnLease)(nil)
	if got := nilLease.SentHandshakeTurnState(); got != "" {
		t.Errorf("nil lease 必须返回空：%q", got)
	}
}
