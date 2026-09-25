//go:build unit

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type kongZstdOutcome struct {
	compressed    bool
	encoding      string
	contentLength int64
	body          []byte
	replay        []byte
	restored      []byte
	restoredLen   int64
	stats         kongZstdStats
	viaPayload    bool // 出站正文由持有者提供，即走了流式压缩；比较时不算在内
}

// kongZstdCompressOnce 在给定开关下压缩一次，记下出站请求的样子、GetBody 重放的内容与换回明文后的正文。
func kongZstdCompressOnce(t *testing.T, streaming bool, build func() *http.Request) kongZstdOutcome {
	t.Helper()
	var restoreSwitch func()
	if streaming {
		restoreSwitch = KongSetOpenAIBodyFastpathForTest(true)
	} else {
		restoreSwitch = KongSetOpenAIBodyFastpathItemsOffForTest("zstd_out")
	}
	defer restoreSwitch()
	z := newKongOpenAIRequestZstd(true, time.Now)
	req := build()
	sent, _, compressed := z.compress(req, false)
	out := kongZstdOutcome{compressed: compressed, encoding: req.Header.Get("Content-Encoding"), contentLength: req.ContentLength, stats: z.stats}
	_, out.viaPayload = req.Body.(*kongZstdPayloadReader)
	out.body, _ = io.ReadAll(req.Body)
	if compressed {
		replay, err := req.GetBody()
		require.NoError(t, err)
		out.replay, _ = io.ReadAll(replay)
		require.NoError(t, sent.restore(req))
		out.restored, _ = io.ReadAll(req.Body)
		out.restoredLen = req.ContentLength
		sent.release()
	}
	return out
}

func kongZstdRandomJSON(seed uint64, size int) []byte {
	rng := rand.New(rand.NewPCG(seed, 3))
	raw := make([]byte, size*3/4)
	for i := range raw {
		raw[i] = byte(rng.UintN(256))
	}
	return []byte(`{"model":"gpt-6-sol","image":"` + base64.StdEncoding.EncodeToString(raw) + `"}`)
}

func TestKongZstdStreamingMatchesOriginal(t *testing.T) {
	const block = 128 << 10
	bodies := map[string][]byte{}
	for _, size := range []int{1000, 1024, 2 << 10, block - 50, block, block + 1, 2*block - 40, 2 * block, 1<<20 + 12345, 3 << 20} {
		bodies[fmt.Sprintf("text_%d", size)] = kongZstdTestBody(size)
	}
	for _, size := range []int{4 << 10, block, 3 << 20} {
		bodies[fmt.Sprintf("random_%d", size)] = kongZstdRandomJSON(uint64(size), size)
	}
	bodies["exact_block"] = append([]byte(`{"x":"`), append(bytes.Repeat([]byte("a"), block-8), '"', '}')...)
	bodies["leading_space"] = append([]byte(" \n\t\r "), kongZstdTestBody(8<<10)...)
	bodies["leading_space_long"] = append(bytes.Repeat([]byte(" "), 3000), kongZstdTestBody(8<<10)...)
	bodies["array"] = []byte("[" + strings.Repeat(`{"a":1},`, 500) + `{"a":2}]`)
	bodies["not_json"] = bytes.Repeat([]byte("x"), 8<<10)
	bodies["all_space"] = bytes.Repeat([]byte(" "), 4<<10)
	bodies["small"] = []byte(`{"model":"m"}`)
	for name, body := range bodies {
		build := func() *http.Request { return newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, body) }
		want := kongZstdCompressOnce(t, false, build)
		got := kongZstdCompressOnce(t, true, build)
		require.False(t, want.viaPayload, name)
		require.Equal(t, got.compressed, got.viaPayload, "%s：压缩了却没走流式路径", name)
		got.viaPayload = false
		require.Equal(t, want, got, name)
		if want.compressed {
			require.Equal(t, body, got.restored, "%s：换回的明文不对", name)
		}
	}
}

