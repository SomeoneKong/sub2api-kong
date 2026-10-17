//go:build unit

package repository

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const kongCompressionTestBody = "data: {\"type\":\"response.completed\"}\n\n"

func kongGzip(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// kongAcceptEncodingServer 记录每个请求的 Accept-Encoding 与协议版本。alwaysGzip 为真时不论请求声明什么都回
// gzip，否则只在请求声明了 gzip 时回 gzip。
type kongAcceptEncodingServer struct {
	mu     sync.Mutex
	seen   []string
	protos []int
}

func (s *kongAcceptEncodingServer) handler(t *testing.T, alwaysGzip bool) http.Handler {
	gz := kongGzip(t, kongCompressionTestBody)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Accept-Encoding")
		s.mu.Lock()
		s.seen = append(s.seen, strings.Join(values, ","))
		s.protos = append(s.protos, r.ProtoMajor)
		s.mu.Unlock()
		if alwaysGzip || strings.Contains(strings.Join(values, ","), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz)
			return
		}
		_, _ = io.WriteString(w, kongCompressionTestBody)
	})
}

func (s *kongAcceptEncodingServer) last(t *testing.T) (string, int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.seen)
	return s.seen[len(s.seen)-1], s.protos[len(s.protos)-1]
}

func kongCompressionTestConfig() *config.Config {
	return &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowPrivateHosts: true}}}
}

func kongDoWithProfile(t *testing.T, up service.HTTPUpstream, url string, profile service.HTTPUpstreamProfile) (*http.Response, string) {
	t.Helper()
	ctx := t.Context()
	if profile != service.HTTPUpstreamProfileDefault {
		ctx = service.WithHTTPUpstreamProfile(ctx, profile)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := up.Do(req, "", 1, 1)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

func TestKongOpenAIProfileOmitsAcceptEncoding(t *testing.T) {
	rec := &kongAcceptEncodingServer{}
	srv := httptest.NewServer(rec.handler(t, false))
	t.Cleanup(srv.Close)
	up := NewHTTPUpstream(kongCompressionTestConfig())

	resp, body := kongDoWithProfile(t, up, srv.URL, service.HTTPUpstreamProfileOpenAI)
	ae, _ := rec.last(t)
	require.Empty(t, ae, "OpenAI profile 不应带 Accept-Encoding")
	require.Equal(t, kongCompressionTestBody, body)
	require.False(t, resp.Uncompressed)

	resp, body = kongDoWithProfile(t, up, srv.URL, service.HTTPUpstreamProfileDefault)
	ae, _ = rec.last(t)
	require.Equal(t, "gzip", ae, "默认 profile 保持 Go 的自动 gzip")
	require.Equal(t, kongCompressionTestBody, body)
	require.True(t, resp.Uncompressed, "默认 profile 照旧透明解压")
}

func TestKongOpenAIProfileDecodesUnsolicitedGzip(t *testing.T) {
	rec := &kongAcceptEncodingServer{}
	srv := httptest.NewServer(rec.handler(t, true))
	t.Cleanup(srv.Close)
	up := NewHTTPUpstream(kongCompressionTestConfig())

	resp, body := kongDoWithProfile(t, up, srv.URL, service.HTTPUpstreamProfileOpenAI)
	ae, _ := rec.last(t)
	require.Empty(t, ae)
	require.Equal(t, kongCompressionTestBody, body, "上游主动回 gzip 时由 decompressResponseBody 解开")
	require.Empty(t, resp.Header.Get("Content-Encoding"))
}

func TestKongOpenAIProtocolModesOmitAcceptEncoding(t *testing.T) {
	rec := &kongAcceptEncodingServer{}
	srv := httptest.NewUnstartedServer(rec.handler(t, false))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	cases := []struct {
		mode  string
		proto int
	}{
		{upstreamProtocolModeOpenAIH2, 2},
		{upstreamProtocolModeOpenAIH1, 1},
		{upstreamProtocolModeOpenAIH1Fallback, 1},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			transport, err := buildUpstreamTransport(poolSettings{kongDisableCompression: true}, nil, tc.mode)
			require.NoError(t, err)
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{}
			}
			transport.TLSClientConfig.RootCAs = roots
			t.Cleanup(transport.CloseIdleConnections)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
			require.NoError(t, err)
			resp, err := (&http.Client{Transport: transport}).Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			ae, proto := rec.last(t)
			require.Equal(t, tc.proto, proto)
			require.Empty(t, ae)
			require.Equal(t, kongCompressionTestBody, string(body))
		})
	}
}

func TestKongProfileTransportsCompressionSetting(t *testing.T) {
	svc, ok := NewHTTPUpstream(kongCompressionTestConfig()).(*httpUpstreamService)
	require.True(t, ok)

	for _, tc := range []struct {
		profile service.HTTPUpstreamProfile
		disable bool
	}{
		{service.HTTPUpstreamProfileOpenAI, true},
		{service.HTTPUpstreamProfileDefault, false},
		{service.HTTPUpstreamProfileGrok, false},
		{service.HTTPUpstreamProfileLongStream, false},
	} {
		entry, err := svc.getClientEntry("", 7, 1, tc.profile, false, false)
		require.NoError(t, err)
		transport, ok := entry.client.Transport.(*http.Transport)
		require.True(t, ok)
		require.Equal(t, tc.disable, transport.DisableCompression, "profile %q", tc.profile)
	}
}

// TLS 指纹路径的缓存键不含 profile：同账号同代理交替发 OpenAI 与默认 profile 的请求，两个连接池应当并存，
// 第三次复用第一次的客户端，而不是互相替换。
func TestKongTLSFingerprintPoolsCoexistAcrossCompression(t *testing.T) {
	svc, ok := NewHTTPUpstream(kongCompressionTestConfig()).(*httpUpstreamService)
	require.True(t, ok)
	fp := &tlsfingerprint.Profile{Name: "kong-compression-test"}

	first, err := svc.getClientEntryWithTLS("", 9, 1, fp, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	second, err := svc.getClientEntryWithTLS("", 9, 1, fp, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	third, err := svc.getClientEntryWithTLS("", 9, 1, fp, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)

	require.Same(t, first, third, "OpenAI profile 的 TLS 客户端应被复用")
	require.NotSame(t, first, second)
	require.Len(t, svc.clients, 2)
	require.True(t, first.client.Transport.(*http.Transport).DisableCompression)
	require.False(t, second.client.Transport.(*http.Transport).DisableCompression)
}
