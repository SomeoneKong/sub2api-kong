//go:build unit

package handler

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"runtime/metrics"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kongcorpus"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 请求体快速路径的性能基准：一次完整的 Responses 请求（handler → Forward → 透传 → 假上游 → 转发响应 →
// 写用量），报告 ns/op、B/op、allocs/op，以及按 runtime/metrics 统计的每请求 CPU（用户代码加 GC）。
// 每个请求体只用一次前先复制：同一份字节反复使用会让按请求体登记的状态跨迭代残留。

// kongFPBenchUpstream 对每次调用回同一份 SSE。
type kongFPBenchUpstream struct {
	service.HTTPUpstream
	sse []byte
}

func (u *kongFPBenchUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(u.sse)),
	}, nil
}

var kongFPCPUMetrics = []string{"/cpu/classes/user:cpu-seconds", "/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/scavenge/total:cpu-seconds"}

func kongFPCPUSeconds() float64 {
	samples := make([]metrics.Sample, len(kongFPCPUMetrics))
	for i, name := range kongFPCPUMetrics {
		samples[i].Name = name
	}
	metrics.Read(samples)
	total := 0.0
	for _, sample := range samples {
		if sample.Value.Kind() == metrics.KindFloat64 {
			total += sample.Value.Float64()
		}
	}
	return total
}

func benchmarkKongFP(b *testing.B, shape kongcorpus.Shape, deltas int, edit func(*kongcorpus.Options)) {
	gin.SetMode(gin.TestMode)
	const variants = 8
	bodies := make([][]byte, variants)
	var headers http.Header
	for i := range bodies {
		o := kongcorpus.Options{Shape: shape, Seed: uint64(100 + i), TargetBytes: 1100 << 10}
		if edit != nil {
			edit(&o)
		}
		bodies[i] = kongcorpus.Request(o)
		headers = kongcorpus.Headers(o)
	}
	model := "gpt-6-sol"
	if shape == kongcorpus.ShapeLite {
		model = "gpt-6-astra"
	}
	upstream := &kongFPBenchUpstream{sse: kongcorpus.SSEResponse(1, model, deltas)}
	restoreZstd := service.KongSwapOpenAIRequestZstdForTest()
	defer restoreZstd()
	// logger 未初始化时 LegacyPrintf 会写标准库 log；基准期间丢弃，免得输出与写日志的开销混进结果。
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogOutput)
	handler := newKongFPHandler(b, upstream, &kongFPUsageRepo{})

	b.ReportAllocs()
	b.SetBytes(int64(len(bodies[0])))
	b.ResetTimer()
	startCPU := kongFPCPUSeconds()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		body := append([]byte(nil), bodies[i%variants]...)
		c, _ := newKongFPContext(kongFPCase{body: body, headers: headers})
		b.StartTimer()
		handler.Responses(c)
	}
	b.StopTimer()
	b.ReportMetric((kongFPCPUSeconds()-startCPU)*1e3/float64(b.N), "cpu-ms/op")
}

// 回落路径的变体：正文只是提到 python 一词（别名门照常跳过）、真有名为 python 的工具（别名门回落，请求体被
// 改写）、历史里写过 lookaround（schema 第二遍回落）。
var (
	kongFPBenchPythonInText = func(o *kongcorpus.Options) {
		o.SuffixItems = []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"run it with python please"}]}`}
	}
	kongFPBenchPythonTool = func(o *kongcorpus.Options) {
		o.ExtraTools = []string{`{"type":"function","name":"python","description":"run python","strict":false,"parameters":{"type":"object","properties":{}}}`}
	}
	kongFPBenchLookaround = func(o *kongcorpus.Options) {
		o.SuffixItems = []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"the regex (?=foo) is a lookahead"}]}`}
	}
)

func BenchmarkKongOpenAIBodyFastpath(b *testing.B) {
	for _, bc := range []struct {
		name   string
		shape  kongcorpus.Shape
		deltas int
		edit   func(*kongcorpus.Options)
	}{
		{"sol/minimal_response", kongcorpus.ShapeSol, 0, nil},
		{"lite/minimal_response", kongcorpus.ShapeLite, 0, nil},
		{"sol/full_response", kongcorpus.ShapeSol, 400, nil},
		{"lite/full_response", kongcorpus.ShapeLite, 400, nil},
		{"sol/python_in_text", kongcorpus.ShapeSol, 0, kongFPBenchPythonInText},
		{"sol/python_tool", kongcorpus.ShapeSol, 0, kongFPBenchPythonTool},
		{"sol/lookaround_in_history", kongcorpus.ShapeSol, 0, kongFPBenchLookaround},
	} {
		b.Run(strings.ReplaceAll(bc.name, " ", "_"), func(b *testing.B) {
			benchmarkKongFP(b, bc.shape, bc.deltas, bc.edit)
		})
	}
}
