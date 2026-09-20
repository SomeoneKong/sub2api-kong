//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// kongVerifyTestAccount 造一个 full + 直连票据出口、业务走代理的账号（两个出口不重合，
// 否则 KongEvaluateEgress 会判成退化）。
func kongVerifyTestAccount() *Account {
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	return account
}

// kongSeedCurrentTicket 放一张已验证、未过期的当前票。
func kongSeedCurrentTicket(repo *kongStubRepo, model string, now time.Time) *KongTicket {
	t := &KongTicket{
		ID: 11, AccountID: 1, Model: model, State: strings.Repeat("a", 292),
		Status: KongTicketStatusVerified, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(40 * time.Minute), CapturedAt: now.Add(-2 * time.Minute),
		FingerprintModel: kongStrPtr(model), FingerprintP: kongFloatPtr(0.97),
		FingerprintProbs: map[string]float64{model: 0.97},
	}
	repo.setCurrent(1, model, t)
	repo.tickets[t.ID] = &kongStubTicketState{
		AccountID: 1, Model: model, Status: KongTicketStatusVerified,
		ExpiresAt: t.ExpiresAt, CapturedAt: t.CapturedAt, Source: KongTicketSourceFetch,
	}
	return t
}

// kongStillCurrent 报告这张票此刻还能不能被选成当前票。
func kongStillCurrent(t *testing.T, repo *kongStubRepo, svc *KongTicketService, model string) *KongTicket {
	t.Helper()
	cur, err := kongPickCurrent(context.Background(), repo, svc.accept, svc.confidence, 1, model)
	if err != nil {
		t.Fatalf("重读当前票: %v", err)
	}
	return cur
}

// 验通过时票必须留着。这条是反面护栏：把"只在真验出问题时作废"写成"验完就作废"同样能让下面那些
// 用例过。
func TestKongTriggerVerifyKeepsTicketWhenAccepted(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	// 合成回答归因到 sol；用生产那套白名单，于是这次重验会通过。
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model, "gpt-6-astra", "gpt-5.5"}}
	ticket := kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if out.TicketID != ticket.ID {
		t.Fatalf("验的应是当前票 #%d，实为 #%d", ticket.ID, out.TicketID)
	}
	if !out.Accepted || out.Revoked || out.Inconclusive {
		t.Fatalf("重验应通过，实得 accepted=%v revoked=%v inconclusive=%v reason=%q",
			out.Accepted, out.Revoked, out.Inconclusive, out.Reason)
	}
	if len(repo.revoked) != 0 {
		t.Errorf("通过的票不该被作废，实际撤销了 %v", repo.revoked)
	}
	// **不该取新票**：这个按钮只管验当前那张，取票要消耗票据出口的静默。
	if up.fetchCalls != 0 {
		t.Errorf("立即验票不该取票，实际取了 %d 次", up.fetchCalls)
	}
}

// 基础设施故障**不得**作废票。
//
// 上游 429、网络抖动、单份挑战超时都没有证伪那张票的原结论，而重新取票要等约 34 分钟静默——
// 据此作废一张仍在 TTL 内的好票就是无谓拒服，与放行降智同级。
func TestKongTriggerVerifyKeepsTicketOnInfraFailure(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answerErr:  errors.New("上游 429"),
	}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	ticket := kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if out.Accepted || out.Revoked {
		t.Fatalf("没能完成测量，不该判成通过也不该作废：%+v", out)
	}
	if !out.Inconclusive {
		t.Fatal("应报「未得出结论」，否则页面会把基础设施故障说成票失效")
	}
	if out.Reason == "" {
		t.Error("未得出结论也要给原因，运维才知道是探测失败还是降档")
	}
	if len(repo.revoked) != 0 {
		t.Fatalf("不该撤销任何票，实际撤销了 %v", repo.revoked)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil || cur.ID != ticket.ID {
		t.Fatalf("旧票必须仍然可用，实得 %v", cur)
	}
	// 重验一张旧票失败说明不了这条出口现在有问题，罚它只会推迟下一个正常取票周期。
	for _, e := range repo.events {
		if e.EventType == KongEventCooldown {
			t.Errorf("重验失败不该进冷却，却记了 %v", e.Detail)
		}
	}
}

