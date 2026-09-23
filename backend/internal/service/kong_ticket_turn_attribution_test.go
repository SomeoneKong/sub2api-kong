//go:build unit

package service

import "testing"

// 票据上下文的释放只能靠**正面归属**。
//
// 把任意 error 都当成本轮结束，会让这条时序把保护整个摘掉：上一轮完成 → 客户端对上一轮发
// response.cancel → 新一轮（门控）准入并注入合格票 → 取消帧的错误回包到达、被当成「本轮结束」
// 清掉**新一轮**的上下文 → 上游为新一轮重发 state 时守卫看到 nil 而放行。
func TestKongWSTurnEventAction(t *testing.T) {
	protected := func(responseID string) *KongUpstreamAttempt {
		return &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}, ResponseID: responseID}
	}
	cases := []struct {
		name      string
		attempt   *KongUpstreamAttempt
		eventType string
		eventID   string
		released  map[string]bool
		want      kongWSTurnAction
	}{
		{"首个带 id 的事件绑定本轮", protected(""), "response.created", "resp_1", nil, kongWSTurnBind},
		{"已绑定后不再重复绑定", protected("resp_1"), "response.metadata", "resp_1", nil, kongWSTurnKeep},
		{"本轮终端释放", protected("resp_1"), "response.completed", "resp_1", nil, kongWSTurnClear},
		{"本轮 error 释放", protected("resp_1"), "error", "resp_1", nil, kongWSTurnClear},
		// 关键一条：别的一轮的错误回包不得释放本轮。
		{"别轮 error 不释放", protected("resp_2"), "error", "resp_1", nil, kongWSTurnKeep},
		{"别轮终端不释放", protected("resp_2"), "response.completed", "resp_1", nil, kongWSTurnKeep},
		// 新一轮还没绑 id 时，一个无归属信息的 error 同样不能释放它。
		{"未绑定 + 无 id 的 error 不释放", protected(""), "error", "", nil, kongWSTurnKeep},
		// 无归属信息的**终端类型**可以释放：那类事件只出现在一轮生成的末尾。
		{"未绑定 + 无 id 的终端释放", protected(""), "response.completed", "", nil, kongWSTurnClear},
		{"未绑定 + 带 id 的 error 不释放", protected(""), "error", "resp_1", nil, kongWSTurnKeep},
		// 正常时序：metadata 不带 id、completed 首次带 id。必须能释放，否则下一个非门控轮被无谓拒掉。
		{"未绑定 + 终端首次给 id 即释放", protected(""), "response.completed", "resp_1", nil, kongWSTurnClear},
		// 已释放过的那一轮的迟到/重复终端，不得再影响当前轮。
		{"已释放 id 的重复终端不动上下文", protected(""), "response.completed", "resp_1", map[string]bool{"resp_1": true}, kongWSTurnKeep},
		{"已释放 id 的迟到 error 不动上下文", protected("resp_2"), "error", "resp_1", map[string]bool{"resp_1": true}, kongWSTurnKeep},
		{"内容分片不动上下文", protected("resp_1"), "response.output_text.delta", "resp_1", nil, kongWSTurnKeep},
		{"无上下文时什么都不做", nil, "response.completed", "resp_1", nil, kongWSTurnKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kongWSTurnEventAction(tc.attempt, tc.eventType, tc.eventID, tc.released); got != tc.want {
				t.Errorf("kongWSTurnEventAction = %v, want %v", got, tc.want)
			}
		})
	}

	// 响应 id 只认 response.id 与顶层 response_id，**不能**退回顶层 id——后者常常是条目 id，
	// 拿它绑定会把本轮钉在一个与终端永不相符的值上，于是永不释放。
	idCases := []struct{ payload, want string }{
		{`{"type":"response.completed","response":{"id":"resp_1"}}`, "resp_1"},
		{`{"type":"response.metadata","response_id":"resp_2"}`, "resp_2"},
		{`{"type":"response.output_item.added","id":"item_9"}`, ""},
		{`{"type":"response.created"}`, ""},
	}
	for _, tc := range idCases {
		if got := kongWSResponseIDOf([]byte(tc.payload)); got != tc.want {
			t.Errorf("kongWSResponseIDOf(%s) = %q, want %q", tc.payload, got, tc.want)
		}
	}
}

// 交接判定：上一轮受保护且未落定时，不允许用一个不带保障的新轮覆盖它。
func TestKongGuardWSTurnHandoff(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect))
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})

	protected := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}}
	unprotected := &KongUpstreamAttempt{Model: "gpt-5.6-sol"}

	if err := g.GuardWSTurnHandoff(nil, unprotected); err != nil {
		t.Errorf("上一轮不存在时应当放行：%v", err)
	}
	if err := g.GuardWSTurnHandoff(unprotected, unprotected); err != nil {
		t.Errorf("上一轮未受保护时覆盖无风险：%v", err)
	}
	if err := g.GuardWSTurnHandoff(protected, nil); !KongIsTicketDenied(err) {
		t.Errorf("用 nil 覆盖未落定的受保护轮必须拒服，得到 %v", err)
	}
	if err := g.GuardWSTurnHandoff(protected, unprotected); !KongIsTicketDenied(err) {
		t.Errorf("用不带保障的新轮覆盖未落定的受保护轮必须拒服，得到 %v", err)
	}
	// 新一轮自带合格票时允许覆盖：判定落到新票身上仍是收紧方向。
	if err := g.GuardWSTurnHandoff(protected, protected); err != nil {
		t.Errorf("新轮自带保障时应当放行：%v", err)
	}
}
