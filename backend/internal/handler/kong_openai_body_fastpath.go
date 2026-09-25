package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// kongCodexUntrustedContextMayApply 为假时 normalizeCodexUntrustedContext 必然不改写：它只改写 type 为
// function_call_output、namespace 为 "agent" 的项，input 里没有这样的项就原样返回。接在原函数的零拷贝预判之后：
// 预判只按工具名走一遍 input、不复制输出，通过了才用候选投影（与两道 bootstrap 共用，登记过的请求体只算一次）确认
// namespace，再交给查重键与完整解码。开关同为 bootstrap 一项。见 DESIGN-openai-request-body-fastpath.md §3.1。
func kongCodexUntrustedContextMayApply(body []byte) bool {
	return service.KongCodexCallOutputBootstrapMayApply(body, kongCodexUntrustedContextCandidate)
}

func kongCodexUntrustedContextCandidate(item map[string]any) bool {
	return item["namespace"] == "agent"
}
