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

// kongSeedCandidate 放一张未验候选。capturedAgo 越小越新——「挑最新」那条规则靠它区分。
func kongSeedCandidate(repo *kongStubRepo, model string, id int64, capturedAgo time.Duration) *KongTicket {
	now := time.Now()
	t := &KongTicket{
		ID: id, AccountID: 1, Model: model, State: strings.Repeat("c", 292),
		Status: KongTicketStatusUnverified, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(40 * time.Minute), CapturedAt: now.Add(-capturedAgo),
	}
	key := kongStubKey(1, model)
	repo.candidates[key] = append(repo.candidates[key], t)
	repo.tickets[id] = &kongStubTicketState{
		AccountID: 1, Model: model, Status: KongTicketStatusUnverified,
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

// 没有当前票时验候选，而且验的是**最新**那张——不是自动路径那个最老的。
//
// 这一条同时锁住两件事：本端点会验候选（批量取票之后"有票但未验"是常态，藏起来就只能等该模型
// 自己来请求），以及挑的是最新那张（剩余 TTL 最长）。把 NewestCandidate 换回 OldestCandidate
// 会让它红。
func TestKongTriggerVerifyVerifiesNewestCandidate(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	// 合成回答归因到 sol，白名单接受 sol → 这次候选验证会通过。
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	// 没有当前票，池里两张候选：一张一小时前采的，一张刚采的。
	kongSeedCandidate(repo, model, 5, time.Hour)
	newest := kongSeedCandidate(repo, model, 6, time.Minute)

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 1 {
		t.Fatalf("只该验一张候选，实得 %d 步：%+v", len(out.Steps), out.Steps)
	}
	step := out.Steps[0]
	if !step.Candidate || step.TicketID != newest.ID {
		t.Fatalf("该验最新那张候选 #%d，实得 %+v", newest.ID, step)
	}
	if !step.Accepted || !out.Accepted || !out.Candidate {
		t.Fatalf("候选合格时应报通过，实得 step=%+v out=%+v", step, out)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil || cur.ID != newest.ID {
		t.Fatalf("验过的候选应当成为当前票 #%d，实得 %+v", newest.ID, cur)
	}
}

// 当前票**真降档**（证据完整、归因不合格）之后才继续验候选：那时当前票已被作废，确实缺票。
func TestKongTriggerVerifyFallsToCandidateAfterProvenDowngrade(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	// 回答归因到 sol，白名单只接受 astra → 当前票与候选都会被判不合格。
	const model = "gpt-6-astra"
	svc.accept = KongTicketAccept{model: []string{model}}
	current := kongSeedCurrentTicket(repo, model, time.Now())
	candidate := kongSeedCandidate(repo, model, 7, time.Minute)

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 2 {
		t.Fatalf("当前票真降档后应接着验一张候选，实得 %d 步：%+v", len(out.Steps), out.Steps)
	}
	if out.Steps[0].TicketID != current.ID || out.Steps[0].Candidate || !out.Steps[0].Revoked {
		t.Fatalf("第一步该是作废当前票 #%d，实得 %+v", current.ID, out.Steps[0])
	}
	if out.Steps[1].TicketID != candidate.ID || !out.Steps[1].Candidate {
		t.Fatalf("第二步该是验候选 #%d，实得 %+v", candidate.ID, out.Steps[1])
	}
}

// 「没得出结论」时**不再验候选**：旧票与旧结论都还在、并不缺票，而 429/超时那类故障多半会在候选
// 上原样重演——继续验就是白烧一张票的额度（最多三份真实挑战）。
func TestKongTriggerVerifyStopsAtInconclusive(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    kongVerifyAnswers(),
		answerErr:  errors.New("上游 429"),
	}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	current := kongSeedCurrentTicket(repo, model, time.Now())
	kongSeedCandidate(repo, model, 8, time.Minute)

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 1 || out.Steps[0].Candidate {
		t.Fatalf("没得出结论时不该再动候选，实得 %+v", out.Steps)
	}
	if !out.Inconclusive || out.Revoked {
		t.Fatalf("应报未得出结论且不作废，实得 %+v", out)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil || cur.ID != current.ID {
		t.Fatalf("旧票必须留着，实得 %+v", cur)
	}
	if up.challengeCall > len(KongFingerprintChallenges()) {
		t.Errorf("只该为一张票发起挑战，实际发了 %d 份", up.challengeCall)
	}
}

// 上游重发票时也**不再验候选**：那说明我们注入的票一律不被接受，候选必然同样被拒。
func TestKongTriggerVerifyStopsWhenUpstreamReissued(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		answers:    []*KongUpstreamAnswer{{Text: "1,2,3", EchoedState: strings.Repeat("z", 292)}},
	}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	current := kongSeedCurrentTicket(repo, model, time.Now())
	candidate := kongSeedCandidate(repo, model, 9, time.Minute)

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 1 || out.Steps[0].TicketID != current.ID {
		t.Fatalf("上游重发票后不该再验候选，实得 %+v", out.Steps)
	}
	if !out.Revoked {
		t.Fatalf("注入不被接受时该作废当前票，实得 %+v", out)
	}
	if st, _ := repo.TicketStatus(context.Background(), candidate.ID); st != KongTicketStatusUnverified {
		t.Errorf("候选应原样留着，实为 %s", st)
	}
}

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
