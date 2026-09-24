package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const kongZstdTestURL = "https://chatgpt.com/backend-api/codex/responses"

// 包内测试默认不压缩：上游的转发测试记下发往 chatgpt.com 的正文后按明文 JSON 断言，而压缩只是
// 传输层的变换，由本文件的测试经 useKongZstd 显式打开后覆盖。本文件不带 build tag，两种口径都生效。
func init() {
	kongOpenAIRequestZstdInstance = func() *kongOpenAIRequestZstd { return nil }
}

type kongZstdTestClock struct{ now time.Time }

func (c *kongZstdTestClock) Now() time.Time { return c.now }

// useKongZstd 在本测试期间替换生效的压缩实例。
func useKongZstd(t *testing.T, enabled bool) (*kongOpenAIRequestZstd, *kongZstdTestClock) {
	t.Helper()
	clock := &kongZstdTestClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	z := newKongOpenAIRequestZstd(enabled, clock.Now)
	previous := kongOpenAIRequestZstdInstance
	kongOpenAIRequestZstdInstance = func() *kongOpenAIRequestZstd { return z }
	t.Cleanup(func() { kongOpenAIRequestZstdInstance = previous })
	return z, clock
}

func kongZstdTestBody(size int) []byte {
	prefix := `{"model":"gpt-6-sol","stream":true,"input":"`
	return []byte(prefix + strings.Repeat("hello codex ", size/12+1) + `"}`)
}

func newKongZstdTestRequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func kongZstdDecode(t *testing.T, data []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(data, nil)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	return plain
}

type kongZstdSent struct {
	encoding      string
	body          []byte
	contentLength int64
}

type kongZstdClosingBody struct {
	io.Reader
	closed bool
}

func (b *kongZstdClosingBody) Close() error {
	b.closed = true
	return nil
}

func kongZstdResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: &kongZstdClosingBody{Reader: strings.NewReader(body)}}
}

// kongZstdStubUpstream 记录每次发送的编码与正文，按调用序号给出响应。
type kongZstdStubUpstream struct {
	HTTPUpstream
	respond func(call int) (*http.Response, error)
	sent    []kongZstdSent
}

func (u *kongZstdStubUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.sent = append(u.sent, kongZstdSent{encoding: req.Header.Get("Content-Encoding"), body: body, contentLength: req.ContentLength})
	return u.respond(len(u.sent))
}

func kongZstdTestService(respond func(call int) (*http.Response, error)) (*OpenAIGatewayService, *kongZstdStubUpstream) {
	upstream := &kongZstdStubUpstream{respond: respond}
	return &OpenAIGatewayService{httpUpstream: upstream}, upstream
}

func kongZstdTestAccount() *Account {
	return &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
}

func TestKongParseOpenAIRequestZstd(t *testing.T) {
	cases := []struct {
		value   string
		present bool
		want    bool
		wantErr bool
	}{
		{present: false, want: true},
		{value: "true", present: true, want: true},
		{value: "1", present: true, want: true},
		{value: " false ", present: true, want: false},
		{value: "0", present: true, want: false},
		{value: "", present: true, want: false, wantErr: true},
		{value: "yes", present: true, want: false, wantErr: true},
	}
	for _, tc := range cases {
		got, err := kongParseOpenAIRequestZstd(tc.value, tc.present)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("value=%q present=%v: got %v err=%v", tc.value, tc.present, got, err)
		}
	}
}

