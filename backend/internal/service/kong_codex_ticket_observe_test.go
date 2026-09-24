//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

// newKongTestObserver 创建一个带收票队列、但不启动写入协程的观测入口：测试直接从队列里取收到的票。
func newKongTestObserver() (*KongTicketObserver, *KongTicketCollector) {
	collector := NewKongTicketCollector(nil)
	o := NewKongTicketObserver(collector)
	o.now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	return o, collector
}

func kongDrainTickets(c *KongTicketCollector) []KongObservedTicket {
	var out []KongObservedTicket
	for {
		select {
		case t := <-c.queue:
			out = append(out, t)
		default:
			return out
		}
	}
}

func kongTestOAuthAccount() *Account {
	return &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
}

func kongTestResponseWithState(state string) *http.Response {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}
	if state != "" {
		resp.Header.Set(openAICodexTurnStateHeader, state)
	}
	return resp
}

func TestKongObserveHTTP(t *testing.T) {
	t.Run("记下出站与下发的票并收票，模型取发送前的明文请求体", func(t *testing.T) {
		o, c := newKongTestObserver()
		// 出站请求都由 http.NewRequestWithContext 构造，带 GetBody（httptest.NewRequest 不带）。
		req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader([]byte(`{"model":" gpt-6-sol "}`)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(openAICodexTurnStateHeader, "sent")
		obs := o.BeforeHTTP(req, kongTestOAuthAccount())
		// 发送时请求体被就地换掉（压缩）：收票读模型不能读到换过的那份。
		req.Body = io.NopCloser(bytes.NewReader([]byte{0x28, 0xb5}))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte{0x28, 0xb5})), nil }
		obs.AfterHTTP(kongTestResponseWithState(" issued "))

		kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), kongStrPtr("sent"), kongStrPtr("issued"))
		got := kongDrainTickets(c)
		if len(got) != 1 || got[0].AccountID != 7 || got[0].Model != "gpt-6-sol" || got[0].State != "issued" || got[0].CapturedAt.IsZero() {
			t.Fatalf("收到的票 = %+v", got)
		}
	})
	t.Run("错误响应里带回的票同样收", func(t *testing.T) {
		o, c := newKongTestObserver()
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{"model":"m"}`))
		resp := kongTestResponseWithState("issued")
		resp.StatusCode = http.StatusTooManyRequests
		o.BeforeHTTP(req, kongTestOAuthAccount()).AfterHTTP(resp)
		if got := kongDrainTickets(c); len(got) != 1 {
			t.Fatalf("收到的票 = %+v", got)
		}
	})
	t.Run("响应不带票：只有出站那一项，不收票", func(t *testing.T) {
		o, c := newKongTestObserver()
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{"model":"m"}`))
		req.Header.Set(openAICodexTurnStateHeader, "sent")
		o.BeforeHTTP(req, kongTestOAuthAccount()).AfterHTTP(kongTestResponseWithState(""))
		kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), kongStrPtr("sent"), nil)
		if got := kongDrainTickets(c); len(got) != 0 {
			t.Fatalf("不该收票：%+v", got)
		}
	})
	t.Run("非 codex 协议的账号只记特征、不收票", func(t *testing.T) {
		o, c := newKongTestObserver()
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{"model":"m"}`))
		apiKey := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		o.BeforeHTTP(req, apiKey).AfterHTTP(kongTestResponseWithState("issued"))
		kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), nil, kongStrPtr("issued"))
		if got := kongDrainTickets(c); len(got) != 0 {
			t.Fatalf("不该收票：%+v", got)
		}
	})
	t.Run("请求体读不到时模型记空、照样收票", func(t *testing.T) {
		o, c := newKongTestObserver()
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
		req.GetBody = nil
		o.BeforeHTTP(req, kongTestOAuthAccount()).AfterHTTP(kongTestResponseWithState("issued"))
		if got := kongDrainTickets(c); len(got) != 1 || got[0].Model != "" {
			t.Fatalf("收到的票 = %+v", got)
		}
	})
	t.Run("未装配时特征与收票一起停掉", func(t *testing.T) {
		var o *KongTicketObserver
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
		req.Header.Set(openAICodexTurnStateHeader, "sent")
		obs := o.BeforeHTTP(req, kongTestOAuthAccount())
		obs.AfterHTTP(kongTestResponseWithState("issued"))
		if obs != nil || KongFeaturesFromRequest(req) != nil {
			t.Fatal("未装配时不应挂记录器")
		}
		o.ObserveWSEvent(kongTestOAuthAccount(), "m", nil, "response.metadata", []byte(`{}`))
		o.CollectHandshake(kongTestOAuthAccount(), "m", "s")
	})
	t.Run("只记特征、不收票", func(t *testing.T) {
		o := NewKongTicketObserver(nil)
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
		o.BeforeHTTP(req, kongTestOAuthAccount()).AfterHTTP(kongTestResponseWithState("issued"))
		kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), nil, kongStrPtr("issued"))
		o.CollectHandshake(kongTestOAuthAccount(), "m", "s")
	})
}

func TestKongObserveWSEvent(t *testing.T) {
	account := kongTestOAuthAccount()
	t.Run("两种拼写的 metadata 事件都认", func(t *testing.T) {
		o, c := newKongTestObserver()
		r := &KongFeatureRecorder{}
		o.ObserveWSEvent(account, "gpt-6-sol", r, "response.metadata",
			[]byte(`{"type":"response.metadata","response":{"headers":{"x-codex-turn-state":"a"}}}`))
		o.ObserveWSEvent(account, "gpt-6-sol", r, "codex.response.metadata",
			[]byte(`{"type":"codex.response.metadata","headers":{"X-Codex-Turn-State":"b"}}`))
		kongTestFeaturesEqual(t, r.Snapshot(), nil, kongStrPtr("b"))
		got := kongDrainTickets(c)
		if len(got) != 2 || got[0].State != "a" || got[1].State != "b" || got[1].Model != "gpt-6-sol" {
			t.Fatalf("收到的票 = %+v", got)
		}
	})
	t.Run("归属不明时只收票", func(t *testing.T) {
		o, c := newKongTestObserver()
		o.ObserveWSEvent(account, "m", nil, "response.metadata", []byte(`{"headers":{"x-codex-turn-state":"a"}}`))
		if got := kongDrainTickets(c); len(got) != 1 {
			t.Fatalf("收到的票 = %+v", got)
		}
	})
	t.Run("其它事件与不带票的 metadata 不处理", func(t *testing.T) {
		o, c := newKongTestObserver()
		r := &KongFeatureRecorder{}
		o.ObserveWSEvent(account, "m", r, "codex.rate_limits", []byte(`{"headers":{"x-codex-turn-state":"a"}}`))
		o.ObserveWSEvent(account, "m", r, "response.metadata", []byte(`{"headers":{}}`))
		o.ObserveWSEvent(account, "m", r, "response.metadata", nil)
		if r.Snapshot() != nil || len(kongDrainTickets(c)) != 0 {
			t.Fatal("不该有任何记录")
		}
	})
}

// 握手响应里的票只进票表，不记进任何一轮的特征。
func TestKongCollectHandshake(t *testing.T) {
	o, c := newKongTestObserver()
	o.CollectHandshake(kongTestOAuthAccount(), "gpt-6-sol", "  hs  ")
	o.CollectHandshake(kongTestOAuthAccount(), "gpt-6-sol", "")
	got := kongDrainTickets(c)
	if len(got) != 1 || got[0].State != "hs" || got[0].Model != "gpt-6-sol" {
		t.Fatalf("收到的票 = %+v", got)
	}
}

// 经 HTTP 上游发送汇聚点的完整链路：出站请求体被压缩后，特征与收票仍然对得上。
func TestKongDoOpenAIUpstreamObservesTicket(t *testing.T) {
	useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) {
		return kongTestResponseWithState("issued"), nil
	})
	o, c := newKongTestObserver()
	svc.SetKongTicketObserver(o)
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10))
	req.Header.Set(openAICodexTurnStateHeader, "sent")

	if _, err := svc.doOpenAIUpstream(req, "", kongZstdTestAccount()); err != nil {
		t.Fatal(err)
	}
	if len(upstream.sent) != 1 || upstream.sent[0].encoding != "zstd" {
		t.Fatalf("这条用例要覆盖压缩后的请求：%+v", upstream.sent)
	}
	kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), kongStrPtr("sent"), kongStrPtr("issued"))
	if got := kongDrainTickets(c); len(got) != 1 || got[0].Model != "gpt-6-sol" || got[0].State != "issued" {
		t.Fatalf("收到的票 = %+v", got)
	}
}

func TestKongDoOpenAIUpstreamTransportErrorRecordsOutboundOnly(t *testing.T) {
	svc, _ := kongZstdTestService(func(int) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
	o, c := newKongTestObserver()
	svc.SetKongTicketObserver(o)
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, []byte(`{"model":"m"}`))
	req.Header.Set(openAICodexTurnStateHeader, "sent")
	if _, err := svc.doOpenAIUpstream(req, "", kongZstdTestAccount()); err == nil {
		t.Fatal("应返回发送错误")
	}
	kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), kongStrPtr("sent"), nil)
	if len(kongDrainTickets(c)) != 0 {
		t.Fatal("没有响应就没有票")
	}
}

// WS 客户端经 HTTP 上游（bridge）的那一轮，用量结果要带上这次上送记下的请求特征。
//
// 记录器挂在 doOpenAIUpstream 的出站请求上，而 bridge 的结果在发送循环之外构造：不在那里取，用量行就
// 只剩空值。
func TestKongWSHTTPBridgeResultCarriesRequestFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
	reissued := strings.Repeat("r", 780)
	sse := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_k","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{reissued},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream: upstream,
	}
	svc.SetKongTicketObserver(NewKongTicketObserver(nil))

	payload := []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"hi"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "test-token", payload, len(payload),
		"gpt-6-astra", "", "", "", "", 1,
		"",
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatalf("bridge 这一轮不该失败: %v", err)
	}
	kongTestFeaturesEqual(t, result.KongRequestFeatures, nil, &reissued)
}
