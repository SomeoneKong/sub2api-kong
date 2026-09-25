//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const kongReasoningCodexUA = "codex_cli_rs/0.156.0 (Ubuntu 24.4.0; x86_64) xterm-256color"

// kongSetCodexReasoningIncluded 在本用例内固定开关。
func kongSetCodexReasoningIncluded(t *testing.T, enabled bool) {
	t.Helper()
	prev := kongCodexReasoningIncludedEnabled
	kongCodexReasoningIncludedEnabled = func() bool { return enabled }
	t.Cleanup(func() { kongCodexReasoningIncludedEnabled = prev })
}

// kongReasoningContext 构造一次 HTTP 请求的 gin 上下文；ua 为空时不设 User-Agent。
func kongReasoningContext(ctx context.Context, ua string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	if ua != "" {
		c.Request.Header.Set("User-Agent", ua)
	}
	return c, rec
}

func kongReasoningSSEResponse(header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_kong_ri"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_kong_ri","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
	}, "\n")))}
}

func kongReasoningFilter() *responseheaders.CompiledHeaderFilter {
	return responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{})
}

func TestKongParseCodexReasoningIncluded(t *testing.T) {
	for _, tc := range []struct {
		value   string
		present bool
		want    bool
		wantErr bool
	}{
		{present: false, want: true},
		{value: "1", present: true, want: true},
		{value: "true", present: true, want: true},
		{value: " TRUE ", present: true, want: true},
		{value: "0", present: true, want: false},
		{value: "false", present: true, want: false},
		{value: "", present: true, wantErr: true},
		{value: "yes", present: true, wantErr: true},
	} {
		got, err := kongParseCodexReasoningIncluded(tc.value, tc.present)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("value=%q present=%v: got %v err=%v", tc.value, tc.present, got, err)
		}
	}
}

func TestKongCodexReasoningIncludedValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		ua         string
		originator string
		upstream   http.Header
		want       string
	}{
		{name: "codex UA，上游无头", enabled: true, ua: kongReasoningCodexUA, want: "1"},
		{name: "codex-tui UA", enabled: true, ua: "codex-tui/0.156.0", want: "1"},
		{name: "只靠 originator", enabled: true, ua: "curl/8.5.0", originator: "codex_exec", want: "1"},
		{name: "originator 被改写，UA 尾部是官方客户端", enabled: true, ua: "cccc/0.156.0 (Linux; x86_64) xterm (codex-tui; 0.156.0)", want: "1"},
		{name: "沿用上游的值", enabled: true, ua: kongReasoningCodexUA, upstream: http.Header{"X-Reasoning-Included": {"true"}}, want: "true"},
		{name: "非 codex 客户端", enabled: true, ua: "curl/8.5.0"},
		{name: "无 UA 无 originator", enabled: true},
		{name: "非 codex 客户端，上游带头也不经这里写", enabled: true, ua: "curl/8.5.0", upstream: http.Header{"X-Reasoning-Included": {"1"}}},
		{name: "开关关闭", enabled: false, ua: kongReasoningCodexUA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.ua != "" {
				req.Header.Set("User-Agent", tc.ua)
			}
			if tc.originator != "" {
				req.Header.Set("originator", tc.originator)
			}
			require.Equal(t, tc.want, kongCodexReasoningIncludedValue(req, tc.upstream, tc.enabled))
		})
	}
	require.Empty(t, kongCodexReasoningIncludedValue(nil, nil, true))
}

// 被放弃的 attempt 留在 writer 上的值、白名单已经转发过的同一个值，都只剩一份。
func TestKongApplyCodexReasoningIncluded_SetsSingleValue(t *testing.T) {
	kongSetCodexReasoningIncluded(t, true)
	c, _ := kongReasoningContext(context.Background(), kongReasoningCodexUA)
	c.Writer.Header().Add("X-Reasoning-Included", "stale")
	c.Writer.Header().Add("X-Reasoning-Included", "stale")

	KongApplyCodexReasoningIncluded(c, nil)
	require.Equal(t, []string{"1"}, c.Writer.Header().Values("X-Reasoning-Included"))

	KongApplyCodexReasoningIncluded(nil, nil) // nil 上下文不 panic
}

