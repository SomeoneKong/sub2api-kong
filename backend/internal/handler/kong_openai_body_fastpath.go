package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// kongCodexPageContextMayApply 为假时 normalizeCodexPageContext 必然不改写：它只改写 type 为 function_call_output、
// namespace 为 "agent" 的项，input 里没有这样的项就原样返回。候选投影与两道 bootstrap 共用（登记过的请求体只算一次），
// 开关同为 bootstrap 一项。原函数自己的字面量预判遇到任何 \u 转义都放行，随后查重键并完整解码三层；codex 的工具输出
// 里常有控制字符的转义，普通请求大多会走到那一步。见 DESIGN-openai-request-body-fastpath.md §3.1。
func kongCodexPageContextMayApply(body []byte) bool {
	return service.KongCodexCallOutputBootstrapMayApply(body, kongCodexPageContextCandidate)
}

func kongCodexPageContextCandidate(item map[string]any) bool {
	return item["namespace"] == "agent"
}
