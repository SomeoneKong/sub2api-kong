package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// registerKongFingerprintRoutes 注册账号指纹测试的管理端点（fork 专有，设计见 DESIGN-openai-fingerprint-test.md）。
func registerKongFingerprintRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.GET("/kong/fingerprint/targets", h.Admin.KongFingerprint.Targets)
	admin.POST("/accounts/:id/kong-fingerprint-test", h.Admin.KongFingerprint.Test)
}
