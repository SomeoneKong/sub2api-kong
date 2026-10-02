package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// registerKongPlanRoutes 注册账号计划执行端用到的管理端点（fork 专有，设计见
// DESIGN-openai-plan-reset-by-expiry.md 与 DESIGN-openai-plan-dispatch.md）。
func registerKongPlanRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.POST("/openai/accounts/:id/kong-reset-quota", h.Admin.OpenAIOAuth.KongResetQuotaByExpiry)

	plan := admin.Group("/kong-plan")
	plan.PUT("/dispatch", h.Admin.KongPlan.PutDispatch)
	plan.GET("/dispatch", h.Admin.KongPlan.GetDispatch)
	plan.GET("/publish", h.Admin.KongPlan.GetPublish)
	plan.PUT("/publish", h.Admin.KongPlan.PutPublish)
	plan.GET("/control", h.Admin.KongPlan.GetControl)
	plan.PUT("/control", h.Admin.KongPlan.PutControl)
	plan.POST("/control/hold", h.Admin.KongPlan.Hold)
	plan.POST("/control/release", h.Admin.KongPlan.Release)
	plan.PUT("/forecast", h.Admin.KongPlan.PutForecast)
	plan.GET("/caller", h.Admin.KongPlan.GetCaller)
	plan.GET("/accounts", h.Admin.KongPlan.Accounts)
	plan.GET("/stats", h.Admin.KongPlan.GetStats)
}