// TestKongZstdStreamingContentLengthMismatch：ContentLength 与 GetBody 读出的长度不符时交给原读法，结果与原读法相同。
func TestKongZstdStreamingContentLengthMismatch(t *testing.T) {
	body := kongZstdTestBody(64 << 10)
	for _, delta := range []int64{-1, -4000, 1, 5000} {
		build := func() *http.Request {
			req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, body)
			req.ContentLength = int64(len(body)) + delta
			return req
		}
		got := kongZstdCompressOnce(t, true, build)
		require.False(t, got.viaPayload, "delta=%d：长度不符时应交给原读法", delta)
		require.Equal(t, kongZstdCompressOnce(t, false, build), got, "delta=%d", delta)
	}
}

func TestKongZstdOutputBoundNoGrowth(t *testing.T) {
	for _, size := range []int{1 << 10, 128 << 10, 128<<10 + 1, 1 << 20, 5<<20 + 7} {
		plain := kongZstdRandomJSON(uint64(size)+1, size)
		encoded, err := kongZstdEncodeStream(bytes.NewReader(plain), int64(len(plain)))
		require.NoError(t, err)
		require.Equal(t, kongZstdOutputBound(int64(len(plain))), cap(encoded), "size=%d：输出缓冲扩容了", size)
		require.Equal(t, plain, kongZstdDecode(t, encoded))
	}
	// 不可压缩的原始字节，长度落在块边界附近：块全部按原样存放，再加每块 3 字节块头、帧头与结尾 3 字节的空块。
	rng := rand.New(rand.NewPCG(5, 5))
	for _, size := range []int{1, 1023, 128<<10 - 1, 128 << 10, 128<<10 + 1, 262132, 256 << 10, 256<<10 + 1, 1<<20 - 3, 1 << 20} {
		plain := make([]byte, size)
		for i := range plain {
			plain[i] = byte(rng.UintN(256))
		}
		encoded, err := kongZstdEncodeStream(bytes.NewReader(plain), int64(size))
		require.NoError(t, err)
		require.Equal(t, kongZstdOutputBound(int64(size)), cap(encoded), "size=%d：输出缓冲扩容了（输出 %d 字节）", size, len(encoded))
		require.Equal(t, plain, kongZstdDecode(t, encoded))
	}
}

func kongZstdReaderHolds(r io.ReadCloser) bool {
	pr := r.(*kongZstdPayloadReader)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.data != nil
}

func TestKongZstdPayloadReleaseSemantics(t *testing.T) {
	data := []byte(strings.Repeat("payload", 1000))
	payload := &kongZstdPayload{data: data}
	first := payload.reader()
	second, err := payload.getBody()
	require.NoError(t, err)
	head := make([]byte, 100)
	_, err = io.ReadFull(first, head)
	require.NoError(t, err)

	payload.release()
	_, err = payload.getBody()
	require.ErrorIs(t, err, errKongZstdPayloadReleased)

	rest, err := io.ReadAll(first)
	require.NoError(t, err)
	require.Equal(t, data, append(head, rest...), "释放之后已有的 reader 应照常读完")
	require.False(t, kongZstdReaderHolds(first), "读到 EOF 后应放掉正文")
	require.True(t, kongZstdReaderHolds(second))
	require.NoError(t, second.Close())
	require.False(t, kongZstdReaderHolds(second), "Close 后应放掉正文")
	n, err := second.Read(head)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.EOF)
}

// kongZstdEventually 等 cond 成立，最多等 5 秒。
func kongZstdEventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func kongZstdPayloadRequest(t *testing.T, ctx context.Context, url string, payload *kongZstdPayload) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	require.NoError(t, err)
	req.Body = payload.reader()
	req.GetBody = payload.getBody
	req.ContentLength = int64(len(payload.data))
	return req
}

