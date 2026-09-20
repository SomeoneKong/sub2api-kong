package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Codex 票据的管理端点。设计见仓库根 DESIGN-codex-ticket.md §6.4。
//
// 这些端点自给自足，不复用上游的 accounts API——那个响应结构上游每月改动多次，依赖它等于把
// 本功能绑在一个高频改动面上。

// KongTicketHandler 提供票据配置、状态与事件的管理接口。
type KongTicketHandler struct {
	svc *service.KongTicketAdminService
}

// NewKongTicketHandler 创建 handler。
//
// svc 为 nil 表示整个功能没有装配（不是「未启用」——未启用时 Admin 仍在），此时端点返回 503，
// 这样也不会 panic。
func NewKongTicketHandler(svc *service.KongTicketAdminService) *KongTicketHandler {
	return &KongTicketHandler{svc: svc}
}

func (h *KongTicketHandler) ready(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex ticket service not available")
		return false
	}
	return true
}

// GetOverview 返回各账号的票据配置与当前状态。
func (h *KongTicketHandler) GetOverview(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	now := time.Now()
	accounts, err := h.svc.Overview(c.Request.Context(), now)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	params := h.svc.Params()
	response.Success(c, gin.H{
		"accounts": accounts,
		// enabled 让界面区分「未启用」与「服务故障」，也决定配置能否编辑。
		"enabled":      h.svc.Enabled(),
		"gated_models": h.svc.GatedModels(),
		"params": gin.H{
			"refresh_before_seconds":         int64(params.RefreshBefore.Seconds()),
			"ticket_fetch_min_idle_seconds":  int64(params.TicketFetchMinIdle.Seconds()),
			"verify_fail_cooldown_seconds":   int64(params.VerifyFailCooldown.Seconds()),
			"min_ticket_age_seconds":         int64(params.MinTicketAge.Seconds()),
			"observe_probe_interval_seconds": int64(params.ObserveProbeInterval.Seconds()),
		},
		// 界面必须按这个口径显示空闲值：它是距本系统最后一次使用该出口的时长，不是实际静默。
		"idle_seconds_caveat": "距本系统最后一次使用该出口的时长；系统外的活动观测不到，该值会高估真实静默",
	})
}

// kongTicketConfigRequest 是改配置的请求体。
//
// 票据出口用一个显式枚举而不是「留空即直连」：「没配出口」与「出口就是直连」是两种不同的
// 行为，前者不主动取票，后者用服务器本机 IP 取票。也刻意不沿用上游 proxy_id 那个「提交 0
// 表示清除」的约定。
type kongTicketConfigRequest struct {
	Mode    string `json:"mode"`
	Egress  string `json:"egress"`
	ProxyID *int64 `json:"proxy_id"`
}

// UpdateAccountConfig 改一个账号的票据配置。
func (h *KongTicketHandler) UpdateAccountConfig(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	var req kongTicketConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body: "+err.Error())
		return
	}
	cfg := service.KongTicketConfig{
		Mode:    service.KongTicketMode(strings.TrimSpace(req.Mode)),
		Egress:  service.KongTicketEgress(strings.TrimSpace(req.Egress)),
		ProxyID: req.ProxyID,
	}
	status, err := h.svc.UpdateConfig(c.Request.Context(), accountID, cfg)
	if err != nil {
		// 配置类错误是调用方能改的，按 400 回；区分不出来时也不该当成 500。
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, status)
}

type kongTicketRefreshRequest struct {
	Model string `json:"model" binding:"required"`
}

// TriggerRefresh 手工触发一次取票/验票。
//
// 同步返回：调用方是页面上的一次点击，异步触发拿不到结论，等于让人对着页面猜。整条路径受服务端
// 的任务预算约束，最坏情况是取票 + 三份挑战。
func (h *KongTicketHandler) TriggerRefresh(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	var req kongTicketRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body: "+err.Error())
		return
	}
	result, status, err := h.svc.TriggerRefresh(c.Request.Context(), accountID, strings.TrimSpace(req.Model))
	if err != nil {
		// 未启用、模型不在门控集合、账号不存在都是调用方能改的，按 400 回。
		response.BadRequest(c, err.Error())
		return
	}
	// grant 里 Allowed 为假时**不是**错误：静默未满、出口不可用、模式不是 full 都是正常结论，
	// 页面要靠 deny_reason 把原因显示出来。当成错误回会让人以为触发本身失败了。
	response.Success(c, gin.H{"result": result, "status": status})
}

// ListEvents 分页查事件。
func (h *KongTicketHandler) ListEvents(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	filter := &service.KongTicketEventFilter{
		Models:     kongTicketQueryList(c, "model"),
		EventTypes: kongTicketQueryList(c, "event_type"),
	}
	for _, raw := range kongTicketQueryList(c, "account_id") {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			response.BadRequest(c, "invalid account_id")
			return
		}
		filter.AccountIDs = append(filter.AccountIDs, id)
	}
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "invalid since (expect RFC3339)")
			return
		}
		filter.Since = &t
	}
	if raw := strings.TrimSpace(c.Query("until")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "invalid until (expect RFC3339)")
			return
		}
		filter.Until = &t
	}
	// 与仓储共用同一个规范化：回给调用方的 limit 必须就是实际生效的那个。
	filter.Limit = service.KongNormalizeEventLimit(kongTicketQueryInt(c, "limit", 100))
	filter.Offset = kongTicketQueryInt(c, "offset", 0)

	events, total, err := h.svc.ListEvents(c.Request.Context(), filter)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response.Success(c, gin.H{
		"items":  events,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// ListProbes 返回一次验证的探测明细。
//
// 原始数字序列不在这里返回：几百个数字在页面上看不出任何东西，它的用途是离线重算，走 SQL
// 或导出即可。
func (h *KongTicketHandler) ListProbes(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	verificationID := strings.TrimSpace(c.Param("verification_id"))
	if verificationID == "" {
		response.BadRequest(c, "missing verification_id")
		return
	}
	probes, err := h.svc.ListProbes(c.Request.Context(), verificationID)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]gin.H, 0, len(probes))
	for _, p := range probes {
		item := gin.H{
			"id":                 p.ID,
			"created_at":         p.CreatedAt,
			"part_index":         p.PartIndex,
			"account_id":         p.AccountID,
			"target_model":       p.TargetModel,
			"ticket_source":      p.TicketSource,
			"verify_egress":      p.VerifyEgress,
			"challenge_id":       p.ChallengeID,
			"digit_count":        p.DigitCount,
			"part_attribution":   p.PartAttribution,
			"cum_probability":    p.CumProbability,
			"temperature_tier":   p.TemperatureTier,
			"library_version":    p.LibraryVersion,
			"parse_valid":        p.ParseValid,
			"counted_in_average": p.CountedInAverage,
			"invalid_reason":     p.InvalidReason,
			"latency_ms":         p.LatencyMs,
			"output_tokens":      p.OutputTokens,
		}
		items = append(items, item)
	}
	response.Success(c, gin.H{"items": items})
}

// kongTicketQueryList 读一个多值查询条件。同名参数重复出现与逗号分隔两种写法都接受——
// 前端用逗号拼，重复参数是 curl 排查时更顺手的写法；空白项一律丢掉，所以「显式传空」等于不过滤。
func kongTicketQueryList(c *gin.Context, key string) []string {
	var out []string
	for _, raw := range c.QueryArray(key) {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func kongTicketQueryInt(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}
