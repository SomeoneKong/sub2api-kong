package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// 发往官方 ChatGPT 后端的请求体 zstd 压缩。
//
// 目标是省出站流量：codex 每轮都把整段上下文重发一遍，请求体动辄上 MB，而上行是本服务出站流量的
// 大头。顺带缩小与官方客户端的差异——官方客户端直连时对 /responses 就是 zstd level 3 压缩发送。
// 不追求与官方逐字节一致：条件比官方宽（不限路径、不要求流式），编码器是 klauspost 而不是 libzstd。
//
// 上游对某个端点不接受压缩时（415，或 400 / 422 且错误信息表明请求体解码或 JSON 解析失败），用明文
// 重发一次；明文通过了，才把该端点记为不压缩一段时间。上游那一刻没能解析请求，不存在重复执行；端点
// 支持情况变化时也不会断服务。

// KongOpenAIRequestZstdEnv 是开关：不设置为开，false 关闭，写错按关闭处理并记错误日志。
const KongOpenAIRequestZstdEnv = "KONG_OPENAI_REQUEST_ZSTD"

const (
	// kongZstdMinBodyBytes 以下的请求体压了也省不了多少，却多一个出错面。
	kongZstdMinBodyBytes = 1 << 10
	// kongZstdEndpointOffTTL 是端点被判为不接受压缩后的冷却期，过期后再试一次。
	kongZstdEndpointOffTTL = 24 * time.Hour
	// kongZstdStatsInterval 是汇总日志的间隔。
	kongZstdStatsInterval = 10 * time.Minute
	// kongZstdErrorPeekBytes 是判定拒绝时最多读取的错误正文长度。
	kongZstdErrorPeekBytes = 64 << 10
)

// kongZstdRejectMarkers 是错误正文里表明"请求体没被正确解码"的片段（小写匹配）。只收专指正文解码
// 失败的写法：zstd 帧以 0x28（'('）开头，JSON 解析器会在第一个字符就报错，各框架的报错都点得出
// 位置或这个字符。"invalid json" 这类宽泛说法会命中字段与 schema 校验错误，让一次业务错误变成两次
// 请求，不收。
var kongZstdRejectMarkers = []string{
	"content-encoding",
	"zstd",
	"decompress",
	"parse the json body",              // OpenAI API
	"json_invalid",                     // pydantic
	"json decode error",                // FastAPI
	"expecting value: line 1 column 1", // Python json
	"invalid character '('",            // Go encoding/json
	"unexpected token '('",             // Node.js JSON.parse
	"in json at position 0",            // Node.js JSON.parse（旧版写法）
	"error parsing the body",           // Starlette
}

type kongOpenAIRequestZstd struct {
	enabled bool
	now     func() time.Time

	mu      sync.Mutex
	off     map[string]time.Time
	stats   kongZstdStats
	lastLog time.Time
}

type kongZstdStats struct {
	compressed int64
	rawBytes   int64
	zstdBytes  int64
	resent     int64 // 压缩请求被拒后的明文重发
	markedOff  int64 // 明文重发通过、端点因此改发明文
	skippedOff int64 // 端点在冷却期内、按明文发送
	failures   int64
}

var kongOpenAIRequestZstdDefault = sync.OnceValue(func() *kongOpenAIRequestZstd {
	value, present := os.LookupEnv(KongOpenAIRequestZstdEnv)
	enabled, err := kongParseOpenAIRequestZstd(value, present)
	if err != nil {
		slog.Error("kong zstd: 出站请求压缩已关闭", "error", err)
	}
	return newKongOpenAIRequestZstd(enabled, time.Now)
})

// kongOpenAIRequestZstdInstance 取当前生效的实例；测试替换它来注入开关与时钟。
var kongOpenAIRequestZstdInstance = func() *kongOpenAIRequestZstd { return kongOpenAIRequestZstdDefault() }

func newKongOpenAIRequestZstd(enabled bool, now func() time.Time) *kongOpenAIRequestZstd {
	return &kongOpenAIRequestZstd{enabled: enabled, now: now, off: make(map[string]time.Time), lastLog: now()}
}

func kongParseOpenAIRequestZstd(value string, present bool) (bool, error) {
	if !present {
		return true, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s 必须是 true / false，得到 %q", KongOpenAIRequestZstdEnv, value)
	}
	return enabled, nil
}

