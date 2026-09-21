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

// **off 也能手动验票。** 那个模式照样收业务响应带回的票（只是不注入、不自动探测），而它手上那些
// 票的档位正是"该不该给这个账号开 full"的判据——验不了就只能靠猜。
//
// 验证走流量出口、不注入、不消耗票据出口的静默，所以模式与它无关。只有**取票**是 full 专属。
func TestKongTriggerVerifyWorksInOffMode(t *testing.T) {
	const model = "gpt-5.6-sol"
	for _, tc := range []struct {
		name string
		mode KongTicketMode
	}{
		{"off", KongTicketModeOff},
		{"observe", KongTicketModeObserve},
		{"full", KongTicketModeFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newKongStubRepo()
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongTestAccount(1, tc.mode, KongTicketEgressDirect)
			proxyID := int64(100)
			account.ProxyID = &proxyID
			accounts.set(account)
			up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
			svc := kongTestService(t, repo, up, accounts)
			svc.accept = KongTicketAccept{model: []string{model}}
			current := kongSeedCurrentTicket(repo, model, time.Now())

			out, err := svc.TriggerVerify(context.Background(), 1, model)
			if err != nil {
				t.Fatalf("立即验票: %v", err)
			}
			if out.NotApplicable {
				t.Fatalf("手上有票就该验，不该报 not_applicable：%+v", out)
			}
			if len(out.Steps) == 0 || out.Steps[0].TicketID != current.ID {
				t.Fatalf("该验当前票 #%d，实得 %+v（deny=%s）", current.ID, out.Steps, out.DenyReason)
			}
			if !out.Steps[0].Accepted {
				t.Fatalf("证据合格时该判通过，实得 %+v", out.Steps[0])
			}
			if len(repo.revoked) != 0 {
				t.Errorf("重新自证合格的票不该被作废，实际撤销了 %v", repo.revoked)
			}
		})
	}
}

// 按 id 验一张票同样不看模式：票列表页每一行都能验。
func TestKongTriggerVerifyTicketWorksInOffMode(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeOff, KongTicketEgressNone)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	svc.accept = KongTicketAccept{model: []string{model}}
	// 票据出口是 none——off 的常态配置。验证走流量出口，所以它不该妨碍验票。
	candidate := kongSeedCandidate(repo, model, 21, time.Minute)

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, candidate.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if out.NotApplicable {
		t.Fatalf("off + 票据出口 none 照样能验，实得 %+v（deny=%s）", out, out.DenyReason)
	}
	if len(out.Steps) != 1 || out.Steps[0].TicketID != candidate.ID {
		t.Fatalf("只该验点名那一张 #%d，实得 %+v", candidate.ID, out.Steps)
	}
	if !out.Steps[0].Accepted {
		t.Fatalf("证据合格时该判通过，实得 %+v", out.Steps[0])
	}
}

// 按 id 验一张**已拒**票：服务端先把它复位成候选再验。
//
// 锁的是"详情页上已拒的票可以重验"——改过接受白名单或阈值之后，同一份证据可能就合格了。
// 这里没有别的 verified 票，所以验过之后它自然成为当前票；"更晚过期才接替"那条规则由
// TestKongManualVerifyEarlierExpiryDoesNotReplaceCurrent 专门锁。
func TestKongTriggerVerifyTicketRevivesRejected(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	svc.accept = KongTicketAccept{model: []string{model}}
	// 一张已拒但未过期的票，且它比当前票晚过期。
	rejected := kongSeedCandidate(repo, model, 21, time.Minute)
	repo.tickets[rejected.ID].Status = KongTicketStatusRejected
	rejected.Status = KongTicketStatusRejected

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, rejected.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if len(out.Steps) != 1 || out.Steps[0].TicketID != rejected.ID {
		t.Fatalf("只该验点名那一张 #%d，实得 %+v", rejected.ID, out.Steps)
	}
	if !out.Accepted {
		t.Fatalf("复位后应能重新判定合格，实得 %+v（reason=%s）", out, out.Reason)
	}
	if len(repo.revived) != 1 || repo.revived[0] != rejected.ID {
		t.Fatalf("应当先复位 #%d，实际复位了 %v", rejected.ID, repo.revived)
	}
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil || cur.ID != rejected.ID {
		t.Fatalf("验过的票应当成为当前票 #%d，实得 %+v", rejected.ID, cur)
	}
}

