package handler

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// kongHotWaitNotedKey 标记这个 HTTP 请求已经记过一次限速挤压（重试、换号再排队不重复记）。
const kongHotWaitNotedKey = "kong_hot_wait_noted"

// kongHotWaitOutcome 把一次等槽的结果换成限速挤压的计数项（fork 专有，见 service.KongHotNoteWait）：等到槽、等到超时
// （包括等待期限恰在抢槽的 Redis 调用中到期：请求本身还在、错误是期限到了）；客户端自己断开等其余情形不计（返回空串）。
func kongHotWaitOutcome(c *gin.Context, err error) string {
	if err == nil {
		return service.KongHotWaitAcquired
	}
	var ce *ConcurrencyError
	if errors.As(err, &ce) && ce.IsTimeout {
		return service.KongHotWaitTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) && c != nil && c.Request != nil && c.Request.Context().Err() == nil {
		return service.KongHotWaitTimeout
	}
	return ""
}

// kongHotNoteRequestWait 记一个 HTTP 请求的排队结果，同一请求只记第一次记上的那一次（冷账号上的排队不算记过）。
func kongHotNoteRequestWait(c *gin.Context, accountID int64, outcome string) {
	if c != nil {
		if _, done := c.Get(kongHotWaitNotedKey); done {
			return
		}
	}
	if service.KongHotNoteWait(accountID, outcome) && c != nil {
		c.Set(kongHotWaitNotedKey, true)
	}
}

// kongHotNoteWSRetryBusy 记 WS 同账号重试时抢不到槽（槽满才算，缓存出错不算）。
func kongHotNoteWSRetryBusy(accountID int64, acquireErr error) {
	if acquireErr == nil {
		service.KongHotNoteWait(accountID, service.KongHotWaitWSBusy)
	}
}
