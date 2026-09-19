//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 一张 verified 票只代表「按**当时**的目标与阈值判过」，而那两个值来自环境变量、可以改。
//
// 目标从 sol 换成 astra 之后，判为 sol 的旧票仍然满足 verified + 未过期——若据此注入，上游会接受它、
// 不重发 state，后置守卫永远不报错，于是直接交付了当前目标之外的输出。阈值提高同理。
func TestKongCurrentTicketHonorsCurrentTargetAndConfidence(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	state := strings.Repeat("a", 292)
	ticket := &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: state,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
	}
	repo.current[kongStubKey(1, "gpt-6-astra")] = ticket
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)

	// 1) 归因为别的模型：不得作为当前票
	ticket.FingerprintModel = kongStrPtr("gpt-5.6-sol")
	ticket.FingerprintP = kongFloatPtr(0.99)
	if got, err := repo.CurrentTicket(context.Background(), 1, "gpt-6-astra", svc.targetModel, svc.confidence); err != nil || got != nil {
		t.Errorf("判为旧目标的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 2) 概率低于当前阈值：不得作为当前票
	ticket.FingerprintModel = kongStrPtr("gpt-6-astra")
	ticket.FingerprintP = kongFloatPtr(0.5)
	if got, err := repo.CurrentTicket(context.Background(), 1, "gpt-6-astra", svc.targetModel, svc.confidence); err != nil || got != nil {
		t.Errorf("概率低于阈值的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 3) 归因字段缺失（历史数据）：同样不得放行
	ticket.FingerprintModel = nil
	ticket.FingerprintP = nil
	if got, err := repo.CurrentTicket(context.Background(), 1, "gpt-6-astra", svc.targetModel, svc.confidence); err != nil || got != nil {
		t.Errorf("归因字段缺失的票不得成为当前票：got=%v err=%v", got, err)
	}

	// 4) 满足当前规则：可用
	ticket.FingerprintModel = kongStrPtr("gpt-6-astra")
	ticket.FingerprintP = kongFloatPtr(0.95)
	got, err := repo.CurrentTicket(context.Background(), 1, "gpt-6-astra", svc.targetModel, svc.confidence)
	if err != nil || got == nil || got.ID != 7 {
		t.Errorf("满足当前规则的票应当可用：got=%v err=%v", got, err)
	}
}