// kongZstdGatedBody 把正文读到只剩最后 tail 字节时停住，等 gate 关闭再读尾部：保证释放持有者的那一刻上传还没完成。
type kongZstdGatedBody struct {
	inner   *kongZstdPayloadReader
	size    int
	tail    int
	read    int
	opened  bool
	gate    chan struct{}
	reached chan struct{}
	once    sync.Once
}

func newKongZstdGatedBody(payload *kongZstdPayload, tail int) *kongZstdGatedBody {
	inner, _ := payload.reader().(*kongZstdPayloadReader)
	return &kongZstdGatedBody{inner: inner, size: len(payload.data), tail: tail, gate: make(chan struct{}), reached: make(chan struct{})}
}

func (g *kongZstdGatedBody) Read(p []byte) (int, error) {
	if !g.opened {
		limit := g.size - g.tail - g.read
		if limit <= 0 {
			g.once.Do(func() { close(g.reached) })
			select {
			case <-g.gate:
				g.opened = true
			case <-time.After(10 * time.Second):
				return 0, errors.New("gate timeout")
			}
		} else if len(p) > limit {
			p = p[:limit]
		}
	}
	n, err := g.inner.Read(p)
	g.read += n
	return n, err
}

func (g *kongZstdGatedBody) Close() error { return g.inner.Close() }

func kongZstdWait(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// kongZstdEarlyResponse 验证"先回响应、正文还没发完"时释放持有者：transport 停在门闩上（上传确实没完成）时释放，
// 放行之后尾部照常送达，送完 reader 放掉正文。
func kongZstdEarlyResponse(t *testing.T, client *http.Client, url string, data []byte, received <-chan []byte) {
	t.Helper()
	payload := &kongZstdPayload{data: data}
	gated := newKongZstdGatedBody(payload, 64<<10)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	require.NoError(t, err)
	req.Body, req.GetBody, req.ContentLength = gated, payload.getBody, int64(len(data))
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	kongZstdWait(t, gated.reached, "transport 没有读到门闩处")
	require.True(t, kongZstdReaderHolds(gated.inner), "释放之前正文应仍未发完")
	payload.release()
	close(gated.gate)
	select {
	case body := <-received:
		require.Equal(t, data, body, "释放之后尾部没有完整送达")
	case <-time.After(5 * time.Second):
		t.Fatal("服务端没有收到完整正文")
	}
	kongZstdEventually(t, func() bool { return !kongZstdReaderHolds(gated.inner) }, "正文发完后 reader 仍持有正文")
}

func TestKongZstdPayloadHTTP1EarlyResponse(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		length := 0
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				length, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		body := make([]byte, length)
		_, _ = io.ReadFull(br, body)
		received <- body
	}()
	kongZstdEarlyResponse(t, &http.Client{Transport: &http.Transport{}}, "http://"+listener.Addr().String()+"/", data, received)
}

func TestKongZstdPayloadHTTP2EarlyResponse(t *testing.T) {
	data := bytes.Repeat([]byte("h2-body-"), 1<<17) // 1 MiB
	received := make(chan []byte, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		body, _ := io.ReadAll(r.Body)
		received <- body
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	kongZstdEarlyResponse(t, server.Client(), server.URL, data, received)
}

// kongZstdClientUpstream 把 HTTPUpstream.Do 交给真实的 http.Client，用来经生产函数发送。
type kongZstdClientUpstream struct {
	HTTPUpstream
	client *http.Client
}

func (u kongZstdClientUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

// TestKongZstdSendRedirectReplay：307 / 308 在 Do 内经 GetBody 重放压缩正文，由 kongSendOpenAIUpstreamCompressed 在
// 返回时释放；若释放提前到发送之前，重放就会失败。
func TestKongZstdSendRedirectReplay(t *testing.T) {
	useKongZstd(t, true)
	defer KongSetOpenAIBodyFastpathForTest(true)()
	plain := kongZstdTestBody(64 << 10)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var mu sync.Mutex
		var got []byte
		var encoding string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/next" {
				http.Redirect(w, r, "/next", status)
				return
			}
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			got, encoding = body, r.Header.Get("Content-Encoding")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		address := server.Listener.Addr().String()
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}}
		svc := &OpenAIGatewayService{httpUpstream: kongZstdClientUpstream{client: client}}
		req := newKongZstdTestRequest(t, http.MethodPost, "http://chatgpt.com/backend-api/codex/responses", plain)
		resp, err := svc.kongSendOpenAIUpstreamCompressed(req, "", kongZstdTestAccount())
		require.NoError(t, err, "status=%d", status)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		mu.Lock()
		require.Equal(t, "zstd", encoding, "status=%d", status)
		require.Equal(t, plain, kongZstdDecode(t, got), "status=%d：重放的正文不对", status)
		mu.Unlock()
		_, err = req.GetBody()
		require.ErrorIs(t, err, errKongZstdPayloadReleased, "status=%d：返回之后应已释放", status)
		server.Close()
	}
}