func TestKongZstdEligible(t *testing.T) {
	body := kongZstdTestBody(4096)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		url    string
		method string
		want   bool
	}{
		{name: "codex responses", want: true},
		{name: "chatgpt subdomain", url: "https://ab.chatgpt.com/backend-api/codex/responses", want: true},
		{name: "other chatgpt path", url: "https://chatgpt.com/backend-api/codex/responses/compact", want: true},
		{name: "empty content type", mutate: func(r *http.Request) { r.Header.Del("Content-Type") }, want: true},
		{name: "api.openai.com", url: "https://api.openai.com/v1/responses"},
		{name: "lookalike host", url: "https://notchatgpt.com/backend-api/codex/responses"},
		{name: "suffix host", url: "https://chatgpt.com.example.io/backend-api/codex/responses"},
		{name: "GET", method: http.MethodGet},
		{name: "no GetBody", mutate: func(r *http.Request) { r.GetBody = nil }},
		{name: "already encoded", mutate: func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
		{name: "multipart", mutate: func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=x") }},
	}
	for _, tc := range cases {
		url, method := tc.url, tc.method
		if url == "" {
			url = kongZstdTestURL
		}
		if method == "" {
			method = http.MethodPost
		}
		req := newKongZstdTestRequest(t, method, url, body)
		if tc.mutate != nil {
			tc.mutate(req)
		}
		if got := kongZstdEligible(req); got != tc.want {
			t.Errorf("%s: eligible=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestKongZstdCompressSkips(t *testing.T) {
	z, _ := useKongZstd(t, true)
	cases := []struct {
		name   string
		body   []byte
		plugin bool
	}{
		{name: "small body", body: []byte(`{"model":"gpt-6-sol","input":"hi"}`)},
		{name: "not json", body: bytes.Repeat([]byte("x"), 4096)},
		{name: "plugin routed", body: kongZstdTestBody(4096), plugin: true},
	}
	for _, tc := range cases {
		req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, tc.body)
		if _, _, compressed := z.compress(req, tc.plugin); compressed || req.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: 不该压缩", tc.name)
		}
	}
	off, _ := useKongZstd(t, false)
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(4096))
	if _, _, compressed := off.compress(req, false); compressed {
		t.Error("关闭时不该压缩")
	}
}

func TestKongZstdCompressRoundTripAndFrameHeader(t *testing.T) {
	z, _ := useKongZstd(t, true)
	for _, size := range []int{2 << 10, 100 << 10, 3 << 20} {
		plain := kongZstdTestBody(size)
		req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, plain)
		gotPlain, endpoint, compressed := z.compress(req, false)
		if !compressed || !bytes.Equal(gotPlain, plain) || endpoint != "POST chatgpt.com/backend-api/codex/responses" {
			t.Fatalf("size=%d: compressed=%v endpoint=%q", size, compressed, endpoint)
		}
		if req.Header.Get("Content-Encoding") != "zstd" {
			t.Fatalf("size=%d: 缺 Content-Encoding", size)
		}
		encoded, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(encoded)) != req.ContentLength || len(encoded) >= len(plain) {
			t.Fatalf("size=%d: encoded=%d content_length=%d plain=%d", size, len(encoded), req.ContentLength, len(plain))
		}
		// 与 codex 的 libzstd 流式输出同一帧头：不带内容大小，窗口 2 MiB。
		if want := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58}; !bytes.Equal(encoded[:6], want) {
			t.Fatalf("size=%d: frame header % x, want % x", size, encoded[:6], want)
		}
		if !bytes.Equal(kongZstdDecode(t, encoded), plain) {
			t.Fatalf("size=%d: 解压结果与原文不一致", size)
		}
		replay, err := req.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		replayed, _ := io.ReadAll(replay)
		if !bytes.Equal(replayed, encoded) {
			t.Fatalf("size=%d: GetBody 应重放压缩后的正文", size)
		}
	}
}

func TestKongZstdSendAccepted(t *testing.T) {
	useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return kongZstdResponse(http.StatusOK, "ok"), nil })
	plain := kongZstdTestBody(8 << 10)
	resp, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, plain), "", kongZstdTestAccount())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(upstream.sent) != 1 || upstream.sent[0].encoding != "zstd" {
		t.Fatalf("sent=%+v", upstream.sent)
	}
	if !bytes.Equal(kongZstdDecode(t, upstream.sent[0].body), plain) {
		t.Fatal("上游收到的正文解压后应等于原文")
	}
}

func TestKongZstdDoOpenAIUpstreamCompresses(t *testing.T) {
	useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return kongZstdResponse(http.StatusOK, "ok"), nil })
	resp, err := svc.doOpenAIUpstream(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(upstream.sent) != 1 || upstream.sent[0].encoding != "zstd" {
		t.Fatalf("OpenAI 上游发送汇聚点应压缩请求体：%+v", upstream.sent)
	}
}

func TestKongZstdFallbackOn415MarksEndpointOff(t *testing.T) {
	z, clock := useKongZstd(t, true)
	rejectedBody := &kongZstdClosingBody{Reader: strings.NewReader("unsupported")}
	svc, upstream := kongZstdTestService(func(call int) (*http.Response, error) {
		if call == 1 {
			return &http.Response{StatusCode: http.StatusUnsupportedMediaType, Header: http.Header{}, Body: rejectedBody}, nil
		}
		return kongZstdResponse(http.StatusOK, "ok"), nil
	})
	account := kongZstdTestAccount()
	plain := kongZstdTestBody(8 << 10)
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, plain)
	resp, err := svc.kongSendOpenAIUpstreamCompressed(req, "", account)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("回退后应拿到明文请求的响应：resp=%v err=%v", resp, err)
	}
	if len(upstream.sent) != 2 || upstream.sent[0].encoding != "zstd" {
		t.Fatalf("sent=%+v", upstream.sent)
	}
	retry := upstream.sent[1]
	if retry.encoding != "" || !bytes.Equal(retry.body, plain) || retry.contentLength != int64(len(plain)) {
		t.Fatalf("重发应为明文：encoding=%q len=%d content_length=%d", retry.encoding, len(retry.body), retry.contentLength)
	}
	if !rejectedBody.closed {
		t.Fatal("被拒的响应要关闭")
	}

	send := func(url string) kongZstdSent {
		t.Helper()
		before := len(upstream.sent)
		if _, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, url, plain), "", account); err != nil {
			t.Fatal(err)
		}
		if len(upstream.sent) != before+1 {
			t.Fatalf("应只发送一次，实际 %d 次", len(upstream.sent)-before)
		}
		return upstream.sent[len(upstream.sent)-1]
	}
	if got := send(kongZstdTestURL); got.encoding != "" {
		t.Fatal("冷却期内该端点应直接发明文")
	}
	if got := send(kongZstdTestURL + "/compact"); got.encoding != "zstd" {
		t.Fatal("其他端点不受影响")
	}
	clock.now = clock.now.Add(kongZstdEndpointOffTTL)
	if got := send(kongZstdTestURL); got.encoding != "zstd" {
		t.Fatal("冷却期过后应重新压缩")
	}
	if len(z.off) != 0 {
		t.Fatalf("过期的端点记录应清掉：%v", z.off)
	}
}

func TestKongZstdFallbackOnDecodeErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"We could not parse the JSON body of your request."}}`},
		{http.StatusUnprocessableEntity, `{"detail":[{"type":"json_invalid","loc":["body",0],"msg":"JSON decode error"}]}`},
		{http.StatusBadRequest, `{"error":"Expecting value: line 1 column 1 (char 0)"}`},
		{http.StatusBadRequest, `{"error":"invalid character '(' looking for beginning of value"}`},
		{http.StatusBadRequest, `{"error":"Unexpected token '(', \"(�/�\" is not valid JSON"}`},
		{http.StatusBadRequest, `There was an error parsing the body`},
	}
	for _, tc := range cases {
		z, _ := useKongZstd(t, true)
		svc, upstream := kongZstdTestService(func(call int) (*http.Response, error) {
			if call == 1 {
				return kongZstdResponse(tc.status, tc.body), nil
			}
			return kongZstdResponse(http.StatusOK, "ok"), nil
		})
		resp, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount())
		if err != nil || resp.StatusCode != http.StatusOK || len(upstream.sent) != 2 || upstream.sent[1].encoding != "" {
			t.Fatalf("%q: 解码失败应回退明文重发：resp=%v err=%v sent=%d", tc.body, resp, err, len(upstream.sent))
		}
		if len(z.off) != 1 || z.stats.resent != 1 || z.stats.markedOff != 1 {
			t.Fatalf("%q: 明文通过后该端点应改发明文：off=%v stats=%+v", tc.body, z.off, z.stats)
		}
	}
}

func TestKongZstdSameRejectionOnPlainKeepsEndpoint(t *testing.T) {
	z, _ := useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(call int) (*http.Response, error) {
		return kongZstdResponse(http.StatusUnsupportedMediaType, fmt.Sprintf("rejected #%d", call)), nil
	})
	resp, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount())
	if err != nil || resp.StatusCode != http.StatusUnsupportedMediaType || len(upstream.sent) != 2 {
		t.Fatalf("resp=%v err=%v sent=%d", resp, err, len(upstream.sent))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "rejected #2" {
		t.Fatalf("明文那次的错误正文要完整交给调用方：%q err=%v", got, err)
	}
	if len(z.off) != 0 || z.stats.resent != 1 || z.stats.markedOff != 0 {
		t.Fatalf("明文同样被拒说明与压缩无关，端点不该关掉压缩：off=%v stats=%+v", z.off, z.stats)
	}
}

func TestKongZstdOrdinaryErrorsKeepBody(t *testing.T) {
	padding := strings.Repeat(" ", 100<<10) + "END"
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"Unsupported parameter: foo","type":"invalid_request_error"}}` + padding},
		{http.StatusBadRequest, `{"error":{"message":"Invalid JSON schema for function 'f'","type":"invalid_request_error"}}`},
		{http.StatusBadRequest, `{"error":{"message":"Invalid JSON payload received in 'tools[0].function.parameters'"}}`},
		{http.StatusUnprocessableEntity, `{"detail":[{"type":"missing","loc":["body","model"],"msg":"Field required"}]}`},
	}
	for _, tc := range cases {
		z, _ := useKongZstd(t, true)
		svc, upstream := kongZstdTestService(func(int) (*http.Response, error) {
			return kongZstdResponse(tc.status, tc.body), nil
		})
		resp, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount())
		if err != nil || resp.StatusCode != tc.status {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
		if len(upstream.sent) != 1 {
			t.Fatalf("%.60q: 普通业务错误不能重发：sent=%d", tc.body, len(upstream.sent))
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil || string(got) != tc.body {
			t.Fatalf("%.60q: 错误正文要完整交给调用方：len=%d err=%v", tc.body, len(got), err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if len(z.off) != 0 {
			t.Fatalf("%.60q: 普通业务错误不该关掉端点压缩：%v", tc.body, z.off)
		}
	}
}

func TestKongZstdTransportErrorNotRetried(t *testing.T) {
	useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return nil, errors.New("connection reset") })
	if _, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount()); err == nil {
		t.Fatal("传输错误要原样返回")
	}
	if len(upstream.sent) != 1 {
		t.Fatalf("传输错误不重发：sent=%d", len(upstream.sent))
	}
}

