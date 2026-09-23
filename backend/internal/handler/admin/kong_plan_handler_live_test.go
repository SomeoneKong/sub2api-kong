//go:build unit

package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKongPlanHandlerLiveReader(t *testing.T) {
	h := &KongPlanHandler{}
	require.Nil(t, h.liveReader(), "没装配网关服务时是 nil 接口，不是装着 nil 指针的接口")

	gw := &service.OpenAIGatewayService{}
	h = &KongPlanHandler{gateway: gw}
	require.Equal(t, service.KongPlanLiveReader(gw), h.liveReader())
}
