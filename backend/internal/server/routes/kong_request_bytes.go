package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// kongCountRequestBytes 在请求体限制中间件外面挂上逐请求字节数的计数容器（fork 专有，设计见
// DESIGN-openai-request-bytes.md §3.1）。
//
// 计数 reader 包在限制中间件的 MaxBytesReader 里面：分组链上的中间件（模型白名单、重复字段检查、组合路由）预读、
// handler 读、zstd 分块读，数到的都是从客户端连接上读进来的原始字节，MaxBytesError 原样上抛。网关路由上每个请求只
// 经过 bodyLimit 与 textBodyLimit 之一（分组层按路由选），幂等是兜底：context 上已有容器就只跑限制中间件，容器只建
// 一次、请求体只包一层。
func kongCountRequestBytes(limit gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service.KongRequestBytesFromContext(c.Request.Context()) == nil {
			rb := service.NewKongRequestBytes()
			c.Request = c.Request.WithContext(service.WithKongRequestBytes(c.Request.Context(), rb))
			if c.Request.Body != nil && c.Request.Body != http.NoBody {
				c.Request.Body = rb.CountInbound(c.Request.Body)
			}
		}
		limit(c)
	}
}