// kongSendOpenAIUpstreamCompressed 按条件压缩请求体后发送；上游像是不接受压缩时用明文重发一次。
//
// 插件判定只做一次。交给插件的请求保持明文、走原发送路径；压缩了的请求连同明文重发都直接交给
// HTTPUpstream，不再经过 sendOpenAIUpstream 的插件判定：插件绑定可在线切换，两次判定之间一旦切了，
// 压缩过的正文就会落进自带传输层的插件，或者重发换了一条发送路径。
func (s *OpenAIGatewayService) kongSendOpenAIUpstreamCompressed(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	routedToPlugin := s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account)
	z := kongOpenAIRequestZstdInstance()
	plain, endpoint, compressed := z.compress(request, routedToPlugin)
	if !compressed {
		return s.sendOpenAIUpstream(request, proxyURL, account)
	}
	send := func() (*http.Response, error) {
		return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	response, err := send()
	if err != nil || response == nil || !kongZstdRejected(response) {
		return response, err
	}
	status := response.StatusCode
	if response.Body != nil {
		_ = response.Body.Close()
	}
	kongZstdRestorePlain(request, plain)
	z.record(func(st *kongZstdStats) { st.resent++ })
	response, err = send()
	if err != nil || response == nil {
		return response, err
	}
	// 明文也被同样拒绝，说明问题不在压缩，端点照旧压缩；明文通过了，才改发明文。
	if kongZstdRejected(response) {
		slog.Info("kong zstd: 明文重发仍被拒，与压缩无关", "endpoint", endpoint, "status", response.StatusCode)
	} else {
		z.markOff(endpoint, status)
	}
	return response, nil
}

// compress 在满足条件时把请求体换成 zstd 压缩后的字节，返回原明文与端点键。
func (z *kongOpenAIRequestZstd) compress(request *http.Request, routedToPlugin bool) ([]byte, string, bool) {
	if z == nil || !z.enabled || routedToPlugin || !kongZstdEligible(request) {
		return nil, "", false
	}
	endpoint := request.Method + " " + strings.ToLower(request.URL.Hostname()) + request.URL.Path
	if z.isOff(endpoint) {
		z.record(func(st *kongZstdStats) { st.skippedOff++ })
		return nil, "", false
	}
	plain, err := kongZstdReadBody(request)
	if err != nil || len(plain) < kongZstdMinBodyBytes || !kongZstdLooksLikeJSON(plain) {
		return nil, "", false
	}
	encoded, err := kongZstdEncode(plain)
	if err != nil {
		z.record(func(st *kongZstdStats) { st.failures++ })
		slog.Warn("kong zstd: 压缩失败，按明文发送", "endpoint", endpoint, "bytes", len(plain), "error", err)
		return nil, "", false
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	request.Body = io.NopCloser(bytes.NewReader(encoded))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	request.ContentLength = int64(len(encoded))
	request.Header.Set("Content-Encoding", "zstd")
	z.record(func(st *kongZstdStats) {
		st.compressed++
		st.rawBytes += int64(len(plain))
		st.zstdBytes += int64(len(encoded))
	})
	return plain, endpoint, true
}

// kongZstdEligible 判断请求是否在压缩范围内：发往官方 ChatGPT 后端的 JSON POST。只看最终出站
// 请求，入站的 Content-Encoding 在读入时已解码并删除，不会影响这里。
func kongZstdEligible(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	if request.Method != http.MethodPost || request.GetBody == nil {
		return false
	}
	host := strings.ToLower(request.URL.Hostname())
	if host != "chatgpt.com" && !strings.HasSuffix(host, ".chatgpt.com") {
		return false
	}
	if strings.TrimSpace(request.Header.Get("Content-Encoding")) != "" {
		return false
	}
	contentType := strings.ToLower(request.Header.Get("Content-Type"))
	return contentType == "" || strings.Contains(contentType, "json")
}

// kongZstdReadBody 经 GetBody 读出明文，不消耗 request.Body。
func kongZstdReadBody(request *http.Request) ([]byte, error) {
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return io.ReadAll(body)
}

func kongZstdLooksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

var kongZstdEncoderPool = sync.Pool{
	New: func() any {
		// 参数取 codex 的：level 3、不写校验和；窗口 2 MiB 是 libzstd level 3 在大小未知时的取值。
		encoder, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderCRC(false),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(2<<20))
		if err != nil {
			return nil
		}
		return encoder
	},
}