// 证据不完整同样不作废：一份有效回答加两份失败会让归因概率天然偏低，据此提交 rejected 等于让
// 基础设施故障推翻一个既有结论。
func TestKongTriggerVerifyKeepsTicketOnIncompleteEvidence(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    kongVerifyAnswers(),
		// 第一份有效，后两份失败。
		answerErrFor: map[int]error{1: errors.New("上游 429"), 2: errors.New("上游超时")},
	}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-6-astra"
	// 合成回答归因到 sol，而白名单只接受 astra——若按不完整证据判，它会被判不合格。
	svc.accept = KongTicketAccept{model: []string{model}}
	ticket := kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if out.Revoked {
		t.Fatal("证据不完整不足以推翻既有结论，不该作废")
	}
	if !out.Inconclusive {
		t.Fatalf("应报「未得出结论」，实得 %+v", out)
	}
	if st, _ := repo.TicketStatus(context.Background(), ticket.ID); st != KongTicketStatusVerified {
		t.Fatalf("票的状态应保持 verified，实为 %s", st)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil {
		t.Fatal("旧票必须仍然可用")
	}
}

// 上游明确重发了票 = 我们这张注入已经不作数，**必须作废**。这是"真验出问题"里最硬的一种证据。
func TestKongTriggerVerifyRevokesWhenUpstreamReissues(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers: []*KongUpstreamAnswer{
			{Text: "1,2,3", EchoedState: strings.Repeat("z", 292)},
		},
	}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	ticket := kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if !out.Revoked || out.Inconclusive {
		t.Fatalf("上游重发票时必须作废，实得 %+v", out)
	}
	if len(repo.revoked) != 1 || repo.revoked[0] != ticket.ID {
		t.Fatalf("应当撤销 #%d，实际撤销了 %v", ticket.ID, repo.revoked)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur != nil {
		t.Fatalf("作废后不该还有当前票，实得 #%d", cur.ID)
	}
}

// 证据完整而归因不合格 = 真降档，作废（这一步由 verifyTicket 自己提交 rejected 完成）。
func TestKongTriggerVerifyRevokesOnProvenDowngrade(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	// 合成回答归因到 sol，白名单只接受 astra，且三份挑战都有效 → 证据完整、结论是不合格。
	const model = "gpt-6-astra"
	svc.accept = KongTicketAccept{model: []string{model}}
	ticket := kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if !out.Revoked {
		t.Fatalf("证据完整且归因不合格时必须作废，实得 %+v", out)
	}
	if st, _ := repo.TicketStatus(context.Background(), ticket.ID); st != KongTicketStatusRejected {
		t.Fatalf("票应被判成 rejected，实为 %s", st)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur != nil {
		t.Fatalf("不合格的票不该还能当当前票，实得 #%d", cur.ID)
	}
}

// 没有当前票时什么都不做——尤其**不能顺手去验候选**，那是 TriggerRefresh 的职责，
// 一个按钮两种语义会让运维分不清自己触发了什么。
func TestKongTriggerVerifyNoCurrentTicket(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	// 只有一张候选（unverified），没有当前票。
	repo.candidate[kongStubKey(1, model)] = &KongTicket{
		ID: 5, AccountID: 1, Model: model, State: strings.Repeat("b", 292),
		Status: KongTicketStatusUnverified, Source: KongTicketSourceObserved,
		ExpiresAt: time.Now().Add(30 * time.Minute), CapturedAt: time.Now().Add(-time.Hour),
	}

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if !out.NotApplicable || out.TicketID != 0 {
		t.Fatalf("没有当前票应报 not_applicable，实得 %+v", out)
	}
	if up.challengeCall != 0 {
		t.Errorf("不该对候选发起挑战，实际发了 %d 份", up.challengeCall)
	}
	if len(repo.revoked) != 0 {
		t.Errorf("不该撤销任何票，实际撤销了 %v", repo.revoked)
	}
}

// off 模式不走这条路：recheckVerify 会以「已退出保护」立刻作废结论，那时再按"验不成功"处置就是
// 纯粹的误伤。
func TestKongTriggerVerifyOffModeNotApplicable(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeOff, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	kongSeedCurrentTicket(repo, model, time.Now())

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if !out.NotApplicable {
		t.Fatalf("off 应报 not_applicable，实得 %+v", out)
	}
	if len(repo.revoked) != 0 {
		t.Errorf("off 下不该动票，实际撤销了 %v", repo.revoked)
	}
}
