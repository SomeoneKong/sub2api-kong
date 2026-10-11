//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
)

// 哨兵未启用（配置不合法、指纹库不可用或模式为 off）时两个端点都已注册，且返回 503。
func TestKongDowngradeGuardRoutesWithoutGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{KongDowngradeGuard: admin.NewKongDowngradeGuardHandler(nil)}}
	registerKongDowngradeGuardRoutes(r.Group("/api/v1/admin"), h)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/kong/downgrade-guard"},
		{http.MethodPost, "/api/v1/admin/accounts/6/kong-downgrade-probe"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		require.Equal(t, http.StatusServiceUnavailable, w.Code, "%s %s", c.method, c.path)
	}
}