// 已过期的票不验：票本身已失效，验它只是白烧额度（最多三份真实挑战）。
func TestKongTriggerVerifyTicketSkipsExpired(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	expired := kongSeedCandidate(repo, model, 22, time.Minute)
	repo.tickets[expired.ID].ExpiresAt = time.Now().Add(-time.Minute)
	expired.ExpiresAt = time.Now().Add(-time.Minute)

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, expired.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if !out.NotApplicable || len(out.Steps) != 0 {
		t.Fatalf("过期票不该验，实得 %+v", out)
	}
	if up.challengeCall != 0 {
		t.Errorf("不该发起任何挑战，实际发了 %d 份", up.challengeCall)
	}
}

// 别人的票验不了：account_id 参与匹配是权限边界，不是优化。
func TestKongTriggerVerifyTicketRejectsForeignTicket(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	const model = "gpt-5.6-sol"
	other := kongSeedCandidate(repo, model, 23, time.Minute)
	// 把它改挂到另一个账号名下。
	other.AccountID = 9
	repo.tickets[other.ID].AccountID = 9

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, other.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if !out.NotApplicable || len(out.Steps) != 0 {
		t.Fatalf("不属于本账号的票不该被验，实得 %+v", out)
	}
	if up.challengeCall != 0 {
		t.Errorf("不该发起任何挑战，实际发了 %d 份", up.challengeCall)
	}
}

// 总览页那个"可验候选 N 张"必须与服务端实际会选的那一张同口径。
//
// 被排出候选池的票（`skip_until_new`）不在 NewestCandidate 的范围里。计数若把它们算进去，页面就会
// 显示一个点下去必然报"没有票可验"的按钮，而同一行还写着候选有几张——两句互相打脸。这类自相矛盾的
// 陈述按本项目口径算真缺陷：管理页面是排查降智的唯一窗口。
func TestKongUnverifiedCountMatchesSelectableCandidates(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo, _, svc := kongManualFixture(t, model)
	selectable := kongSeedCandidate(repo, model, 21, time.Minute)
	skipped := kongSeedCandidate(repo, model, 22, 2*time.Minute)
	repo.tickets[skipped.ID].SkipUntilNew = true

	ctx := context.Background()
	views := &kongStubAdminAccounts{views: []KongAccountView{
		{ID: 1, Ready: true, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeOff)}},
	}}
	admin := NewKongTicketAdminService(repo, views, KongDefaultTicketParams(),
		[]string{model}, svc.accept, svc.confidence)
	admin.SetTicketService(svc)

	status, err := admin.statusOf(ctx, &views.views[0], time.Now())
	if err != nil {
		t.Fatalf("查账号状态: %v", err)
	}
	if len(status.Models) != 1 {
		t.Fatalf("模型数 = %d, want 1", len(status.Models))
	}
	if got := status.Models[0].UnverifiedCount; got != 1 {
		t.Errorf("可验候选数 = %d, want 1（#%d 可选，#%d 已被排出候选池）", got, selectable.ID, skipped.ID)
	}

	// 反面：把可选的那张也排出去，计数必须归零——否则按钮还在，点了却报"没有票可验"。
	repo.tickets[selectable.ID].SkipUntilNew = true
	status, err = admin.statusOf(ctx, &views.views[0], time.Now())
	if err != nil {
		t.Fatalf("查账号状态: %v", err)
	}
	if got := status.Models[0].UnverifiedCount; got != 0 {
		t.Errorf("全部候选都被排出后计数应当是 0，实得 %d", got)
	}
	// 服务端此刻确实选不出东西——两侧口径一致才是这条用例要的。
	out, err := svc.TriggerVerify(ctx, 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if !out.NotApplicable {
		t.Errorf("没有可选候选时该报 not_applicable，实得 %+v", out)
	}
}

