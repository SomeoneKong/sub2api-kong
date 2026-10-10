package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongServeCodexCatalog 在调度器分支取版本存档的对外版本（DESIGN-codex-catalog-consensus.md）。
// served 为 true 表示响应已写出；否则返回给原调度循环用的回退句柄，nil 表示这个请求不纳管。
// 对外版本是独立副本，照原样经分组合并与窗口折算后写出。start 是请求进入 handler 的时刻。
func (h *OpenAIGatewayHandler) kongServeCodexCatalog(c *gin.Context, group *service.Group, ifNoneMatch string, start time.Time) (*service.KongCodexCatalogFallback, bool) {
	ctx := c.Request.Context()
	result := h.gatewayService.KongServeCodexCatalog(ctx, group, c.Query("client_version"), start)
	if result.Manifest == nil {
		return result.Fallback, false
	}
	if ctx.Err() != nil {
		return nil, true
	}
	if result.AccountID > 0 {
		setOpsSelectedAccount(c, result.AccountID, service.PlatformOpenAI)
	}
	if err := h.gatewayService.MergeGroupConfiguredCodexModels(ctx, group, result.Manifest, ""); err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to build Codex models manifest")
		return nil, true
	}
	if ctx.Err() != nil {
		return nil, true
	}
	service.KongFinalizeCodexModelsManifest(result.Manifest, ifNoneMatch)
	writeOpenAIModelsResponse(c, result.Manifest)
	return nil, true
}
