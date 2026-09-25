package service

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

// codex 客户端的 x-reasoning-included 响应头。
//
// codex 判断是否自动压缩上下文时，用的是 API 回报的 total_tokens 加上本地估算的历史 encrypted
// reasoning（codex-rs context_manager/history.rs 的 get_total_token_usage）。只有响应带这个头时，
// 后一项才不计入。ChatGPT codex 后端的 HTTP 响应不带它，但回报的用量已经含 reasoning，结果
// reasoning 被算了两遍，会话远没到阈值就被压缩。
//
// 写入分两步。handler 在排队等槽之前先写 1：排队心跳与首输出前的 keepalive 都会在选定上游之前
// 提交响应头，晚了就写不进去。之后各 Responses 成功出口在头提交之前按本次上游响应重写一次：
// 上游给了就用上游的值，并把白名单转发来的那一份合成一份。
//
// codex 只在成功响应（2xx 流、101 握手）上读这个头，而且只看在不在，所以错误响应带上它没有影响；
// 这个头也不改变请求内容。

// KongCodexReasoningIncludedEnv 是开关：不设置为开，false 关闭，写错按关闭处理并记错误日志。
const KongCodexReasoningIncludedEnv = "KONG_CODEX_REASONING_INCLUDED"

const kongCodexReasoningIncludedHeader = "X-Reasoning-Included"

var kongCodexReasoningIncludedFromEnv = sync.OnceValue(func() bool {
	value, present := os.LookupEnv(KongCodexReasoningIncludedEnv)
	enabled, err := kongParseCodexReasoningIncluded(value, present)
	if err != nil {
		slog.Error("codex x-reasoning-included 注入已关闭", "error", err)
		return false
	}
	if enabled {
		slog.Info("codex x-reasoning-included 注入已开启")
	}
	return enabled
})

// kongCodexReasoningIncludedEnabled 取当前开关；测试替换它来固定开关。
var kongCodexReasoningIncludedEnabled = func() bool { return kongCodexReasoningIncludedFromEnv() }

func kongParseCodexReasoningIncluded(value string, present bool) (bool, error) {
	if !present {
		return true, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s 必须是 true / false，得到 %q", KongCodexReasoningIncludedEnv, value)
	}
	return enabled, nil
}

// KongApplyCodexReasoningIncluded 把 x-reasoning-included 写进发给客户端的响应头。upstream 是本次
// 上游的响应头，handler 在选定上游之前调用时传 nil。
//
// 用 Set 写入：白名单已经转发过的同一个头、之前写下的值都合成一份。头已经提交后再写不会上线，但也无害。
func KongApplyCodexReasoningIncluded(c *gin.Context, upstream http.Header) {
	if c == nil || c.Writer == nil {
		return
	}
	if value := kongCodexReasoningIncludedValue(c.Request, upstream, kongCodexReasoningIncludedEnabled()); value != "" {
		c.Writer.Header().Set(kongCodexReasoningIncludedHeader, value)
	}
}

// kongApplyCodexReasoningIncludedUnstaged 用于首输出暂存路径（handleStreamingResponseWithReasoning）。
// 这个头与账号无关，不随暂存头延迟提交，直接写 writer：首输出前的 keepalive 会先提交响应头，暂存的头
// 届时全部作废。暂存集合里白名单带进来的同一个头要移除，否则提交时逐项 Add 会再写一份。
func kongApplyCodexReasoningIncludedUnstaged(staged http.Header, c *gin.Context, upstream http.Header) {
	if c == nil || c.Writer == nil {
		return
	}
	value := kongCodexReasoningIncludedValue(c.Request, upstream, kongCodexReasoningIncludedEnabled())
	if value == "" {
		return
	}
	staged.Del(kongCodexReasoningIncludedHeader)
	c.Writer.Header().Set(kongCodexReasoningIncludedHeader, value)
}

// kongCodexReasoningIncludedValue 返回要写给客户端的值，空串表示不写。
func kongCodexReasoningIncludedValue(req *http.Request, upstream http.Header, enabled bool) string {
	if !enabled || req == nil {
		return ""
	}
	if !openai.IsCodexOfficialClientByHeaders(req.Header.Get("User-Agent"), req.Header.Get("originator")) {
		return ""
	}
	if value := strings.TrimSpace(upstream.Get(kongCodexReasoningIncludedHeader)); value != "" {
		return value
	}
	return "1"
}
