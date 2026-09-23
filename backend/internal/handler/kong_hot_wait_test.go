//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestKongHotWaitOutcome(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	require.Equal(t, service.KongHotWaitAcquired, kongHotWaitOutcome(c, nil))
	require.Equal(t, service.KongHotWaitTimeout, kongHotWaitOutcome(c, &ConcurrencyError{IsTimeout: true}))
	require.Equal(t, service.KongHotWaitTimeout, kongHotWaitOutcome(c, context.DeadlineExceeded), "等待期限在抢槽的 Redis 调用中到期")
	require.Equal(t, "", kongHotWaitOutcome(c, errors.New("redis: connection refused")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	require.Equal(t, "", kongHotWaitOutcome(c, context.Canceled), "客户端断开不计")
	require.Equal(t, "", kongHotWaitOutcome(c, context.DeadlineExceeded), "请求本身已结束：不算等槽超时")
}
