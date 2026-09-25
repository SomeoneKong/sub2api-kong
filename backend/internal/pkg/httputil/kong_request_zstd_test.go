//go:build unit

package httputil

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type kongReadOutcome struct {
	body            []byte
	err             string
	maxBytes        bool
	contentEncoding string
	contentLength   string
	length          int64
}

type kongReadCase struct {
	name  string
	wire  []byte
	cl    int64 // -1 表示没有 Content-Length
	wrap  func(io.Reader) io.Reader
	limit int64 // >0 时套一层 http.MaxBytesReader
	// wantErr 非空时两边都必须返回含这段文字的错误、正文为空
	wantErr string
}

func kongReadWith(t *testing.T, fast bool, tc kongReadCase) kongReadOutcome {
	t.Helper()
	previous := KongRequestZstdFastpathEnabled
	KongRequestZstdFastpathEnabled = func() bool { return fast }
	defer func() { KongRequestZstdFastpathEnabled = previous }()

	var reader io.Reader = bytes.NewReader(tc.wire)
	if tc.wrap != nil {
		reader = tc.wrap(reader)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", reader)
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Length", fmt.Sprint(len(tc.wire)))
	req.ContentLength = tc.cl
	if tc.limit > 0 {
		req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, tc.limit)
	}
	body, err := ReadRequestBodyWithPrealloc(req)
	out := kongReadOutcome{body: body, contentEncoding: req.Header.Get("Content-Encoding"), contentLength: req.Header.Get("Content-Length"), length: req.ContentLength}
	if err != nil {
		out.err = err.Error()
		var maxErr *http.MaxBytesError
		out.maxBytes = errors.As(err, &maxErr)
	}
	return out
}

func kongRequireReadSame(t *testing.T, tc kongReadCase) {
	t.Helper()
	want := kongReadWith(t, false, tc)
	got := kongReadWith(t, true, tc)
	if tc.wantErr != "" {
		require.Contains(t, want.err, tc.wantErr, tc.name)
		require.Empty(t, want.body, tc.name)
	}
	require.Equal(t, want.err, got.err, tc.name)
	require.Equal(t, want.maxBytes, got.maxBytes, tc.name)
	require.Equal(t, want.contentEncoding, got.contentEncoding, tc.name)
	require.Equal(t, want.contentLength, got.contentLength, tc.name)
	require.Equal(t, want.length, got.length, tc.name)
	require.True(t, bytes.Equal(want.body, got.body), "%s：正文不同（%d / %d 字节）", tc.name, len(want.body), len(got.body))
}

func kongEncodeAll(t *testing.T, plain []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, append([]zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3))}, opts...)...)
	require.NoError(t, err)
	defer encoder.Close()
	return encoder.EncodeAll(plain, nil)
}

func kongEncodeStream(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	require.NoError(t, err)
	_, err = encoder.Write(plain)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	return buf.Bytes()
}

func kongJSONBody(seed uint64, size int, random bool) []byte {
	rng := rand.New(rand.NewPCG(seed, 11))
	var b strings.Builder
	b.WriteString(`{"model":"gpt-6-sol","input":[`)
	for b.Len() < size {
		if random {
			raw := make([]byte, 3000)
			for i := range raw {
				raw[i] = byte(rng.UintN(256))
			}
			b.WriteString(`{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(raw) + `"},`)
		} else {
			fmt.Fprintf(&b, `{"type":"message","role":"user","content":[{"type":"input_text","text":"line %d %s"}]},`, rng.IntN(1000), strings.Repeat("word ", rng.IntN(40)))
		}
	}
	b.WriteString(`{"type":"message"}]}`)
	return []byte(b.String())
}

type kongErrAfter struct {
	r io.Reader
	n int
}

func (e *kongErrAfter) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	if len(p) > e.n {
		p = p[:e.n]
	}
	n, err := e.r.Read(p)
	e.n -= n
	return n, err
}

func kongFlip(b []byte, at int) []byte {
	out := append([]byte(nil), b...)
	out[at] ^= 0xff
	return out
}

