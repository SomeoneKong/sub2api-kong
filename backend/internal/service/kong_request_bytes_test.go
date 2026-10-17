//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// kongBytesStubUpstream 像 transport 那样读请求体（readBody 为假时模拟建连失败、正文没被读），并像
// repository 的 doUpstreamRequest 那样在返回前挂上响应计数。
type kongBytesStubUpstream struct {
	HTTPUpstream
	respond func(call int, req *http.Request) (*http.Response, error)
	calls   int
	bodies  [][]byte
}

func (u *kongBytesStubUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	resp, err := u.respond(u.calls, req)
	if resp != nil {
		KongCountUpstreamResponseBody(req.Context(), resp)
	}
	return resp, err
}

func (u *kongBytesStubUpstream) readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	u.bodies = append(u.bodies, body)
	return body
}

func kongBytesTestContext() (context.Context, *KongRequestBytes) {
	rb := NewKongRequestBytes()
	return WithKongRequestBytes(context.Background(), rb), rb
}

func kongBytesTestAccount(id int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
}

func kongBytesRequest(t *testing.T, ctx context.Context, body []byte) *http.Request {
	t.Helper()
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, body)
	return req.WithContext(ctx)
}

func kongBytesMerge(ctx context.Context, account *Account, written int) *KongRequestBytesFeature {
	res := &OpenAIForwardResult{}
	KongMergeRequestBytes(ctx, res, account, written)
	if res.KongRequestFeatures == nil {
		return nil
	}
	return res.KongRequestFeatures.Bytes
}

func kongBytesReadAllAndClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return string(body)
}

func TestKongRequestBytesJSONShape(t *testing.T) {
	f := &KongRequestFeatures{Bytes: &KongRequestBytesFeature{}}
	require.False(t, f.IsEmpty(), "只有字节数的特征不算空")
	encoded, err := json.Marshal(f)
	require.NoError(t, err)
	require.JSONEq(t, `{"bytes":{"in":0,"body":0,"out":0,"port":0,"up":0,"up_wire":0,"sends":0,"down":0}}`, string(encoded))

	f.Bytes.Others = []KongRequestBytesOther{{Account: 3, Port: 23618, Sends: 1}}
	f.Bytes.Plugin = 2
	encoded, err = json.Marshal(f)
	require.NoError(t, err)
	var back KongRequestFeatures
	require.NoError(t, json.Unmarshal(encoded, &back))
	require.Equal(t, f, &back)
}

func TestKongRequestBytesNoContainerIsNoop(t *testing.T) {
	upstream := &kongBytesStubUpstream{}
	var sentBody io.ReadCloser
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		sentBody = req.Body
		return kongZstdResponse(http.StatusOK, "ok"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	req := kongBytesRequest(t, context.Background(), []byte(`{"a":1}`))
	original := req.Body
	resp, err := svc.kongDoUpstreamCounted(req, "http://127.0.0.1:23625", kongBytesTestAccount(1), req.ContentLength)
	require.NoError(t, err)
	require.True(t, original == sentBody, "没有容器时请求原样交出")
	kongBytesReadAllAndClose(t, resp)

	res := &OpenAIForwardResult{}
	KongMergeRequestBytes(context.Background(), res, kongBytesTestAccount(1), 10)
	require.Nil(t, res.KongRequestFeatures)
}

func TestKongRequestBytesNonOpenAIPlatformNotCounted(t *testing.T) {
	ctx, rb := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusOK, "ok"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	grok := &Account{ID: 5, Platform: PlatformGrok, Concurrency: 1}
	req := kongBytesRequest(t, ctx, []byte(`{"a":1}`))
	resp, err := svc.kongDoUpstreamCounted(req, "", grok, req.ContentLength)
	require.NoError(t, err)
	kongBytesReadAllAndClose(t, resp)
	require.Empty(t, rb.groups)

	res := &OpenAIForwardResult{}
	KongMergeRequestBytes(ctx, res, grok, 10)
	require.Nil(t, res.KongRequestFeatures, "非 OpenAI 平台的行不写 bytes")
}

func TestKongRequestBytesCompressedSend(t *testing.T) {
	useKongZstd(t, true)
	ctx, _ := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusOK, "data: done\n\n"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	plain := kongZstdTestBody(8 << 10)
	resp, err := svc.kongSendOpenAIUpstreamCompressed(kongBytesRequest(t, ctx, plain), "http://127.0.0.1:23625", kongBytesTestAccount(7))
	require.NoError(t, err)
	require.Equal(t, "data: done\n\n", kongBytesReadAllAndClose(t, resp))

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.NotNil(t, b)
	require.Equal(t, 23625, b.Port)
	require.Equal(t, int64(len(plain)), b.Up)
	require.Equal(t, int64(len(upstream.bodies[0])), b.UpWire)
	require.Less(t, b.UpWire, b.Up, "压缩后的线上字节应小于明文")
	require.Equal(t, int64(1), b.Sends)
	require.Equal(t, int64(len("data: done\n\n")), b.Down)
	require.Empty(t, b.Others)
}

func TestKongRequestBytesRejectedCompressionResend(t *testing.T) {
	useKongZstd(t, true)
	ctx, _ := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(call int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		if call == 1 {
			// 415 不读正文
			return kongZstdResponse(http.StatusUnsupportedMediaType, "unsupported"), nil
		}
		return kongZstdResponse(http.StatusOK, "ok"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	plain := kongZstdTestBody(8 << 10)
	resp, err := svc.kongSendOpenAIUpstreamCompressed(kongBytesRequest(t, ctx, plain), "", kongBytesTestAccount(7))
	require.NoError(t, err)
	kongBytesReadAllAndClose(t, resp)

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, int64(2*len(plain)), b.Up)
	require.Equal(t, int64(len(upstream.bodies[0])+len(plain)), b.UpWire)
	require.Equal(t, int64(2), b.Sends)
	require.Equal(t, int64(len("ok")), b.Down, "415 的正文没读，不计")
}

func TestKongRequestBytesProbedErrorCountedOnce(t *testing.T) {
	useKongZstd(t, true)
	ctx, _ := kongBytesTestContext()
	errBody := `{"error":{"message":"model not found","type":"invalid_request_error"}}`
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusBadRequest, errBody), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	resp, err := svc.kongSendOpenAIUpstreamCompressed(kongBytesRequest(t, ctx, kongZstdTestBody(8<<10)), "", kongBytesTestAccount(7))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, errBody, kongBytesReadAllAndClose(t, resp), "普通业务错误的正文原样交还")

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.Equal(t, int64(1), b.Sends)
	require.Equal(t, int64(len(errBody)), b.Down, "探读的前缀与回填只计一次")
}

