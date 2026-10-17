//go:build unit

package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 逐请求字节数的 down 在 decompressResponseBody 之前计：上游主动回 gzip 时记的是线上的压缩字节，业务拿到的
// 仍是解压后的正文。
func TestKongUpstreamDownCountedBeforeDecompression(t *testing.T) {
	rec := &kongAcceptEncodingServer{}
	srv := httptest.NewServer(rec.handler(t, true))
	t.Cleanup(srv.Close)
	up := NewHTTPUpstream(kongCompressionTestConfig())
	wireLen := len(kongGzip(t, kongCompressionTestBody))

	var down atomic.Int64
	ctx := service.KongWithUpstreamDownCounter(service.WithHTTPUpstreamProfile(t.Context(), service.HTTPUpstreamProfileOpenAI), &down)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := up.Do(req, "", 1, 1)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, kongCompressionTestBody, string(body))
	require.Equal(t, int64(wireLen), down.Load())
	require.NotEqual(t, int64(len(kongCompressionTestBody)), down.Load())
}