// kongZstdEnablePluginRoute 让测试账号命中插件绑定；插件运行时缺席，命中插件的发送会返回错误。
func kongZstdEnablePluginRoute(manager *PluginManager) {
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "测试不可用"})
}

func TestKongZstdPluginRoutedStaysPlain(t *testing.T) {
	useKongZstd(t, true)
	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return kongZstdResponse(http.StatusOK, "ok"), nil })
	svc.pluginManager = &PluginManager{}
	kongZstdEnablePluginRoute(svc.pluginManager)
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10))
	if _, err := svc.kongSendOpenAIUpstreamCompressed(req, "", kongZstdTestAccount()); err == nil || !strings.Contains(err.Error(), "插件不可用") {
		t.Fatalf("命中插件的请求应交给插件：err=%v", err)
	}
	if len(upstream.sent) != 0 || req.Header.Get("Content-Encoding") != "" {
		t.Fatalf("交给插件的请求保持明文：sent=%d encoding=%q", len(upstream.sent), req.Header.Get("Content-Encoding"))
	}
}

func TestKongZstdPluginBindingSwitchKeepsSendPath(t *testing.T) {
	t.Run("switched while compressing", func(t *testing.T) {
		useKongZstd(t, true)
		svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return kongZstdResponse(http.StatusOK, "ok"), nil })
		svc.pluginManager = &PluginManager{}
		req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10))
		getBody := req.GetBody
		req.GetBody = func() (io.ReadCloser, error) {
			kongZstdEnablePluginRoute(svc.pluginManager)
			return getBody()
		}
		if _, err := svc.kongSendOpenAIUpstreamCompressed(req, "", kongZstdTestAccount()); err != nil {
			t.Fatalf("已压缩的请求不能再转交插件：%v", err)
		}
		if len(upstream.sent) != 1 || upstream.sent[0].encoding != "zstd" {
			t.Fatalf("sent=%+v", upstream.sent)
		}
	})
	t.Run("switched before plain resend", func(t *testing.T) {
		useKongZstd(t, true)
		manager := &PluginManager{}
		svc, upstream := kongZstdTestService(func(call int) (*http.Response, error) {
			if call == 1 {
				kongZstdEnablePluginRoute(manager)
				return kongZstdResponse(http.StatusUnsupportedMediaType, "unsupported"), nil
			}
			return kongZstdResponse(http.StatusOK, "ok"), nil
		})
		svc.pluginManager = manager
		if _, err := svc.kongSendOpenAIUpstreamCompressed(newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, kongZstdTestBody(8<<10)), "", kongZstdTestAccount()); err != nil {
			t.Fatalf("明文重发要走与首发相同的路径：%v", err)
		}
		if len(upstream.sent) != 2 || upstream.sent[1].encoding != "" {
			t.Fatalf("sent=%d", len(upstream.sent))
		}
	})
}

func TestKongZstdPeriodicStatsLog(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	z, clock := useKongZstd(t, true)
	compress := func(url string) bool {
		req := newKongZstdTestRequest(t, http.MethodPost, url, kongZstdTestBody(8<<10))
		_, _, ok := z.compress(req, false)
		return ok
	}
	if !compress(kongZstdTestURL) {
		t.Fatal("应压缩")
	}
	if strings.Contains(buf.String(), "周期计数") {
		t.Fatal("未到间隔不该输出汇总")
	}
	clock.now = clock.now.Add(kongZstdStatsInterval)
	if !compress(kongZstdTestURL) {
		t.Fatal("应压缩")
	}
	if !strings.Contains(buf.String(), "周期计数") || !strings.Contains(buf.String(), "compressed=2") {
		t.Fatalf("到间隔应输出累计汇总：%s", buf.String())
	}

	// 端点全在冷却期、一个请求都没压缩时，汇总照样按间隔输出。
	buf.Reset()
	z.markOff("POST chatgpt.com/backend-api/codex/responses", http.StatusUnsupportedMediaType)
	clock.now = clock.now.Add(kongZstdStatsInterval)
	if compress(kongZstdTestURL) {
		t.Fatal("冷却期内不该压缩")
	}
	if !strings.Contains(buf.String(), "周期计数") || !strings.Contains(buf.String(), "skipped_off=1") || !strings.Contains(buf.String(), "marked_off=1") {
		t.Fatalf("只有冷却请求时也应输出汇总：%s", buf.String())
	}
}
