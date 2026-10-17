//go:build unit

package routes

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type kongBytesSeen struct {
	rb   *service.KongRequestBytes
	body []byte
	err  error
}

func (s *kongBytesSeen) handler(c *gin.Context) {
	s.rb = service.KongRequestBytesFromContext(c.Request.Context())
	s.body, s.err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	c.Status(http.StatusNoContent)
}

// kongBytesTestEngine 按网关路由的两种套法挂计数中间件：组上 bodyLimit、路由上再 textBodyLimit（/v1 与
// /backend-api/codex 的 alpha search），以及只有 textBodyLimit（根路径的 alpha search）。
func kongBytesTestEngine(seen *kongBytesSeen, limit int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	bodyLimit := kongCountRequestBytes(middleware.RequestBodyLimit(limit))
	textBodyLimit := kongCountRequestBytes(middleware.RequestBodyLimit(limit))
	r := gin.New()
	group := r.Group("/v1")
	group.Use(bodyLimit)
	group.POST("/alpha/search", textBodyLimit, seen.handler)
	r.POST("/alpha/search", textBodyLimit, seen.handler)
	return r
}

func kongBytesPost(r *gin.Engine, path string, body []byte, encoding string) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
}

func kongBytesFeature(t *testing.T, rb *service.KongRequestBytes) *service.KongRequestBytesFeature {
	t.Helper()
	require.NotNil(t, rb)
	res := &service.OpenAIForwardResult{}
	service.KongMergeRequestBytes(service.WithKongRequestBytes(t.Context(), rb), res, &service.Account{ID: 1, Platform: service.PlatformOpenAI}, 0)
	return res.KongRequestFeatures.Bytes
}

func TestKongCountRequestBytesInboundEncodings(t *testing.T) {
	plain := []byte(`{"model":"gpt-6.1-sol","input":"` + strings.Repeat("search ", 400) + `"}`)

	var zbuf bytes.Buffer
	zw, err := zstd.NewWriter(&zbuf)
	require.NoError(t, err)
	_, _ = zw.Write(plain)
	require.NoError(t, zw.Close())
	var gbuf bytes.Buffer
	gw := gzip.NewWriter(&gbuf)
	_, _ = gw.Write(plain)
	require.NoError(t, gw.Close())

	for _, tc := range []struct {
		name, encoding string
		wire           []byte
	}{
		{"plain", "", plain},
		{"zstd", "zstd", zbuf.Bytes()},
		{"gzip", "gzip", gbuf.Bytes()},
	} {
		for _, path := range []string{"/v1/alpha/search", "/alpha/search"} {
			t.Run(tc.name+path, func(t *testing.T) {
				seen := &kongBytesSeen{}
				kongBytesPost(kongBytesTestEngine(seen, 1<<20), path, tc.wire, tc.encoding)
				require.NoError(t, seen.err)
				require.Equal(t, plain, seen.body)
				service.KongRecordRequestBodyLen(service.WithKongRequestBytes(t.Context(), seen.rb), len(seen.body))
				b := kongBytesFeature(t, seen.rb)
				require.Equal(t, int64(len(tc.wire)), b.In, "in 是解压前从连接上读进来的字节，只计一次")
				require.Equal(t, int64(len(plain)), b.Body)
			})
		}
	}
}

// 先经 bodyLimit、再经 textBodyLimit 的请求只建一个容器：handler 看到的就是外层建的那个。
func TestKongCountRequestBytesIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	seen := &kongBytesSeen{}
	var outer *service.KongRequestBytes
	r := gin.New()
	r.POST("/x",
		kongCountRequestBytes(middleware.RequestBodyLimit(1<<20)),
		func(c *gin.Context) { outer = service.KongRequestBytesFromContext(c.Request.Context()); c.Next() },
		kongCountRequestBytes(middleware.RequestBodyLimit(1<<20)),
		seen.handler,
	)
	kongBytesPost(r, "/x", []byte(`{"a":"b"}`), "")
	require.NoError(t, seen.err)
	require.NotNil(t, outer)
	require.Same(t, outer, seen.rb)
	require.Equal(t, int64(len(`{"a":"b"}`)), kongBytesFeature(t, seen.rb).In)
}

func TestKongCountRequestBytesKeepsMaxBytesError(t *testing.T) {
	seen := &kongBytesSeen{}
	kongBytesPost(kongBytesTestEngine(seen, 16), "/alpha/search", []byte(strings.Repeat("x", 100)), "")
	var maxErr *http.MaxBytesError
	require.True(t, errors.As(seen.err, &maxErr), "超限错误原样上抛：%v", seen.err)
}

// 中间件预读后回填（分组模型白名单、组合路由那样），in 只计从连接上读的那一次。
func TestKongCountRequestBytesPrereadNotDoubleCounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	seen := &kongBytesSeen{}
	body := []byte(`{"model":"gpt-6.1-sol"}`)
	r := gin.New()
	r.POST("/x",
		kongCountRequestBytes(middleware.RequestBodyLimit(1<<20)),
		func(c *gin.Context) {
			raw, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
			require.NoError(t, err)
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Next()
		},
		seen.handler,
	)
	kongBytesPost(r, "/x", body, "")
	require.NoError(t, seen.err)
	require.Equal(t, body, seen.body)
	require.Equal(t, int64(len(body)), kongBytesFeature(t, seen.rb).In)
}

// 网关路由上 Responses 与 alpha search 的各个入口都带计数容器。
func TestKongGatewayRoutesAttachRequestBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var got *service.KongRequestBytes
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Next()
		got = service.KongRequestBytesFromContext(c.Request.Context())
	})
	RegisterGatewayRoutes(
		engine,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			groupID := int64(1)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				GroupID: &groupID,
				Group:   &service.Group{Platform: service.PlatformOpenAI},
			})
			c.Next()
		}),
		nil, nil, nil, nil, nil,
		&config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}},
	)
	for _, path := range []string{
		"/v1/responses",
		"/responses",
		"/backend-api/codex/responses",
		"/v1/alpha/search",
		"/alpha/search",
		"/backend-api/codex/alpha/search",
	} {
		got = nil
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-6.1-sol"}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(httptest.NewRecorder(), req)
		require.NotNil(t, got, "path=%s 应带计数容器", path)
	}
}