func TestKongRequestBytesFailedAttemptThenSwitch(t *testing.T) {
	ctx, rb := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(call int, req *http.Request) (*http.Response, error) {
		if call == 1 {
			return nil, errors.New("dial tcp: connection refused") // 建连失败，正文没被读
		}
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusOK, "hello"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	body := []byte(`{"model":"gpt-6.1-sol","input":"hi"}`)
	accountA, accountB := kongBytesTestAccount(3), kongBytesTestAccount(4)

	_, err := svc.sendOpenAIUpstream(kongBytesRequest(t, ctx, body), "http://127.0.0.1:23618", accountA)
	require.Error(t, err)
	resp, err := svc.sendOpenAIUpstream(kongBytesRequest(t, ctx, body), "socks5://127.0.0.1:23625", accountB)
	require.NoError(t, err)
	kongBytesReadAllAndClose(t, resp)

	b := kongBytesMerge(ctx, accountB, 0)
	require.Equal(t, 23625, b.Port)
	require.Equal(t, int64(len(body)), b.Up)
	require.Equal(t, int64(len(body)), b.UpWire)
	require.Equal(t, int64(1), b.Sends)
	require.Equal(t, int64(len("hello")), b.Down)
	require.Equal(t, []KongRequestBytesOther{{Account: 3, Port: 23618, Sends: 1}}, b.Others, "失败那次只计发送次数")

	// 合并之后再有发送（例如账号换了出口）不影响已合并的结果。
	resp, err = svc.sendOpenAIUpstream(kongBytesRequest(t, ctx, body), "http://127.0.0.1:23699", accountB)
	require.NoError(t, err)
	kongBytesReadAllAndClose(t, resp)
	require.Equal(t, 23625, b.Port)
	require.Len(t, b.Others, 1)
	require.Len(t, rb.groups, 3)
}

func TestKongRequestBytesTopGroupIsRowAccountsLatest(t *testing.T) {
	ctx, _ := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusOK, "ok"), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := kongBytesTestAccount(4)
	for _, proxy := range []string{"http://127.0.0.1:23618", "http://127.0.0.1:23625"} {
		resp, err := svc.sendOpenAIUpstream(kongBytesRequest(t, ctx, []byte(`{}`)), proxy, account)
		require.NoError(t, err)
		kongBytesReadAllAndClose(t, resp)
	}
	b := kongBytesMerge(ctx, account, 0)
	require.Equal(t, 23625, b.Port, "同一账号经两个出口时，最近一次发送的出口放顶层")
	require.Equal(t, []KongRequestBytesOther{{Account: 4, Port: 23618, Up: 2, UpWire: 2, Sends: 1, Down: 2}}, b.Others)
}