func TestKongApplyCodexReasoningIncludedUnstaged(t *testing.T) {
	kongSetCodexReasoningIncluded(t, true)
	c, _ := kongReasoningContext(context.Background(), kongReasoningCodexUA)
	staged := http.Header{"X-Reasoning-Included": {"true"}, "X-Request-Id": {"req-1"}}

	kongApplyCodexReasoningIncludedUnstaged(staged, c, http.Header{"X-Reasoning-Included": {"true"}})
	require.Equal(t, []string{"true"}, c.Writer.Header().Values("X-Reasoning-Included"), "直接写 writer，不等首输出")
	require.Empty(t, staged.Values("X-Reasoning-Included"), "暂存集合里的那一份要移除，提交时才不会再 Add 一份")
	require.Equal(t, "req-1", staged.Get("X-Request-Id"), "其它暂存头不动")

	kongApplyCodexReasoningIncludedUnstaged(nil, c, nil) // 暂存集合为空时照样写 writer
	require.Equal(t, []string{"1"}, c.Writer.Header().Values("X-Reasoning-Included"))

	other, _ := kongReasoningContext(context.Background(), "curl/8.5.0")
	untouched := http.Header{"X-Reasoning-Included": {"1"}}
	kongApplyCodexReasoningIncludedUnstaged(untouched, other, untouched)
	require.Equal(t, "1", untouched.Get("X-Reasoning-Included"), "不注入时暂存集合保持原样，照旧随暂存头转发")
	require.Empty(t, other.Writer.Header().Values("X-Reasoning-Included"))
}

// Responses 透传的流式与两条非流式出口。
func TestKongCodexReasoningIncluded_Passthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{responseHeaderFilter: kongReasoningFilter()}

	t.Run("流式，上游无头", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), kongReasoningSSEResponse(nil), c, account, time.Now(), "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("流式，上游已带", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		resp := kongReasoningSSEResponse(http.Header{"X-Reasoning-Included": {"true"}})
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"true"}, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("流式，非 codex 客户端", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		c, rec := kongReasoningContext(context.Background(), "curl/8.5.0")
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), kongReasoningSSEResponse(nil), c, account, time.Now(), "model", "model")
		require.NoError(t, err)
		require.Empty(t, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("流式，开关关闭", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, false)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), kongReasoningSSEResponse(nil), c, account, time.Now(), "model", "model")
		require.NoError(t, err)
		require.Empty(t, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("非流式 JSON", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_kong_ri","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))}
		_, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, account, "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("非流式，上游回 SSE", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		_, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), kongReasoningSSEResponse(nil), c, account, "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Header().Values("X-Reasoning-Included"))
	})
}

// 非透传：OpenAI 账号走首输出暂存，其它平台直接写 writer；非流式两条出口。preset 模拟 handler 在
// 排队之前预写的值。
func TestKongCodexReasoningIncluded_Native(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}

	for _, tc := range []struct {
		name     string
		platform string
		filter   *responseheaders.CompiledHeaderFilter
		preset   bool
		upstream http.Header
		want     []string
	}{
		{name: "暂存分支，暂存头为空", platform: PlatformOpenAI, want: []string{"1"}},
		{name: "暂存分支，上游已带", platform: PlatformOpenAI, filter: kongReasoningFilter(), upstream: http.Header{"X-Reasoning-Included": {"true"}}, want: []string{"true"}},
		{name: "暂存分支，预写值与上游值合成一份", platform: PlatformOpenAI, filter: kongReasoningFilter(), preset: true, upstream: http.Header{"X-Reasoning-Included": {"true"}}, want: []string{"true"}},
		{name: "非暂存分支", platform: "", filter: kongReasoningFilter(), want: []string{"1"}},
		{name: "非暂存分支，预写值与上游值合成一份", platform: "", filter: kongReasoningFilter(), preset: true, upstream: http.Header{"X-Reasoning-Included": {"true"}}, want: []string{"true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kongSetCodexReasoningIncluded(t, true)
			svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: tc.filter}
			c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
			if tc.preset {
				KongApplyCodexReasoningIncluded(c, nil)
			}
			_, err := svc.handleStreamingResponse(c.Request.Context(), kongReasoningSSEResponse(tc.upstream), c, &Account{ID: 1, Platform: tc.platform}, time.Now(), "model", "model")
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), "response.completed")
			require.Equal(t, tc.want, rec.Result().Header.Values("X-Reasoning-Included"))
		})
	}

	t.Run("非流式 JSON", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: kongReasoningFilter()}
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_kong_ri","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))}
		_, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Header().Values("X-Reasoning-Included"))
	})
	t.Run("非流式，上游回 SSE", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: kongReasoningFilter()}
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		_, err := svc.handleNonStreamingResponse(c.Request.Context(), kongReasoningSSEResponse(nil), c, &Account{ID: 1, Platform: PlatformOpenAI}, "model", "model")
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Header().Values("X-Reasoning-Included"))
	})
}

