package admin

import (
	"net/http"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 账号选择与 credits 的接口（fork 专有，设计见 DESIGN-openai-plan-dispatch.md）：管理接口在
// /api/v1/admin/kong-plan/ 下，调用方接口在 /api/v1/pool/ 下。

// kongPlanMaxBody 是请求体的上限；一次发布按几十个账号算只有几十 KB。
const kongPlanMaxBody = 4 << 20

// kongPlanStatsMaxSpan 是决策记录一次最多查多长：与保留期相同。
const kongPlanStatsMaxSpan = 31 * 24 * time.Hour

// KongPlanHandler 处理计划组件的接口。store 为 nil 时一律返回 503。
type KongPlanHandler struct {
	store    *service.KongPlanStore
	accounts service.AccountRepository
	stats    *service.KongPlanStats
	gateway  *service.OpenAIGatewayService
}

// NewKongPlanHandler 创建处理器。accounts 供 GET /accounts 列出账号；stats 是决策记录；gateway 算容量视图。
func NewKongPlanHandler(store *service.KongPlanStore, accounts service.AccountRepository, stats *service.KongPlanStats,
	gateway *service.OpenAIGatewayService) *KongPlanHandler {
	return &KongPlanHandler{store: store, accounts: accounts, stats: stats, gateway: gateway}
}

func (h *KongPlanHandler) ready(c *gin.Context) bool {
	if h == nil || h.store == nil {
		response.Error(c, http.StatusServiceUnavailable, "计划组件未启用")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, kongPlanMaxBody)
	return true
}

func kongPlanRespond(c *gin.Context, data any, err error) {
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, data)
}

// PutDispatch 接受边车的一次发布。
// PUT /api/v1/admin/kong-plan/dispatch
func (h *KongPlanHandler) PutDispatch(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanDispatch(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.PutDispatch(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// GetDispatch 返回最近一次接受的发布。
// GET /api/v1/admin/kong-plan/dispatch
func (h *KongPlanHandler) GetDispatch(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	data, err := h.store.GetDispatch(c.Request.Context())
	kongPlanRespond(c, data, err)
}

// GetPublish 返回发布状态。
// GET /api/v1/admin/kong-plan/publish
func (h *KongPlanHandler) GetPublish(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	data, err := h.store.GetPublish(c.Request.Context())
	kongPlanRespond(c, data, err)
}

// PutPublish 停用、恢复或清空发布。
// PUT /api/v1/admin/kong-plan/publish
func (h *KongPlanHandler) PutPublish(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanPublish(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.PutPublish(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// GetControl 返回人工约束。
// GET /api/v1/admin/kong-plan/control
func (h *KongPlanHandler) GetControl(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	data, err := h.store.GetControl(c.Request.Context())
	kongPlanRespond(c, data, err)
}

// PutControl 按约束修订号同步人工约束。
// PUT /api/v1/admin/kong-plan/control
func (h *KongPlanHandler) PutControl(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanControl(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.PutControl(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// Hold 设置锁存暂停。
// POST /api/v1/admin/kong-plan/control/hold
func (h *KongPlanHandler) Hold(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanHold(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.Hold(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// Release 解除锁存暂停。
// POST /api/v1/admin/kong-plan/control/release
func (h *KongPlanHandler) Release(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanRelease(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.Release(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// PutForecast 写容量视图的慢速部分。
// PUT /api/v1/admin/kong-plan/forecast
func (h *KongPlanHandler) PutForecast(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPlanForecast(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	data, err := h.store.PutForecast(c.Request.Context(), req)
	kongPlanRespond(c, data, err)
}

// GetCaller 返回调用方声明，供边车读。
// GET /api/v1/admin/kong-plan/caller
func (h *KongPlanHandler) GetCaller(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	data, err := h.store.GetCaller(c.Request.Context())
	kongPlanRespond(c, data, err)
}

// Accounts 返回网关此刻对每个 OpenAI OAuth 账号的定层、credits 资格、窗口状态与未结算的入层。
// GET /api/v1/admin/kong-plan/accounts
func (h *KongPlanHandler) Accounts(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	data, err := service.KongPlanAccounts(c.Request.Context(), h.accounts)
	kongPlanRespond(c, data, err)
}

// GetStats 按小时返回 [from, to) 内的决策记录；缺省为最近 24 小时。
// GET /api/v1/admin/kong-plan/stats?from=&to=
func (h *KongPlanHandler) GetStats(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	to, err := kongPlanQueryTime(c, "to", time.Now())
	if response.ErrorFrom(c, err) {
		return
	}
	from, err := kongPlanQueryTime(c, "from", to.Add(-24*time.Hour))
	if response.ErrorFrom(c, err) {
		return
	}
	if !from.Before(to) || to.Sub(from) > kongPlanStatsMaxSpan {
		response.ErrorFrom(c, infraerrors.BadRequest(service.KongPlanReasonInvalid, "from 必须早于 to，且跨度不超过 31 天").
			WithMetadata(map[string]string{"field": "from"}))
		return
	}
	if h.stats == nil {
		kongPlanRespond(c, []service.KongPlanStatRow{}, nil)
		return
	}
	data, err := h.stats.List(c.Request.Context(), from, to)
	kongPlanRespond(c, data, err)
}

func kongPlanQueryTime(c *gin.Context, field string, def time.Time) (time.Time, error) {
	v := c.Query(field)
	if v == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, infraerrors.BadRequest(service.KongPlanReasonInvalid, field+" 必须是 RFC 3339 时刻").
			WithMetadata(map[string]string{"field": field})
	}
	return t, nil
}

// GetPoolCapacity 返回池子的容量视图：当前能力由网关现算，慢速部分取边车最近一次写入的那份。
// GET /api/v1/pool/capacity
func (h *KongPlanHandler) GetPoolCapacity(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	if h.gateway == nil {
		response.Error(c, http.StatusServiceUnavailable, "计划组件未启用")
		return
	}
	data, err := h.gateway.KongPoolCapacity(c.Request.Context())
	kongPlanRespond(c, data, err)
}

// PutPoolCaller 写入调用方声明，记下写入的 API key。
// PUT /api/v1/pool/caller
func (h *KongPlanHandler) PutPoolCaller(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	req, err := service.ParseKongPoolCaller(c.Request.Body)
	if response.ErrorFrom(c, err) {
		return
	}
	var apiKeyID *int64
	if key, ok := middleware.GetAPIKeyFromContext(c); ok && key != nil {
		id := key.ID
		apiKeyID = &id
	}
	data, err := h.store.PutCaller(c.Request.Context(), req, apiKeyID)
	kongPlanRespond(c, data, err)
}