func kongZstdEncode(body []byte) ([]byte, error) {
	encoder, _ := kongZstdEncoderPool.Get().(*zstd.Encoder)
	if encoder == nil {
		return nil, errors.New("zstd encoder unavailable")
	}
	defer func() {
		// 放回池前断开对输出缓冲的引用，免得池里的编码器拖住上一个请求体。
		encoder.Reset(nil)
		kongZstdEncoderPool.Put(encoder)
	}()
	var out bytes.Buffer
	out.Grow(len(body) / 2)
	encoder.Reset(&out)
	if _, err := encoder.Write(body); err != nil {
		return nil, err
	}
	// 先 Flush 再 Close：klauspost 对单块能装下的小请求体会在帧头写入内容大小，先刷出一块就与
	// codex 的 libzstd 流式输出一样，帧头不带内容大小（28 b5 2f fd 00 58）。
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// kongZstdRejected 判断一个对压缩请求的响应是否表明上游没能解码请求体。读过的错误正文放回 Body。
func kongZstdRejected(response *http.Response) bool {
	switch response.StatusCode {
	case http.StatusUnsupportedMediaType:
		return true
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
	default:
		return false
	}
	if response.Body == nil {
		return false
	}
	peek, err := io.ReadAll(io.LimitReader(response.Body, kongZstdErrorPeekBytes))
	response.Body = kongZstdPrefixedBody{Reader: io.MultiReader(bytes.NewReader(peek), response.Body), closer: response.Body}
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(peek))
	for _, marker := range kongZstdRejectMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

type kongZstdPrefixedBody struct {
	io.Reader
	closer io.Closer
}

func (b kongZstdPrefixedBody) Close() error { return b.closer.Close() }

func kongZstdRestorePlain(request *http.Request, plain []byte) {
	request.Body = io.NopCloser(bytes.NewReader(plain))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(plain)), nil }
	request.ContentLength = int64(len(plain))
	request.Header.Del("Content-Encoding")
}

func (z *kongOpenAIRequestZstd) isOff(endpoint string) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	until, ok := z.off[endpoint]
	if !ok {
		return false
	}
	if !z.now().Before(until) {
		delete(z.off, endpoint)
		return false
	}
	return true
}

func (z *kongOpenAIRequestZstd) markOff(endpoint string, status int) {
	z.mu.Lock()
	z.off[endpoint] = z.now().Add(kongZstdEndpointOffTTL)
	z.mu.Unlock()
	z.record(func(st *kongZstdStats) { st.markedOff++ })
	slog.Warn("kong zstd: 上游不接受压缩请求体，该端点改发明文",
		"endpoint", endpoint, "status", status, "retry_after", kongZstdEndpointOffTTL.String())
}

// record 更新计数，并按间隔输出一行汇总（借请求触发，不另开协程；没有请求也就没什么可报）。
func (z *kongOpenAIRequestZstd) record(update func(*kongZstdStats)) {
	z.mu.Lock()
	update(&z.stats)
	now := z.now()
	if now.Sub(z.lastLog) < kongZstdStatsInterval {
		z.mu.Unlock()
		return
	}
	st := z.stats
	z.stats = kongZstdStats{}
	z.lastLog = now
	off := make([]string, 0, len(z.off))
	for endpoint, until := range z.off {
		if now.Before(until) {
			off = append(off, endpoint)
		}
	}
	z.mu.Unlock()
	sort.Strings(off)
	saved := 0.0
	if st.rawBytes > 0 {
		saved = 1 - float64(st.zstdBytes)/float64(st.rawBytes)
	}
	slog.Info("kong zstd: 周期计数",
		"compressed", st.compressed, "raw_bytes", st.rawBytes, "zstd_bytes", st.zstdBytes,
		"saved_ratio", strconv.FormatFloat(saved, 'f', 3, 64),
		"resent", st.resent, "marked_off", st.markedOff, "skipped_off", st.skippedOff, "failures", st.failures,
		"off_endpoints", off)
}