// 首输出前的 keepalive 会先提交响应头，暂存的头随之作废；这个头不走暂存，照样送达。
func TestKongCodexReasoningIncluded_KeepaliveBeforeFirstOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	kongSetCodexReasoningIncluded(t, true)
	svc := &OpenAIGatewayService{
		cfg:                  &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1}},
		responseHeaderFilter: kongReasoningFilter(),
	}
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_kong_ri_slow"}}` + "\n\n"))
		time.Sleep(1500 * time.Millisecond)
		_, _ = pw.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))
		_, _ = pw.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_kong_ri_slow","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))
	}()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"req-slow"}}, Body: pr}
	c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")
	require.NoError(t, err)
	body := rec.Body.String()
	keepaliveAt := strings.Index(body, ":\n\n")
	require.GreaterOrEqual(t, keepaliveAt, 0, "这条用例要让 keepalive 先于首输出")
	require.Less(t, keepaliveAt, strings.Index(body, "response.output_text.delta"), "这条用例要让 keepalive 先于首输出")
	committed := rec.Result().Header
	require.Empty(t, committed.Get("X-Request-Id"), "账号相关的暂存头在 keepalive 之后本就不再下发")
	require.Equal(t, []string{"1"}, committed.Values("X-Reasoning-Included"))
}

// HTTP→WS（forwardOpenAIWSV2）不写这个头：handler 预写的值原样送达，成功与错误响应都只有一份。
func TestKongCodexReasoningIncluded_WSv2KeepsPresetValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	forward := func(t *testing.T, stream bool, events ...string) (*httptest.ResponseRecorder, error) {
		t.Helper()
		frames := make([][]byte, 0, len(events))
		for _, event := range events {
			frames = append(frames, []byte(event))
		}
		dialer := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{events: frames}, handshake: http.Header{}}
		svc, _ := kongWSTicketPoolService(t, kongWSTicketPoolConfig(), dialer)
		c, rec := kongReasoningContext(context.Background(), kongReasoningCodexUA)
		KongApplyCodexReasoningIncluded(c, nil)
		body := []byte(fmt.Sprintf(`{"model":%q,"stream":%v,"input":[{"type":"input_text","text":"hi"}]}`, kongWSTicketModel, stream))
		result, err := svc.Forward(c.Request.Context(), c, kongWSTicketPoolOAuthAccount(7601), body)
		if err == nil {
			require.True(t, result.OpenAIWSMode, "这条用例要走 HTTP→WS，不能回退 HTTP")
		}
		return rec, err
	}

	t.Run("流式", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		rec, err := forward(t, true, kongWSTicketCompleted("resp_kong_ri_ws1"))
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Result().Header.Values("X-Reasoning-Included"))
	})
	t.Run("非流式", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		rec, err := forward(t, false, kongWSTicketCompleted("resp_kong_ri_ws2"))
		require.NoError(t, err)
		require.Equal(t, []string{"1"}, rec.Result().Header.Values("X-Reasoning-Included"))
	})
	t.Run("非流式错误分支", func(t *testing.T) {
		kongSetCodexReasoningIncluded(t, true)
		rec, err := forward(t, false, `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"invalid input"}}`)
		require.Error(t, err)
		require.GreaterOrEqual(t, rec.Code, http.StatusBadRequest)
		require.Equal(t, []string{"1"}, rec.Result().Header.Values("X-Reasoning-Included"), "codex 不读错误响应上的这个头，带着无妨")
	})
}
