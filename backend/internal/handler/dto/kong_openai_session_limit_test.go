package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestKongExposeOpenAISessionLimit(t *testing.T) {
	limited := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{"max_sessions": float64(3), "session_idle_timeout_minutes": float64(15)}}
	out := AccountFromServiceShallow(limited)
	if out.MaxSessions == nil || *out.MaxSessions != 3 || out.SessionIdleTimeoutMin == nil || *out.SessionIdleTimeoutMin != 15 {
		t.Fatalf("OpenAI OAuth 账号要导出会话上限与空闲超时：max=%v idle=%v", out.MaxSessions, out.SessionIdleTimeoutMin)
	}

	unlimited := &service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	if out := AccountFromServiceShallow(unlimited); out.MaxSessions != nil || out.SessionIdleTimeoutMin != nil {
		t.Fatal("未设上限不导出")
	}

	apiKey := &service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Extra: map[string]any{"max_sessions": float64(3)}}
	if out := AccountFromServiceShallow(apiKey); out.MaxSessions != nil {
		t.Fatal("API Key 账号不导出")
	}
}