// kongZstdCanceledUpload：上传途中取消（服务端收到请求后既不读正文也不回响应），Do 应及时以 context.Canceled 返回，
// transport 关闭正文，reader 放掉引用。
func kongZstdCanceledUpload(t *testing.T, client *http.Client, url string, started <-chan struct{}) {
	t.Helper()
	data := bytes.Repeat([]byte("cancel"), 8<<20/6) // 约 8 MiB，远超流控窗口与套接字缓冲
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := &kongZstdPayload{data: data}
	req := kongZstdPayloadRequest(t, ctx, url, payload)
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	kongZstdWait(t, started, "服务端没有收到请求")
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Do 没有及时返回")
	}
	payload.release()
	kongZstdEventually(t, func() bool { return !kongZstdReaderHolds(req.Body) }, "取消后 reader 仍持有正文")
}

func TestKongZstdPayloadCanceledUploadHTTP1(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
	})
	started := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		close(started)
		<-stop
	}()
	kongZstdCanceledUpload(t, &http.Client{Transport: &http.Transport{}}, "http://"+listener.Addr().String()+"/", started)
}

func TestKongZstdPayloadCanceledUploadHTTP2(t *testing.T) {
	stop := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() {
		close(stop)
		server.Close()
	})
	kongZstdCanceledUpload(t, server.Client(), server.URL, started)
}

func TestKongZstdSendReleasesPayload(t *testing.T) {
	useKongZstd(t, true)
	defer KongSetOpenAIBodyFastpathForTest(true)()
	plain := kongZstdTestBody(16 << 10)

	svc, upstream := kongZstdTestService(func(int) (*http.Response, error) { return kongZstdResponse(http.StatusOK, "ok"), nil })
	req := newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, plain)
	resp, err := svc.kongSendOpenAIUpstreamCompressed(req, "", kongZstdTestAccount())
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, "zstd", upstream.sent[0].encoding)
	_, err = req.GetBody()
	require.True(t, errors.Is(err, errKongZstdPayloadReleased), "返回之后 GetBody 应报已释放，得到 %v", err)

	svc, upstream = kongZstdTestService(func(call int) (*http.Response, error) {
		if call == 1 {
			return kongZstdResponse(http.StatusUnsupportedMediaType, "unsupported"), nil
		}
		return kongZstdResponse(http.StatusOK, "ok"), nil
	})
	req = newKongZstdTestRequest(t, http.MethodPost, kongZstdTestURL, plain)
	resp, err = svc.kongSendOpenAIUpstreamCompressed(req, "", kongZstdTestAccount())
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Len(t, upstream.sent, 2)
	require.Equal(t, "", upstream.sent[1].encoding)
	require.Equal(t, plain, upstream.sent[1].body)
	require.Equal(t, int64(len(plain)), upstream.sent[1].contentLength)
	replay, err := req.GetBody()
	require.NoError(t, err, "换回明文后 GetBody 应给出明文")
	replayed, _ := io.ReadAll(replay)
	require.Equal(t, plain, replayed)
}
