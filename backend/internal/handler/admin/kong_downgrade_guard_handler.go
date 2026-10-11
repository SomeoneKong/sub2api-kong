package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 降档哨兵的管理端点（fork 专有，设计见 DESIGN-account-downgrade-guard.md）。打开被哨兵关闭的账号用已有的"可调度"开关，
// 这里不提供放行接口。

// kongGuardManualProbeTimeout 是手动探测的期限：两个请求加 3 秒间隔，单个请求 30 秒封顶。
const kongGuardManualProbeTimeout = 80 * time.Second

// KongDowngradeGuardHandler 处理降档哨兵的接口。guard 为 nil（未启用）时一律返回 503。
type KongDowngradeGuardHandler struct {
	guard *service.KongDowngradeGuard
}

// NewKongDowngradeGuardHandler 创建处理器。
func NewKongDowngradeGuardHandler(guard *service.KongDowngradeGuard) *KongDowngradeGuardHandler {
	return &KongDowngradeGuardHandler{guard: guard}
}

// Status 返回生效的配置、当前的持有记录与最近的事件。
// GET /api/v1/admin/kong/downgrade-guard?account_id=&limit=
func (h *KongDowngradeGuardHandler) Status(c *gin.Context) {
	if h == nil || h.guard == nil {
		response.Error(c, http.StatusServiceUnavailable, "降档哨兵未启用")
		return
	}
	accountID, _ := strconv.ParseInt(c.Query("account_id"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	holds, err := h.guard.Holds(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := h.guard.ListEvents(c.Request.Context(), accountID, limit)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := h.guard.Config()
	response.Success(c, gin.H{
		"config": gin.H{
			"mode": cfg.Mode, "group_id": cfg.GroupID, "probe_model": cfg.ProbeModel,
			"fresh_seconds": int(cfg.FreshAge.Seconds()), "ticket_hold_max": cfg.TicketHoldMax,
			"probe_busy_minutes": int(cfg.ProbeBusy.Minutes()), "probe_idle_minutes": int(cfg.ProbeIdle.Minutes()),
		},
		"holds":  holds,
		"events": events,
	})
}

// Probe 对单个账号立即做一次主动探测，返回结论。结论照常交给哨兵判断；服务分组外的账号只记录。
// POST /api/v1/admin/accounts/:id/kong-downgrade-probe
func (h *KongDowngradeGuardHandler) Probe(c *gin.Context) {
	if h == nil || h.guard == nil {
		response.Error(c, http.StatusServiceUnavailable, "降档哨兵未启用")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), kongGuardManualProbeTimeout)
	defer cancel()
	out, err := h.guard.ManualProbe(ctx, accountID)
	switch {
	case errors.Is(err, service.ErrKongFingerprintAccount):
		response.BadRequest(c, err.Error())
		return
	case errors.Is(err, service.ErrKongGuardBusy):
		response.Error(c, http.StatusConflict, err.Error())
		return
	case err != nil:
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, out)
}