func TestKongReadZstdRequestBodyMatchesOriginal(t *testing.T) {
	small := kongJSONBody(1, 300, false)
	mid := kongJSONBody(2, 600<<10, false)
	big := kongJSONBody(3, 3<<20, false)
	images := kongJSONBody(4, 3<<20, true)
	frame := kongEncodeAll(t, mid)
	withCRC := kongEncodeAll(t, mid, zstd.WithEncoderCRC(true))
	skippable := append([]byte{0x50, 0x2a, 0x4d, 0x18, 4, 0, 0, 0, 'k', 'o', 'n', 'g'}, frame...)
	bigFrame := kongEncodeAll(t, big)

	cases := []kongReadCase{
		{name: "frame_small", wire: kongEncodeAll(t, small)},
		{name: "frame_mid", wire: frame},
		{name: "frame_big", wire: kongEncodeAll(t, big)},
		{name: "frame_images", wire: kongEncodeAll(t, images)},
		{name: "stream_no_fcs", wire: kongEncodeStream(t, big)},
		{name: "multi_frame", wire: append(kongEncodeAll(t, mid[:len(mid)/2]), kongEncodeAll(t, mid[len(mid)/2:])...)},
		{name: "skippable_first", wire: skippable},
		{name: "truncated", wire: frame[:len(frame)*2/3]},
		{name: "corrupt_middle", wire: kongFlip(frame, len(frame)/2)},
		{name: "corrupt_crc", wire: kongFlip(withCRC, len(withCRC)-1)},
		{name: "garbage_after", wire: append(append([]byte(nil), frame...), []byte("not a zstd frame")...)},
		{name: "empty", wire: []byte{}},
		{name: "short", wire: []byte{0x28, 0xb5, 0x2f}},
		{name: "not_zstd", wire: []byte(`{"model":"plain"}`)},
		{name: "chunked", wire: kongEncodeAll(t, big), cl: -1},
		{name: "read_error", wire: bigFrame, wrap: func(r io.Reader) io.Reader { return &kongErrAfter{r: r, n: len(bigFrame) / 2} }, wantErr: "connection reset by peer"},
		{name: "max_bytes", wire: kongEncodeAll(t, images), limit: 1 << 20},
	}
	for _, procs := range []int{1, 2} {
		previous := runtime.GOMAXPROCS(procs)
		for _, tc := range cases {
			if tc.cl == 0 {
				tc.cl = int64(len(tc.wire))
			}
			tc.name = fmt.Sprintf("%s/procs=%d", tc.name, procs)
			kongRequireReadSame(t, tc)
		}
		runtime.GOMAXPROCS(previous)
	}
}

// TestKongReadZstdRequestBodyTruncationLine 覆盖 64 MiB 截断线：超长时原代码报 MaxBytesError，最后一块的校验和跨过
// 截断线时同样由原代码决定报哪个错——快路径这时交回原代码重解。
func TestKongReadZstdRequestBodyTruncationLine(t *testing.T) {
	if testing.Short() {
		t.Skip("要解出 64 MiB 以上的数据")
	}
	head := kongEncodeAll(t, bytes.Repeat([]byte("h"), 1024))
	tail := kongEncodeAll(t, bytes.Repeat([]byte("z"), maxDecompressedBodySize), zstd.WithEncoderCRC(true))
	over := append(append([]byte(nil), head...), tail...)
	corrupt := append(append([]byte(nil), head...), kongFlip(tail, len(tail)-1)...)
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	for _, tc := range []kongReadCase{{name: "over_limit", wire: over}, {name: "crc_across_limit", wire: corrupt}} {
		_, handled, _ := kongDecodeZstd(context.Background(), tc.wire)
		require.False(t, handled, "%s：快路径应交回原代码", tc.name)
		tc.cl = int64(len(tc.wire))
		kongRequireReadSame(t, tc)
	}
}

// TestKongReadZstdRequestBodyUntrustedFCS：帧头声明 64 MiB、正文只有几个字节时，不能因为声明就分配 64 MiB。
func TestKongReadZstdRequestBodyUntrustedFCS(t *testing.T) {
	full := kongEncodeAll(t, bytes.Repeat([]byte("a"), maxDecompressedBodySize))
	var header zstd.Header
	require.NoError(t, header.Decode(full))
	require.True(t, header.HasFCS)
	wire := full[:header.HeaderSize]
	tc := kongReadCase{name: "lying_fcs", wire: wire, cl: int64(len(wire))}
	kongRequireReadSame(t, tc)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	kongReadWith(t, true, tc)
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "只有帧头的输入分配了 %d 字节", after.TotalAlloc-before.TotalAlloc)
}

