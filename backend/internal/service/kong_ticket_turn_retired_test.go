//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
)

// 带票的新一轮覆盖一个尚未结束的受保护轮之后，旧轮迟到的终端不得释放新一轮。
//
// 时序：A 已结束 → B 进行中且已绑上 id → 上游重发 A 的终端（通用生命周期据此放行下一轮）→ 带票的 C
// 准入并覆盖 B → B 的终端到达。C 此时还没绑上自己的 id，若按"未绑定时带 id 的终端即本轮终端"把它
// 释放，C 随后带 state 的 metadata 会被当成无保护而放行。
func TestKongWSRetiredTurnTerminalDoesNotReleaseNewTurn(t *testing.T) {
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, strings.Repeat("a", 780))
	released := map[string]bool{"resp_A": true}
	b := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}, ResponseID: "resp_B"}
	c := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}}

	if err := g.GuardWSTurnHandoff(b, c); err != nil {
		t.Fatalf("带票的新一轮应当允许覆盖：%v", err)
	}
	kongWSInheritRetired(b, c)

	if act := kongWSTurnEventAction(c, "response.completed", "resp_A", released); act != kongWSTurnKeep {
		t.Fatalf("已释放那一轮的重复终端不该影响当前轮，得到 %v", act)
	}
	if act := kongWSTurnEventAction(c, "response.completed", "resp_B", released); act != kongWSTurnKeep {
		t.Fatalf("被覆盖那一轮的终端不该释放新一轮，得到 %v", act)
	}
	meta := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + strings.Repeat("r", 780) + `"}}`)
	if err := g.GuardWSDownstream(context.Background(), account, c, meta); !KongIsDeliveryBlocked(err) {
		t.Fatalf("新一轮带 state 的 metadata 必须拦下，得到 %v", err)
	}
	// 新一轮自己的终端照常释放（它没绑过 id，带 id 的终端即本轮终端）。
	if act := kongWSTurnEventAction(c, "response.completed", "resp_C", released); act != kongWSTurnClear {
		t.Errorf("新一轮自己的终端应当释放它，得到 %v", act)
	}
}

func TestKongWSInheritRetiredCarriesChain(t *testing.T) {
	a := &KongUpstreamAttempt{ResponseID: "resp_A"}
	b := &KongUpstreamAttempt{}
	kongWSInheritRetired(a, b)
	b.ResponseID = "resp_B"
	c := &KongUpstreamAttempt{}
	kongWSInheritRetired(b, c)
	if got := strings.Join(c.RetiredResponseIDs, ","); got != "resp_A,resp_B" {
		t.Errorf("连续覆盖时应当带上整条链，得到 %q", got)
	}
	unbound := &KongUpstreamAttempt{}
	d := &KongUpstreamAttempt{}
	kongWSInheritRetired(unbound, d)
	if len(d.RetiredResponseIDs) != 0 {
		t.Errorf("上一轮没有已知 id 时无从记录，得到 %v", d.RetiredResponseIDs)
	}
}

// 承接轮结束之后，被它覆盖的那一轮的 id 仍要挡住：C 覆盖 B → C 正常结束 → D 开始、尚未绑 id →
// 上游又发来 B 的终端。退役 id 只挂在 C 上，C 一释放就没了，必须并进连接级的已释放集合。
func TestKongWSRetiredIDsOutliveTheInheritingTurn(t *testing.T) {
	g, account, _ := kongFeatureTestGateway(t, KongTicketModeFull, strings.Repeat("a", 780))
	released := map[string]bool{}
	b := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}, ResponseID: "resp_B"}
	c := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}}
	kongWSInheritRetired(b, c)

	if act := kongWSTurnEventAction(c, "response.completed", "resp_C", released); act != kongWSTurnClear {
		t.Fatalf("C 自己的终端应当释放它，得到 %v", act)
	}
	kongWSMarkReleased(released, c, "resp_C")

	d := &KongUpstreamAttempt{Model: "gpt-6-astra", Grant: &KongTicketGrant{Allowed: true, TicketID: 7}}
	kongWSInheritRetired(nil, d)
	if act := kongWSTurnEventAction(d, "response.completed", "resp_B", released); act != kongWSTurnKeep {
		t.Fatalf("被覆盖那一轮的终端不该释放再下一轮，得到 %v", act)
	}
	meta := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + strings.Repeat("r", 780) + `"}}`)
	if err := g.GuardWSDownstream(context.Background(), account, d, meta); !KongIsDeliveryBlocked(err) {
		t.Fatalf("D 带 state 的 metadata 必须拦下，得到 %v", err)
	}
	if act := kongWSTurnEventAction(d, "response.completed", "resp_D", released); act != kongWSTurnClear {
		t.Errorf("D 自己的终端照常释放，得到 %v", act)
	}
}
