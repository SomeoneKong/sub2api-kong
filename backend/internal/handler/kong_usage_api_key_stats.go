package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// KongDashboardAPIKeyStats 返回当前用户在所选范围内按 API key 汇总的用量，供用量页「API Key 使用分布」。
// 查询参数与 /usage/stats 相同，只统计当前用户自己的请求。
// GET /api/v1/usage/dashboard/kong-api-key-stats
func (h *UsageHandler) KongDashboardAPIKeyStats(c *gin.Context) {
	parsed, ok := h.parseUserUsageFilters(c, true)
	if !ok {
		return
	}

	stats, err := h.usageService.KongGetAPIKeyStatsWithFilters(c.Request.Context(), parsed.Filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, gin.H{"api_keys": stats})
}