func TestKongRequestBytesRedirectReplayCountsWire(t *testing.T) {
	final := "final-body"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/end")
			w.WriteHeader(http.StatusTemporaryRedirect)
			_, _ = io.WriteString(w, "redirect body is not counted")
			return
		}
		_, _ = io.WriteString(w, final)
	}))
	t.Cleanup(srv.Close)

	ctx, _ := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		return srv.Client().Do(req)
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	body := []byte(`{"model":"gpt-6.1-sol","input":"redirect me"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/start", bytes.NewReader(body))
	require.NoError(t, err)
	resp, err := svc.sendOpenAIUpstream(req, "", kongBytesTestAccount(7))
	require.NoError(t, err)
	require.Equal(t, final, kongBytesReadAllAndClose(t, resp))

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.Equal(t, int64(1), b.Sends, "重定向跳转不另算发送")
	require.Equal(t, int64(len(body)), b.Up)
	require.Equal(t, int64(2*len(body)), b.UpWire, "重定向经 GetBody 重放的正文计入 up_wire")
	require.Equal(t, int64(len(final)), b.Down, "重定向的中间响应不计")
}

func TestKongRequestBytesUnknownLengthCountsReadBytes(t *testing.T) {
	ctx, _ := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{}
	upstream.respond = func(_ int, req *http.Request) (*http.Response, error) {
		upstream.readBody(t, req)
		return kongZstdResponse(http.StatusOK, ""), nil
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kongZstdTestURL, io.NopCloser(strings.NewReader("streamed-body")))
	require.NoError(t, err)
	require.Equal(t, int64(0), req.ContentLength)
	req.ContentLength = -1
	resp, err := svc.kongDoUpstreamCounted(req, "", kongBytesTestAccount(7), req.ContentLength)
	require.NoError(t, err)
	kongBytesReadAllAndClose(t, resp)

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.Equal(t, int64(len("streamed-body")), b.Up)
	require.Equal(t, int64(len("streamed-body")), b.UpWire)
}

func TestKongRequestBytesPluginSendOnlyCounted(t *testing.T) {
	ctx, rb := kongBytesTestContext()
	upstream := &kongBytesStubUpstream{respond: func(int, *http.Request) (*http.Response, error) {
		t.Fatal("交给插件的发送不应走 HTTPUpstream")
		return nil, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, pluginManager: &PluginManager{}}
	kongZstdEnablePluginRoute(svc.pluginManager)
	_, err := svc.kongSendOpenAIUpstreamCompressed(kongBytesRequest(t, ctx, kongZstdTestBody(8<<10)), "", kongBytesTestAccount(7))
	require.ErrorContains(t, err, "插件不可用")
	require.Empty(t, rb.groups)

	b := kongBytesMerge(ctx, kongBytesTestAccount(7), 0)
	require.Equal(t, int64(1), b.Plugin)
	require.Zero(t, b.Sends)
	require.Zero(t, b.Port)
}

func TestKongRequestBytesClientSideAndOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, rb := kongBytesTestContext()
	body := rb.CountInbound(io.NopCloser(strings.NewReader("compressed-bytes")))
	_, err := io.ReadAll(body)
	require.NoError(t, err)
	KongRecordRequestBodyLen(ctx, 42)

	require.Equal(t, -1, c.Writer.Size())
	require.Zero(t, kongBytesMerge(ctx, kongBytesTestAccount(7), c.Writer.Size()).Out, "没写过时按 0 记")

	_, _ = c.Writer.WriteString(": keepalive\n\n")
	_, _ = c.Writer.WriteString("data: {}\n\n")
	b := kongBytesMerge(ctx, kongBytesTestAccount(7), c.Writer.Size())
	require.Equal(t, int64(len("compressed-bytes")), b.In)
	require.Equal(t, int64(42), b.Body)
	require.Equal(t, int64(len(": keepalive\n\ndata: {}\n\n")), b.Out, "心跳计入 out")
}

func TestKongMergeKeepsTicketFeatures(t *testing.T) {
	ctx, _ := kongBytesTestContext()
	stateLen := 12
	res := &OpenAIForwardResult{KongRequestFeatures: &KongRequestFeatures{StateFP: "abcdef012345", StateLen: &stateLen}}
	KongMergeRequestBytes(ctx, res, kongBytesTestAccount(7), 5)
	require.Equal(t, "abcdef012345", res.KongRequestFeatures.StateFP)
	require.NotNil(t, res.KongRequestFeatures.Bytes)
	require.Equal(t, int64(5), res.KongRequestFeatures.Bytes.Out)
}

func TestKongProxyPort(t *testing.T) {
	for in, want := range map[string]int{
		"":                               0,
		"http://127.0.0.1:23625":         23625,
		"socks5://user:pass@host:23618":  23618,
		"http://proxy.example":           0,
		"://bad":                         0,
		"socks5h://[::1]:1080/somewhere": 1080,
	} {
		require.Equal(t, want, kongProxyPort(in), in)
	}
}

func TestKongCountingReaderConcurrentGroups(t *testing.T) {
	ctx, rb := kongBytesTestContext()
	account := kongBytesTestAccount(9)
	done := make(chan struct{})
	var total atomic.Int64
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			g := kongRequestBytesGroupFor(ctx, account, "http://127.0.0.1:23625")
			r := &kongCountingReadCloser{ReadCloser: io.NopCloser(strings.NewReader("12345")), n: &g.down}
			n, _ := io.Copy(io.Discard, r)
			total.Add(n)
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	require.Len(t, rb.groups, 1)
	require.Equal(t, total.Load(), rb.groups[0].down.Load())
}
