package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// registerKongDowngradeGuardRoutes 注册降档哨兵的管理端点（fork 专有，设计见 DESIGN-account-downgrade-guard.md）。
func registerKongDowngradeGuardRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.GET("/kong/downgrade-guard", h.Admin.KongDowngradeGuard.Status)
	admin.POST("/accounts/:id/kong-downgrade-probe", h.Admin.KongDowngradeGuard.Probe)
}