// 挑战期间前提变了，失败的副作用一个都不许施加。
//
// 三件事都不可逆：排除候选让它不再被自动选中、冷却把下一个正常周期往后推、返回的 sentinel 会让
// 人工入口撤销这张票。而"上游又下发了票"这个失败是在**另一套前提**下拿到的——模式被切走、出口被
// 改、账号换了上游身份之后，它说明不了这张票的任何问题。
// 覆盖**候选**路径上的两个新复核点：那里 skipCandidate / skipDrySpell / failCooldown 才真的会动。
//
// 重验一张正在服务的票时那三件事本来就直接返回（reverify 的分支），所以只种 verified 当前票的用例
// 抓不住"不排除候选、不冷却"这一半。这里种的是 unverified、fetch 来源的候选。
func TestKongVerifyCandidateKeepsSideEffectsWhenPreconditionLost(t *testing.T) {
	const model = "gpt-5.6-sol"
	for _, tc := range []struct {
		name      string
		echo      bool // true = 上游回显票（第一份挑战后就返回）；false = 三份都答不出
		breakAtNo int  // 第几份挑战时改掉前提（1 起）
	}{
		{"上游不接受候选", true, 1},
		{"三份都没有有效回答", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newKongStubRepo()
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongVerifyTestAccount()
			accounts.set(account)
			up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}
			if tc.echo {
				up.answers = kongVerifyAnswers()
				up.echoState = strings.Repeat("e", 292)
			} else {
				// 答不出可用序列：三份都是空文本，归因取不到数字。
				up.answers = []*KongUpstreamAnswer{{Text: ""}, {Text: ""}, {Text: ""}}
			}
			svc := kongTestService(t, repo, up, accounts)
			svc.accept = KongTicketAccept{model: []string{model}}
			candidate := kongSeedCandidate(repo, model, 21, time.Minute)
			// 另一张更早的 observed 候选：skipDrySpell 会批量标它，正好用来验"没有批量排除"。
			other := kongSeedCandidate(repo, model, 22, 5*time.Minute)
			repo.tickets[other.ID].Source = KongTicketSourceObserved

			calls := 0
			up.beforeChallenge = func() {
				calls++
				if calls == tc.breakAtNo {
					account.Extra[KongTicketModeKey] = string(KongTicketModeObserve)
					accounts.set(account)
				}
			}

			out, err := svc.TriggerVerifyTicket(context.Background(), 1, candidate.ID)
			if err != nil {
				t.Fatalf("按 id 验票: %v", err)
			}
			if len(out.Steps) != 1 {
				t.Fatalf("该有一步，实得 %+v（deny=%s）", out.Steps, out.DenyReason)
			}
			step := out.Steps[0]
			if step.Revoked {
				t.Errorf("前提已失效时不该作废候选 #%d", candidate.ID)
			}
			if !step.Inconclusive {
				t.Errorf("该报未得出结论，实得 %+v", step)
			}
			if st, _ := repo.TicketStatus(context.Background(), candidate.ID); st != KongTicketStatusUnverified {
				t.Errorf("候选应当原样留在池子里，实为 %s", st)
			}
			if repo.tickets[candidate.ID].SkipUntilNew {
				t.Error("前提已失效时不该把这张候选排出池子")
			}
			if repo.tickets[other.ID].SkipUntilNew {
				t.Error("前提已失效时不该批量排除本段无票期的其它候选")
			}
			if n := kongCountEvents(repo, KongEventCooldown); n != 0 {
				t.Errorf("前提已失效时不该推进冷却，实得 %d 条", n)
			}
		})
	}
}

