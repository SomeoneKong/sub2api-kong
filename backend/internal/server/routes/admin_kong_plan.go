package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// registerKongPlanRoutes 注册账号计划执行端用到的管理端点（fork 专有，设计见 DESIGN-openai-plan-reset-by-expiry.md）。
func registerKongPlanRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.POST("/openai/accounts/:id/kong-reset-quota", h.Admin.OpenAIOAuth.KongResetQuotaByExpiry)
}
