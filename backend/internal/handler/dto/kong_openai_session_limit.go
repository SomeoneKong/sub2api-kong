package dto

import "github.com/Wei-Shaw/sub2api/internal/service"

// kongExposeOpenAISessionLimit 为 OpenAI OAuth 账号导出会话数上限与空闲超时，供编辑框与容量列显示。
// 上游只对 Anthropic OAuth/SetupToken 账号导出这两个字段；本 fork 的 OpenAI 会话上限用的是同两个 extra 键
// （设计见 DESIGN-openai-session-limit.md）。
func kongExposeOpenAISessionLimit(out *Account, a *service.Account) {
	if out == nil || a == nil || !a.IsOpenAIOAuth() {
		return
	}
	if maxSessions := a.GetMaxSessions(); maxSessions > 0 {
		out.MaxSessions = &maxSessions
		idleTimeout := a.GetSessionIdleTimeoutMinutes()
		out.SessionIdleTimeoutMin = &idleTimeout
	}
}
