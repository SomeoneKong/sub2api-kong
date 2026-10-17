package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// kongCountRequestBytes 在请求体限制中间件外面挂上逐请求字节数的计数容器（fork 专有，设计见
// DESIGN-openai-request-bytes.md §3.1）。
//
// 计数 reader 包在限制中间件的 MaxBytesReader 里面：中间件预读、handler 读、zstd 分块读，数到的都是从客户端连接上
// 读进来的原始字节，MaxBytesError 原样上抛。同一请求可能先后经过 bodyLimit 与 textBodyLimit（alpha search），所以
// 必须幂等：context 上已有容器就只跑限制中间件，容器只建一次、请求体只包一层。
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
