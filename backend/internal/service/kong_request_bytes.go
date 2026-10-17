package service

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
)

// 逐请求的上下行字节数（fork 专有，设计见 DESIGN-openai-request-bytes.md）。
//
// 用途是核算带宽：每个用量行记下这个请求在客户端一侧读写了多少消息体字节，以及经哪个账号、哪个出口（账号代理
// 的端口）向上游发了多少、收了多少。出口按**发送当时**的代理记，账号事后调换出口不影响已写的行。
//
// 数据流：
//   - 网关路由的请求体限制中间件外面挂一个计数容器（routes/kong_request_bytes.go），同时把客户端请求体包一层计数，
//     得到 in；handler 读完请求体记下 body。
//   - 每一次真正交给 HTTPUpstream 的 OpenAI 发送都经 kongDoUpstreamCounted：按（账号，端口）分组，包请求体计 up /
//     up_wire、计 sends，并把这组挂进请求 context；repository 的 doUpstreamRequest 在解压之前据此包响应体计 down。
//     交给插件的发送只计次数。
//   - handler 提交用量之前同步调用 KongMergeRequestBytes，连同写给客户端的字节数 out 合并进请求特征。
//
// 只计账号平台为 OpenAI 的发送与用量行；没有容器的 context（后台任务、WS）全部是空操作。

// KongRequestBytes 是一个客户端请求的计数容器。各字段会被不同 goroutine 更新（上游响应体由服务层读，心跳由另一
// 个 goroutine 写，合并在 handler），所以都用原子量或锁。
type KongRequestBytes struct {
	in     atomic.Int64
	body   atomic.Int64
	plugin atomic.Int64

	mu     sync.Mutex
	groups []*kongBytesGroup // 按首次出现的顺序
}

// kongBytesGroup 是同一请求里经同一（账号，端口）的全部发送。
type kongBytesGroup struct {
	account int64
	port    int
	up      atomic.Int64
	upWire  atomic.Int64
	sends   atomic.Int64
	down    atomic.Int64
	last    atomic.Int64 // 最近一次发送的序号，合并时用来挑行账号的那一组
}

type kongRequestBytesKey struct{}

type kongUpstreamDownKey struct{}

// NewKongRequestBytes 新建一个计数容器。
func NewKongRequestBytes() *KongRequestBytes {
	return &KongRequestBytes{}
}

// WithKongRequestBytes 把计数容器挂到 context 上。
func WithKongRequestBytes(ctx context.Context, rb *KongRequestBytes) context.Context {
	return context.WithValue(ctx, kongRequestBytesKey{}, rb)
}

// KongRequestBytesFromContext 取出 context 上的计数容器，没有时返回 nil。
func KongRequestBytesFromContext(ctx context.Context) *KongRequestBytes {
	if ctx == nil {
		return nil
	}
	rb, _ := ctx.Value(kongRequestBytesKey{}).(*KongRequestBytes)
	return rb
}

// CountInbound 给客户端请求体包一层计数，读到的原始字节（解压前）记进 in。
func (rb *KongRequestBytes) CountInbound(body io.ReadCloser) io.ReadCloser {
	return &kongCountingReadCloser{ReadCloser: body, n: &rb.in}
}

// KongRecordRequestBodyLen 记下 handler 读体函数返回的请求体长度。
func KongRecordRequestBodyLen(ctx context.Context, n int) {
	if rb := KongRequestBytesFromContext(ctx); rb != nil {
		rb.body.Store(int64(n))
	}
}

// kongRequestBytesGroupFor 返回这次发送应当计入的分组；context 上没有容器或账号平台不是 OpenAI 时返回 nil。
func kongRequestBytesGroupFor(ctx context.Context, account *Account, proxyURL string) *kongBytesGroup {
	if account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	rb := KongRequestBytesFromContext(ctx)
	if rb == nil {
		return nil
	}
	port := kongProxyPort(proxyURL)
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, g := range rb.groups {
		if g.account == account.ID && g.port == port {
			return g
		}
	}
	g := &kongBytesGroup{account: account.ID, port: port}
	rb.groups = append(rb.groups, g)
	return g
}

