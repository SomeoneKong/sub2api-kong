//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// 请求体内存的量法（DESIGN-openai-request-body-memory.md §4）：一份线上形态的请求（带 id、夹图片、入站 zstd）经
// handler → Forward → 透传发往假上游。假上游像真实 transport 一样边读边丢弃请求体、把请求挂在 Response.Request
// 上；block 为真时响应体第一次被读就停住，用来量流式期间的存活。

type kongMemUpstream struct {
	service.HTTPUpstream
	block     bool
	streaming chan struct{}
	release   chan struct{}
	once      sync.Once
}

func newKongMemUpstream(block bool) *kongMemUpstream {
	return &kongMemUpstream{block: block, streaming: make(chan struct{}), release: make(chan struct{})}
}

type kongMemBody struct {
	u    *kongMemUpstream
	r    io.Reader
	wait bool
}

func (b *kongMemBody) Read(p []byte) (int, error) {
	if b.wait {
		b.wait = false
		b.u.once.Do(func() { close(b.u.streaming) })
		<-b.u.release
	}
	return b.r.Read(p)
}

func (b *kongMemBody) Close() error { return nil }

func (u *kongMemUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"req_mem"}},
		Body:       &kongMemBody{u: u, r: bytes.NewReader(kongcorpus.SSEResponse(1, "gpt-6-sol", 40)), wait: u.block},
		Request:    req,
	}, nil
}

type kongMemRequest struct {
	plain   []byte
	wire    []byte
	headers http.Header
}

func newKongMemRequest(t testing.TB) kongMemRequest {
	t.Helper()
	o := kongcorpus.Options{Shape: kongcorpus.ShapeSol, Seed: 7, TargetBytes: 2700 << 10, ItemIDs: true, Images: 6, ImageBytes: 300 << 10}
	plain := kongcorpus.Request(o)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	require.NoError(t, err)
	wire := encoder.EncodeAll(plain, nil)
	require.NoError(t, encoder.Close())
	headers := kongcorpus.Headers(o)
	headers.Set("Content-Encoding", "zstd")
	return kongMemRequest{plain: plain, wire: wire, headers: headers}
}

// prepare 装好一次请求要用的 handler 与 gin 上下文，返回执行它的函数；装配本身的分配不算进请求。upstream 为 nil
// 时用不阻塞的假上游。
func (m kongMemRequest) prepare(t testing.TB, upstream *kongMemUpstream) func() {
	if upstream == nil {
		upstream = newKongMemUpstream(false)
	}
	handler := newKongFPHandler(t, upstream, &kongFPUsageRepo{})
	c, _ := newKongFPContext(kongFPCase{name: "mem", body: m.wire, headers: m.headers})
	return func() {
		handler.Responses(c)
		require.Equal(t, http.StatusOK, c.Writer.Status())
	}
}

func (m kongMemRequest) serve(t testing.TB) { m.prepare(t, nil)() }

func kongMemHeap() (heap, total uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.TotalAlloc
}

// liveCopies 量流式期间请求自己持有的存活：测量前强制 GC，池子被清空，量到的正是请求引用着的版本。
func (m kongMemRequest) liveCopies(t testing.TB) float64 {
	upstream := newKongMemUpstream(true)
	run := m.prepare(t, upstream)
	runtime.GC()
	runtime.GC()
	base, _ := kongMemHeap()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-upstream.streaming:
	case <-done:
		t.Fatal("请求没有走到流式阶段")
	}
	runtime.GC()
	runtime.GC()
	live, _ := kongMemHeap()
	close(upstream.release)
	<-done
	return float64(int64(live)-int64(base)) / float64(len(m.plain))
}

// allocCopies 先处理 warmup 个请求，再量随后 n 个请求平均每个的分配。从预热到计量结束都暂停 GC：测试进程的存活堆
// 很小，GC 每分配几 MB 就跑一轮，sync.Pool 隔两轮就被清空，编码器、解码器几乎每个请求都要重建；线上 GC 隔几秒
// 一轮，池子是热的。装配放在暂停之前，免得恢复 GC 后为下一批装配又把池子清掉。
func (m kongMemRequest) allocCopies(t testing.TB, warmup, n int) float64 {
	runs := make([]func(), warmup+n)
	for i := range runs {
		runs[i] = m.prepare(t, nil)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, run := range runs[:warmup] {
		run()
	}
	_, before := kongMemHeap()
	for _, run := range runs[warmup:] {
		run()
	}
	_, after := kongMemHeap()
	return float64(after-before) / float64(n) / float64(len(m.plain))
}

// poolRetention 量并发处理一批请求之后池子留下的内存（入站解码器、出站编码器与其它 sync.Pool）：先装好请求、清空
// 各个池，暂停 GC 后同时起跑；跑完一轮 GC 后池里的对象移到备用区、仍然被留着，再两轮才被清掉，两者之差就是池子的
// 保留。不暂停 GC 的话，归还的对象可能已经熬过一轮 GC，量出来会偏少甚至是 0。
func (m kongMemRequest) poolRetention(t testing.TB, concurrency int) uint64 {
	runs := make([]func(), concurrency)
	for i := range runs {
		runs[i] = m.prepare(t, nil)
	}
	runtime.GC()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run()
		}()
	}
	close(start)
	wg.Wait()
	runtime.GC()
	kept, _ := kongMemHeap()
	runtime.GC()
	runtime.GC()
	cleared, _ := kongMemHeap()
	if kept < cleared {
		return 0
	}
	return kept - cleared
}

func TestKongOpenAIBodyMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("要处理几十个 3 MB 的请求")
	}
	gin.SetMode(gin.TestMode)
	defer service.KongSwapOpenAIRequestZstdForTest()()
	m := newKongMemRequest(t)
	t.Logf("请求体 %.2f MB，入站压缩后 %.2f MB", float64(len(m.plain))/1e6, float64(len(m.wire))/1e6)

	type result struct{ live, cold, warm, pool float64 }
	measure := func(on bool) result {
		defer service.KongSetOpenAIBodyFastpathForTest(on)()
		m.serve(t) // 开关的 sync.OnceValue 等一次性的初始化不算进来
		var r result
		r.live = m.liveCopies(t)
		runtime.GC()
		runtime.GC()
		r.cold = m.allocCopies(t, 0, 1)
		r.warm = m.allocCopies(t, 3, 8)
		r.pool = float64(m.poolRetention(t, 4)) / 1e6
		return r
	}
	off := measure(false)
	on := measure(true)
	for name, r := range map[string]result{"全关": off, "全开": on} {
		t.Logf("%s：流式期间请求持有的存活 %.2f 份；预热后每请求分配 %.2f 份（冷启动 %.2f 份）；并发 4 个之后池子保留 %.1f MB",
			name, r.live, r.warm, r.cold, r.pool)
	}
	require.LessOrEqual(t, on.live, 2.3, "流式期间存活应只剩入站与最终版本")
	// 全量测试时其他包并行在跑、前面用例留下的后台协程也在分配，计量窗口里会混进少量别人的分配，所以留一份余量。
	require.LessOrEqual(t, on.warm, 7.0, "预热后每请求分配")
}
