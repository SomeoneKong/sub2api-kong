package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 账号指纹测试的管理端点（fork 专有，设计见 DESIGN-openai-fingerprint-test.md）。

// kongFingerprintKeepAlive 是 SSE 保活的间隔：一份挑战可以跑一两分钟而没有任何进度事件，保活让反向代理
// 的空闲超时不至于在那期间断开连接。
const kongFingerprintKeepAlive = 15 * time.Second

// KongFingerprintHandler 处理账号指纹测试。tester 为 nil（指纹库不可用）时端点返回 503。
type KongFingerprintHandler struct {
	tester *service.KongFingerprintTester
}

// NewKongFingerprintHandler 创建处理器。
func NewKongFingerprintHandler(tester *service.KongFingerprintTester) *KongFingerprintHandler {
	return &KongFingerprintHandler{tester: tester}
}

// Targets 列出可选的目标模型。
// GET /api/v1/admin/kong/fingerprint/targets
func (h *KongFingerprintHandler) Targets(c *gin.Context) {
	if h == nil || h.tester == nil {
		response.Error(c, http.StatusServiceUnavailable, "指纹库不可用")
		return
	}
	response.Success(c, gin.H{"targets": h.tester.Targets()})
}

type kongFingerprintTestRequest struct {
	Model string `json:"model"`
}

// Test 对一个账号发起指纹测试，以 SSE 推送进度与结论。
// POST /api/v1/admin/accounts/:id/kong-fingerprint-test
func (h *KongFingerprintHandler) Test(c *gin.Context) {
	if h == nil || h.tester == nil {
		response.Error(c, http.StatusServiceUnavailable, "指纹库不可用")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req kongFingerprintTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	run, err := h.tester.Begin(c.Request.Context(), accountID, req.Model)
	switch {
	case errors.Is(err, service.ErrKongFingerprintTarget):
		response.BadRequest(c, err.Error())
		return
	case errors.Is(err, service.ErrKongFingerprintAccount):
		response.Error(c, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, service.ErrKongFingerprintBusy):
		response.Error(c, http.StatusConflict, err.Error())
		return
	case err != nil:
		response.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// 事件由执行协程产出、在这里统一写出：保活与事件共用一个写者，不会交错。管理员断开后写会失败，
	// 照旧读完事件——执行协程看到 ctx 取消会很快收尾，读完才不会让它卡在发送上。
	events := make(chan service.KongFingerprintTestEvent, 8)
	go func() {
		defer close(events)
		run.Execute(c.Request.Context(), func(e service.KongFingerprintTestEvent) { events <- e })
	}()
	keepAlive := time.NewTicker(kongFingerprintKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", payload); err == nil {
				c.Writer.Flush()
			}
		case <-keepAlive.C:
			if _, err := fmt.Fprint(c.Writer, ": keepalive\n\n"); err == nil {
				c.Writer.Flush()
			}
		}
	}
}