// kongProxyPort 取代理 URL 的端口；没有代理、没写端口或解析不了时为 0。
func kongProxyPort(proxyURL string) int {
	if proxyURL == "" {
		return 0
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(u.Port())
	return port
}

var kongSendSeq atomic.Int64

// kongDoUpstreamCounted 是每一次真正交给 HTTPUpstream 的 OpenAI 发送的入口：计数之后照原样调用 httpUpstream.Do。
//
// plainLen 是这次发送的明文长度（网关改写后、压缩前）。compress 会把 Body、GetBody、ContentLength 换成压缩后的
// 版本，所以由调用方在压缩之前取好传进来；为负表示不知道，此时按这次发送的正文读到的字节数计。
func (s *OpenAIGatewayService) kongDoUpstreamCounted(request *http.Request, proxyURL string, account *Account, plainLen int64) (*http.Response, error) {
	if g := kongRequestBytesGroupFor(request.Context(), account, proxyURL); g != nil {
		request = g.prepareSend(request, plainLen)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// prepareSend 记一次发送，返回一个浅拷贝的请求：请求体与 GetBody 给出的 reader 都包了计数，context 上挂着这一组的
// down 计数，供 doUpstreamRequest 包响应体。调用方手里的原请求不变（明文重发时由 restore 换回明文正文）。
func (g *kongBytesGroup) prepareSend(request *http.Request, plainLen int64) *http.Request {
	g.sends.Add(1)
	g.last.Store(kongSendSeq.Add(1))
	out := request.WithContext(KongWithUpstreamDownCounter(request.Context(), &g.down))

	// up 每次发送只计一次：正文第一次被 transport 读取时计入明文长度。重定向与连接级重试经 GetBody 重放的
	// 正文只计入 up_wire。建连就失败、正文没被读的发送，两项都不加。
	started := &atomic.Bool{}
	var plainUp *atomic.Int64
	if plainLen < 0 {
		plainUp = &g.up
	}
	onFirstRead := func() {
		if started.CompareAndSwap(false, true) && plainLen > 0 {
			g.up.Add(plainLen)
		}
	}
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = &kongCountingReadCloser{ReadCloser: out.Body, n: &g.upWire, extra: plainUp, onFirstRead: onFirstRead}
	}
	if getBody := out.GetBody; getBody != nil {
		out.GetBody = func() (io.ReadCloser, error) {
			rc, err := getBody()
			if err != nil || rc == nil || rc == http.NoBody {
				return rc, err
			}
			return &kongCountingReadCloser{ReadCloser: rc, n: &g.upWire, onFirstRead: onFirstRead}, nil
		}
	}
	return out
}

// kongCountPluginSend 记一次交给插件的发送。插件自带传输层，字节量不到，只计次数。
func kongCountPluginSend(ctx context.Context, account *Account) {
	if account == nil || account.Platform != PlatformOpenAI {
		return
	}
	if rb := KongRequestBytesFromContext(ctx); rb != nil {
		rb.plugin.Add(1)
	}
}

// KongWithUpstreamDownCounter 让 HTTPUpstream 把这个请求的响应体原始字节（解压之前）计进 n。
func KongWithUpstreamDownCounter(ctx context.Context, n *atomic.Int64) context.Context {
	return context.WithValue(ctx, kongUpstreamDownKey{}, n)
}

// KongCountUpstreamResponseBody 在 HTTPUpstream 解压响应之前给响应体包一层计数。只有经 kongDoUpstreamCounted 发出的
// 请求在 context 上带着计数，其余请求不受影响。
func KongCountUpstreamResponseBody(ctx context.Context, resp *http.Response) {
	if ctx == nil || resp == nil || resp.Body == nil {
		return
	}
	n, _ := ctx.Value(kongUpstreamDownKey{}).(*atomic.Int64)
	if n == nil {
		return
	}
	resp.Body = &kongCountingReadCloser{ReadCloser: resp.Body, n: n}
}

// KongRequestBytesFeature 是用量行请求特征里的 bytes 对象，口径见 DESIGN-openai-request-bytes.md §2。
type KongRequestBytesFeature struct {
	In     int64                   `json:"in"`
	Body   int64                   `json:"body"`
	Out    int64                   `json:"out"`
	Port   int                     `json:"port"`
	Up     int64                   `json:"up"`
	UpWire int64                   `json:"up_wire"`
	Sends  int64                   `json:"sends"`
	Down   int64                   `json:"down"`
	Others []KongRequestBytesOther `json:"others,omitempty"`
	Plugin int64                   `json:"plugin,omitempty"`
}

// KongRequestBytesOther 是同一请求里经其他（账号，端口）的发送。
type KongRequestBytesOther struct {
	Account int64 `json:"account"`
	Port    int   `json:"port"`
	Up      int64 `json:"up"`
	UpWire  int64 `json:"up_wire"`
	Sends   int64 `json:"sends"`
	Down    int64 `json:"down"`
}

// KongMergeRequestBytes 把计数容器的内容合并进用量结果的请求特征。必须在提交异步用量任务之前同步调用：written
// 取自 gin 的 c.Writer.Size()，handler 返回后 gin context 会被回收复用（-1 表示没写过，按 0 记）。
//
// 行的账号最近一次发送所在的那组放顶层，其余各组放 others；行的账号没有经 HTTP 的发送时（例如全部交给了插件）
// 顶层各项为 0。只在行的账号平台为 OpenAI 时写。
func KongMergeRequestBytes(ctx context.Context, res *OpenAIForwardResult, account *Account, written int) {
	if res == nil || account == nil || account.Platform != PlatformOpenAI {
		return
	}
	rb := KongRequestBytesFromContext(ctx)
	if rb == nil {
		return
	}
	feature := &KongRequestBytesFeature{
		In:     rb.in.Load(),
		Body:   rb.body.Load(),
		Out:    int64(max(written, 0)),
		Plugin: rb.plugin.Load(),
	}
	rb.mu.Lock()
	groups := append([]*kongBytesGroup(nil), rb.groups...)
	rb.mu.Unlock()

	var top *kongBytesGroup
	for _, g := range groups {
		if g.account == account.ID && (top == nil || g.last.Load() > top.last.Load()) {
			top = g
		}
	}
	for _, g := range groups {
		if g == top {
			feature.Port = g.port
			feature.Up, feature.UpWire, feature.Sends, feature.Down = g.up.Load(), g.upWire.Load(), g.sends.Load(), g.down.Load()
			continue
		}
		feature.Others = append(feature.Others, KongRequestBytesOther{
			Account: g.account, Port: g.port,
			Up: g.up.Load(), UpWire: g.upWire.Load(), Sends: g.sends.Load(), Down: g.down.Load(),
		})
	}

	if res.KongRequestFeatures == nil {
		res.KongRequestFeatures = &KongRequestFeatures{}
	}
	res.KongRequestFeatures.Bytes = feature
}

// kongCountingReadCloser 在底层 Read 返回之后、自己返回之前把读到的字节记上，所以合并之前完成的读取一定在账上。
type kongCountingReadCloser struct {
	io.ReadCloser
	n           *atomic.Int64
	extra       *atomic.Int64 // 同时计入的另一个计数，可为 nil
	onFirstRead func()        // 每次 Read 之前调用，由它自己保证只生效一次；可为 nil
}

func (r *kongCountingReadCloser) Read(p []byte) (int, error) {
	if r.onFirstRead != nil {
		r.onFirstRead()
	}
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.n.Add(int64(n))
		if r.extra != nil {
			r.extra.Add(int64(n))
		}
	}
	return n, err
}
