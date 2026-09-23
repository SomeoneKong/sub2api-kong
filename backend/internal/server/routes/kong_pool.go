package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// RegisterKongPoolRoutes 注册调用方用的池子接口（fork 专有，设计见 DESIGN-openai-plan-dispatch.md）：
// 任何有效的用户 API key 可用。
func RegisterKongPoolRoutes(v1 *gin.RouterGroup, h *handler.Handlers, apiKeyAuth middleware.APIKeyAuthMiddleware) {
	pool := v1.Group("/pool")
	pool.Use(gin.HandlerFunc(apiKeyAuth))
	pool.PUT("/caller", h.Admin.KongPlan.PutPoolCaller)
	pool.GET("/capacity", h.Admin.KongPlan.GetPoolCapacity)
}
