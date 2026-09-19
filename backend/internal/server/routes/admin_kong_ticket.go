package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

// registerKongTicketRoutes 注册 Codex 票据的管理端点。
//
// 端点自给自足，不复用上游的 accounts API：那个响应结构上游每月改动多次，依赖它等于把本功能
// 绑在一个高频改动面上。设计见仓库根 DESIGN-codex-ticket.md §6.4。
func registerKongTicketRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	group := admin.Group("/kong/ticket")
	{
		group.GET("/overview", h.Admin.KongTicket.GetOverview)
		group.PUT("/accounts/:id", h.Admin.KongTicket.UpdateAccountConfig)
		group.GET("/events", h.Admin.KongTicket.ListEvents)
		group.GET("/probes/:verification_id", h.Admin.KongTicket.ListProbes)
	}
}