// TestKongReadZstdLargeWindowNotPooled：窗口超过上限的输入（声明大窗口却只解出两个字节的合法帧、原始大小超过上限的
// 单段帧、第二帧才声明大窗口）都交回原代码，结果一致，池里也不留下大块历史缓冲。
func TestKongReadZstdLargeWindowNotPooled(t *testing.T) {
	kongZstdDecoderPool = sync.Pool{}
	defer func() { kongZstdDecoderPool = sync.Pool{} }()
	var big bytes.Buffer
	encoder, err := zstd.NewWriter(&big, zstd.WithWindowSize(32<<20))
	require.NoError(t, err)
	_, _ = encoder.Write(kongJSONBody(8, 64<<10, false))
	require.NoError(t, encoder.Flush()) // 先 Flush，帧头才按设定的窗口写；直接 Close 会写成按内容大小的单段帧
	require.NoError(t, encoder.Close())
	inputs := map[string][]byte{
		"tiny_frame_32mib_window": {0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x78, 0x11, 0x00, 0x00, 0x7b, 0x7d},
		"single_segment_over":     kongEncodeAll(t, kongJSONBody(5, kongZstdDecoderMaxWindow+(1<<20), false), zstd.WithWindowSize(16<<20)),
		"second_frame_big_window": append(kongEncodeAll(t, []byte(`{"a":`)), big.Bytes()...),
	}
	for name, wire := range inputs {
		_, handled, _ := kongDecodeZstd(context.Background(), wire)
		require.False(t, handled, "%s：快路径应交回原代码", name)
		kongRequireReadSame(t, kongReadCase{name: name, wire: wire, cl: int64(len(wire))})
	}

	// 只量快路径留在池里的东西：原代码每次新建解码器，用完即成垃圾，不必一起量。
	kongZstdDecoderPool = sync.Pool{}
	runtime.GC()
	runtime.GC()
	before, _ := kongHeapNow()
	for _, wire := range inputs {
		_, _, _ = kongDecodeZstd(context.Background(), wire)
	}
	runtime.GC() // 一轮 GC 后池里的对象仍被留着
	after, _ := kongHeapNow()
	require.Less(t, int64(after)-int64(before), int64(4<<20), "池里留下了大块历史缓冲")

	small := kongEncodeAll(t, kongJSONBody(6, 64<<10, false))
	for range 3 {
		out := kongReadWith(t, true, kongReadCase{name: "small", wire: small, cl: int64(len(small))})
		require.Empty(t, out.err)
	}
}

func kongHeapNow() (uint64, uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.TotalAlloc
}

// TestKongReadZstdRequestBodyBudget：带内存预算时，预算不足的报错与原代码相同；预算充足时正文相同，读完时快路径只
// 记着解出的那一份，释放后预算全部退还。
func TestKongReadZstdRequestBodyBudget(t *testing.T) {
	wire := kongEncodeAll(t, kongJSONBody(9, 600<<10, false))
	read := func(fast bool, limit int64) (out kongReadOutcome, held, left int64) {
		previous := KongRequestZstdFastpathEnabled
		KongRequestZstdFastpathEnabled = func() bool { return fast }
		defer func() { KongRequestZstdFastpathEnabled = previous }()
		budget := NewBodyMemoryBudget(limit)
		used := func() int64 {
			budget.mu.Lock()
			defer budget.mu.Unlock()
			return budget.used
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
		req.Header.Set("Content-Encoding", "zstd")
		ctx, release := WithRequestBodyMemoryBudget(req.Context(), budget)
		req = req.WithContext(ctx)
		body, err := ReadRequestBodyWithPrealloc(req)
		held = used()
		release()
		out.body = body
		if err != nil {
			out.err = err.Error()
		}
		return out, held, used()
	}

	want, _, wantLeft := read(false, 4<<20)
	got, _, gotLeft := read(true, 4<<20)
	require.Contains(t, want.err, ErrRequestBodyMemoryBudget.Error())
	require.Equal(t, want.err, got.err)
	require.Zero(t, wantLeft)
	require.Zero(t, gotLeft)

	want, _, wantLeft = read(false, 64<<20)
	got, held, gotLeft := read(true, 64<<20)
	require.Empty(t, want.err)
	require.Empty(t, got.err)
	require.True(t, bytes.Equal(want.body, got.body))
	require.Equal(t, int64(cap(got.body)), held)
	require.Zero(t, wantLeft)
	require.Zero(t, gotLeft)
}

// kongCancelAfter 的 Err 前 n 次返回 nil，之后返回 context.Canceled：让取消确定地落在第 n+1 次检查上。
type kongCancelAfter struct {
	context.Context
	n     int
	calls int
}

func (c *kongCancelAfter) Err() error {
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

// TestKongReadZstdRequestBodyCanceledWhileDecoding：解码中途请求被取消时，快路径交出读输出时的错误（带读取进度，
// 与原代码在同一处取消时一样），不重解；自己的预留全部退还。
func TestKongReadZstdRequestBodyCanceledWhileDecoding(t *testing.T) {
	wire := kongEncodeAll(t, kongJSONBody(10, 2<<20, false))
	budget := NewBodyMemoryBudget(64 << 20)
	base, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release()
	// 第 1 次检查在预留窗口，第 2 次在预留输出块，之后是读输出的循环。
	ctx := &kongCancelAfter{Context: base, n: 3}
	_, handled, err := kongDecodeZstd(ctx, wire)
	require.True(t, handled, "取消不交回原代码重解")
	var readErr *RequestBodyReadError
	require.ErrorAs(t, err, &readErr)
	require.ErrorIs(t, err, context.Canceled)
	budget.mu.Lock()
	used := budget.used
	budget.mu.Unlock()
	require.Zero(t, used, "窗口与输出块的预留都已退还")

	ctx = &kongCancelAfter{Context: base, n: 3}
	_, err = decompressRequestBodyWithBudget(ctx, "zstd", wire)
	require.ErrorAs(t, err, &readErr, "原代码在同一处取消时同样带读取进度")
}
