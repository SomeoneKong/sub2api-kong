package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 按到期时间指定重置卡的用卡端点（fork 专有，设计见 DESIGN-openai-plan-reset-by-expiry.md）。
// 与上游的 ResetQuota 走同一套用卡后处理（解除限流、刷新额度缓存、刷新账号行）。

type kongOpenAIQuotaResetByExpiry interface {
	KongResetCreditByExpiry(ctx context.Context, accountID int64, expiresAt, redeemRequestID string) (*service.OpenAIQuotaResetResult, error)
}

type kongOpenAIResetQuotaByExpiryRequest struct {
	ExpiresAt       string `json:"expires_at"`
	RedeemRequestID string `json:"redeem_request_id"`
}

// KongResetQuotaByExpiry 消耗到期时间为 expires_at 的那张重置卡。
// POST /api/v1/admin/openai/accounts/:id/kong-reset-quota
func (h *OpenAIOAuthHandler) KongResetQuotaByExpiry(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	svc, ok := h.quotaService.(kongOpenAIQuotaResetByExpiry)
	if h.quotaService == nil || !ok {
		response.BadRequest(c, "openai quota service is not enabled")
		return
	}
	var req kongOpenAIResetQuotaByExpiryRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.ExpiresAt == "" || req.RedeemRequestID == "" {
		response.BadRequest(c, "expires_at and redeem_request_id are required")
		return
	}
	result, err := svc.KongResetCreditByExpiry(c.Request.Context(), accountID, req.ExpiresAt, req.RedeemRequestID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result == nil {
		response.Error(c, http.StatusInternalServerError, "openai quota reset returned an empty result")
		return
	}

	resetResponse := openAIQuotaResetResponse{OpenAIQuotaResetResult: *result}
	postCtx, cancelPost := openAIQuotaResetPostProcessContext(c.Request.Context())
	defer cancelPost()
	postResult := service.RunOpenAIQuotaResetPostProcess(postCtx, accountID, h.quotaService, h.rateLimitService, h.adminService.GetAccount)
	resetResponse.Quota = postResult.Quota
	resetResponse.CacheRefreshed = postResult.CacheRefreshed
	resetResponse.AccountStateRecovered = postResult.AccountStateRecovered
	resetResponse.WarningCode = postResult.WarningCode
	if postResult.Account != nil {
		resetResponse.Account = dto.AccountFromService(postResult.Account)
	}
	response.Success(c, resetResponse)
}
