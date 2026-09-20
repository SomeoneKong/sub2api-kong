package service

import "context"

// HTTP→WS 这条路径（forwardOpenAIWSV2）的接入。
//
// 它是第三条独立通路：客户端用 HTTP 进来（例如 /v1/messages 转 Responses），上游却是 WebSocket。
// 它既不经过 doOpenAIUpstream，也不经过 ctx_pool / passthrough 两条原生 WS 适配器——所以必须
// 自己接同一套判定，否则门控模型在这条路上完全没有保护。
//
// 与原生 WS 的差别只在载体：这里的 payload 是 `map[string]any`，由 encoding/json 序列化后
// WriteJSON 上送。map 天然没有重复键，所以不需要 GuardWSFrame 那套歧义判定；但注入仍要确认
// `client_metadata` 是个可写的对象，写不进去就拒服。

// PrepareWSMapPayload 在一次 HTTP→WS 发送之前做准入，并把票写进 payload 的 client_metadata。
//
// 直接原地改 payload：调用方随后把同一个 map 交给 WriteJSON（预热帧也用它）。
// 返回的 attempt 交给 GuardWSDownstream 做交付判定。
func (g *KongTicketGateway) PrepareWSMapPayload(ctx context.Context, account *Account, model string, payload map[string]any) (*KongUpstreamAttempt, error) {
	if account == nil || !g.IsGatedModel(model) {
		return nil, nil
	}
	if payload == nil {
		return nil, &KongErrTicketDenied{Reason: "payload_missing"}
	}
	// 原生 WS：不交接，理由同 PrepareWSTurn。
	grant, err := g.svc.EnsureTicketNoHandoff(ctx, account.ID, model)
	if err != nil {
		return nil, &KongErrTicketDenied{Reason: "ensure_failed: " + err.Error()}
	}
	if grant.NotApplicable {
		return &KongUpstreamAttempt{Model: model}, nil
	}
	if !grant.Allowed {
		denied := &KongErrTicketDenied{Reason: grant.DenyReason}
		if !grant.RetryAfter.IsZero() {
			denied.RetryAfter = grant.RetryAfter.UTC().Format("2006-01-02T15:04:05Z")
		}
		return nil, denied
	}
	meta, ok := payload["client_metadata"]
	if !ok || meta == nil {
		payload["client_metadata"] = map[string]any{kongWSTurnStateMetadataKey: grant.State}
		return &KongUpstreamAttempt{Model: model, Grant: grant}, nil
	}
	typed, ok := meta.(map[string]any)
	if !ok {
		// 已有的 client_metadata 不是对象，无法在不破坏它的前提下注入。放行等于这一轮不受保障。
		return nil, &KongErrTicketDenied{Reason: "client_metadata_not_object"}
	}
	typed[kongWSTurnStateMetadataKey] = grant.State
	return &KongUpstreamAttempt{Model: model, Grant: grant}, nil
}
