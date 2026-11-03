package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/gin-gonic/gin"
)

// KongDashboardAPIKeyProjects 返回当前用户在所选范围内各 API key 按模型、档位、小时的用量，供 API Key 项目统计页。
// 查询参数与 /usage/stats 相同，只统计当前用户自己的请求。不传 api_key_id 时不校验 key，已删除的 key 也能列出。
// GET /api/v1/usage/dashboard/kong-api-key-projects
func (h *UsageHandler) KongDashboardAPIKeyProjects(c *gin.Context) {
	parsed, ok := h.parseUserUsageFilters(c, true)
	if !ok {
		return
	}

	stats, err := h.usageService.KongGetAPIKeyProjectStats(c.Request.Context(), parsed.Filters, kongUsageBucketTimezone(c.Query("timezone")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, stats)
}

// kongUsageBucketTimezone 返回划分时间桶的时区，规则与 timezone.ParseInUserLocation 解释日期范围时相同，
// 这样按天汇总的"每天"就是所选的日期。Local 是 Go 的特殊名字，PostgreSQL 不认识，按缺失处理。
func kongUsageBucketTimezone(userTZ string) string {
	if userTZ != "" && userTZ != "Local" {
		if _, err := time.LoadLocation(userTZ); err == nil {
			return userTZ
		}
	}
	return timezone.Name()
}