func TestKongVerifyKeepsTicketWhenPreconditionLostAtNotAccepted(t *testing.T) {
	const model = "gpt-5.6-sol"
	for _, tc := range []struct {
		name   string
		break_ func(*kongStubAccounts, *Account)
	}{
		{"模式被切走", func(accs *kongStubAccounts, a *Account) {
			a.Extra[KongTicketModeKey] = string(KongTicketModeObserve)
			accs.set(a)
		}},
		{"账号换了上游身份", func(accs *kongStubAccounts, a *Account) {
			// chatgpt-account-id 在凭据里，不在 extra——它是出站请求头的来源之一。
			if a.Credentials == nil {
				a.Credentials = map[string]any{}
			}
			a.Credentials["chatgpt_account_id"] = "another-upstream-account"
			accs.set(a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newKongStubRepo()
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongVerifyTestAccount()
			accounts.set(account)
			// 上游"又下发了票"——注入未被接受的信号。
			up := &kongStubUpstream{
				proxyState: KongTicketProxyState{Exists: true},
				answers:    kongVerifyAnswers(),
				echoState:  strings.Repeat("e", 292),
			}
			svc := kongTestService(t, repo, up, accounts)
			svc.accept = KongTicketAccept{model: []string{model}}
			current := kongSeedCurrentTicket(repo, model, time.Now())
			// 挑战发出之后、判定之前把前提改掉。
			up.beforeChallenge = func() { tc.break_(accounts, account) }

			out, err := svc.TriggerVerify(context.Background(), 1, model)
			if err != nil {
				t.Fatalf("立即验票: %v", err)
			}
			if len(out.Steps) != 1 {
				t.Fatalf("该有一步，实得 %+v（deny=%s）", out.Steps, out.DenyReason)
			}
			step := out.Steps[0]
			if step.Revoked {
				t.Errorf("前提已失效时不该作废票 #%d：那次失败是在另一套前提下拿到的", current.ID)
			}
			if !step.Inconclusive {
				t.Errorf("该报未得出结论，实得 %+v", step)
			}
			if st, _ := repo.TicketStatus(context.Background(), current.ID); st != KongTicketStatusVerified {
				t.Errorf("票应当原样留着，实为 %s", st)
			}
			if n := kongCountEvents(repo, KongEventCooldown); n != 0 {
				t.Errorf("前提已失效时不该推进冷却，实得 %d 条", n)
			}
			if repo.tickets[current.ID].SkipUntilNew {
				t.Error("前提已失效时不该把票排出候选池")
			}
		})
	}
}

// 挑战全程复用验证开始时读到的那个 account，所以「以谁的身份出站」必须进快照。
//
// 管理端可以在验证期间把账号换绑到另一个上游账号（改 chatgpt-account-id、改 setup token）。那时
// 后续挑战仍以旧身份发出，而结论会被提交成**当前**账号的合格票——等于把别人那张票的证据授予了这个
// 账号，管理面与后续准入都会照着它走。
func TestKongVerifyRejectsConclusionAfterIdentityChange(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongVerifyTestAccount()
	accounts.set(account)
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	svc.accept = KongTicketAccept{model: []string{model}}
	candidate := kongSeedCandidate(repo, model, 21, time.Minute)

	swapped := false
	up.beforeChallenge = func() {
		if swapped {
			return
		}
		swapped = true
		if account.Credentials == nil {
			account.Credentials = map[string]any{}
		}
		account.Credentials["chatgpt_account_id"] = "another-upstream-account"
		accounts.set(account)
	}

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, candidate.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if len(out.Steps) != 1 {
		t.Fatalf("该有一步，实得 %+v（deny=%s）", out.Steps, out.DenyReason)
	}
	if out.Steps[0].Accepted {
		t.Errorf("换绑之后拿到的证据不能提交成合格票：%+v", out.Steps[0])
	}
	if st, _ := repo.TicketStatus(context.Background(), candidate.ID); st == KongTicketStatusVerified {
		t.Error("票被标成 verified 了——那是旧身份的证据")
	}
}

// 身份摘要要覆盖**每一个参与出站身份构造的值**，且**只**覆盖它们。
//
// 判据不是"看起来像不像配置"，而是"改了它，上游收到的请求有没有变"。漏一项就意味着换绑之后旧身份
// 的证据仍会被提交成当前账号的合格票；多含一项（比如会正常轮换的 access token）则会让每次刷新都
// 误杀一次验证。
func TestKongAccountIdentityDigestCoversOutboundIdentity(t *testing.T) {
	base := func() *Account {
		a := kongVerifyTestAccount()
		a.Credentials = map[string]any{
			"chatgpt_account_id": "acc-1",
			"chatgpt_user_id":    "user-1",
			"access_token":       "token-before",
		}
		return a
	}
	before := kongAccountIdentityDigest(base())

	t.Run("不变的项", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*Account)
		}{
			// access token 会正常轮换：把它算进去等于让每次刷新都误杀一次验证。
			{"OAuth access token 刷新", func(a *Account) { a.Credentials["access_token"] = "token-after" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := base()
				tc.mutate(a)
				if got := kongAccountIdentityDigest(a); got != before {
					t.Errorf("%s 不该算身份变化", tc.name)
				}
			})
		}
	})

	t.Run("必须算变化的项", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*Account)
		}{
			// 出站头 chatgpt-account-id 的来源。
			{"换了 chatgpt-account-id", func(a *Account) { a.Credentials["chatgpt_account_id"] = "acc-2" }},
			// 同一个 Team 换用户：account_id 不变而主体变了，项目已有的身份投影能区分。
			{"同 Team 换了用户", func(a *Account) { a.Credentials["chatgpt_user_id"] = "user-2" }},
			// 决定 x-openai-fedramp 头。
			{"FedRAMP 开关", func(a *Account) { a.Credentials["chatgpt_account_is_fedramp"] = true }},
			// 参与重建最终的 User-Agent / originator / version。
			{"账号级 User-Agent", func(a *Account) {
				a.Credentials["user_agent"] = "codex-cli/9.9.9 (another os) tty"
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := base()
				tc.mutate(a)
				if got := kongAccountIdentityDigest(a); got == before {
					t.Errorf("%s 必须算身份变化——它会改变上游收到的请求", tc.name)
				}
			})
		}
	})

	// 长度前缀防的是这个：直接用分隔符拼接时，值里含分隔符能让不同组合撞同一摘要。
	t.Run("含分隔符的值不撞摘要", func(t *testing.T) {
		a, b := base(), base()
		a.Credentials["chatgpt_account_id"] = "x"
		a.Credentials["chatgpt_user_id"] = "y|z"
		b.Credentials["chatgpt_account_id"] = "x|y"
		b.Credentials["chatgpt_user_id"] = "z"
		if kongAccountIdentityDigest(a) == kongAccountIdentityDigest(b) {
			t.Error("两组不同的身份拼出了同一个摘要")
		}
	})
}
