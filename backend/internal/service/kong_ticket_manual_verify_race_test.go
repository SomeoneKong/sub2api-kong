//go:build unit

package service

// 人工验票的时序与展示护栏。
//
// 这一组用例都在构造**先后交错**而不是并发：账号级槽位（kongVerifyOnlySlot）保证同一账号不会同时
// 跑两拨挑战，但它挡不住"读快照 → 别的任务改了库 → 占到槽位"这种交错，而人工验票的每一步都要花
// 真实额度（一张票最多三份挑战），按过时快照走下去就是白烧。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func kongManualFixture(t *testing.T, model string) (*kongStubRepo, *kongStubUpstream, *KongTicketService) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	svc.accept = KongTicketAccept{model: []string{model}}
	return repo, up, svc
}

// kongReadHookRepo 在读票之后插一个钩子，用来把"另一个任务"精确插进读与占槽之间。
type kongReadHookRepo struct {
	*kongStubRepo
	afterRead func()
}

func (r *kongReadHookRepo) TicketByID(ctx context.Context, accountID, id int64) (*KongTicket, error) {
	t, err := r.kongStubRepo.TicketByID(ctx, accountID, id)
	if r.afterRead != nil {
		hook := r.afterRead
		r.afterRead = nil
		hook()
	}
	return t, err
}

// kongChallengeHook 在第一份挑战发出前插一个钩子，用来模拟"验证进行中，业务路径撤销了票"。
type kongChallengeHook struct {
	*kongStubUpstream
	before func()
}

func (u *kongChallengeHook) RunChallenge(ctx context.Context, a *Account, proxy, model string,
	c KongFingerprintChallenge, state string,
) (*KongUpstreamAnswer, error) {
	if u.before != nil {
		hook := u.before
		u.before = nil
		hook()
	}
	return u.kongStubUpstream.RunChallenge(ctx, a, proxy, model, c, state)
}

// 被跳过的候选被点名验证时，不能出现"挑战发出去了、结论却提交不上"。
//
// CommitVerification 要求 `NOT skip_until_new`，所以不清标记就必然零行落库：额度烧了、票还留在
// 候选池外，再点一次再烧一次。断言写成"要么零挑战、要么验过能提交"，两种修法都放行。
func TestKongManualVerifySkippedCandidateNotWasted(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo, up, svc := kongManualFixture(t, model)
	candidate := kongSeedCandidate(repo, model, 21, time.Minute)
	repo.tickets[candidate.ID].SkipUntilNew = true
	candidate.SkipUntilNew = true

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, candidate.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if up.challengeCall > 0 && !out.Accepted {
		t.Fatalf("烧了 %d 份挑战却提交不了结论：%+v", up.challengeCall, out)
	}
}

// 一张**仍然合格、只是过期较早**的备用 verified 票，同样受"证据不完整不推翻既有结论"的保护。
//
// 它已经完整通过过一次，按首次候选验证对待的话，一份不合格归因加两份 429 就能把它判成 rejected
// ——而它恰恰是当前票被撤销后唯一的接替者，那是无谓拒服。
func TestKongManualVerifyBackupVerifiedKeepsVerdict(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	current := kongSeedCurrentTicket(repo, model, time.Now())
	backup := kongSeedCandidate(repo, model, 21, 20*time.Minute)
	backup.Status = KongTicketStatusVerified
	backup.ExpiresAt = time.Now().Add(20 * time.Minute)
	backup.FingerprintProbs = map[string]float64{model: 0.97}
	repo.tickets[backup.ID].Status = backup.Status
	repo.tickets[backup.ID].ExpiresAt = backup.ExpiresAt
	// 一份有效回答 + 两份基础设施故障 = 证据不完整。
	up.answerErrFor = map[int]error{1: errors.New("429"), 2: errors.New("timeout")}
	// 验证途中业务路径撤销了原当前票，于是这张备用票成了唯一的接替者。
	svc.upstream = &kongChallengeHook{kongStubUpstream: up, before: func() {
		svc.RevokeUsedTicket(context.Background(), 1, model, current.ID)
	}}

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, backup.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	if repo.tickets[backup.ID].Status != KongTicketStatusVerified || !out.Inconclusive {
		t.Fatalf("证据不完整不该推翻备用票的既有结论：status=%s result=%+v",
			repo.tickets[backup.ID].Status, out)
	}
}

// 占到槽位之后必须重读票：读与占槽之间，一个刚释放槽位的自动任务可能已经把它判成 rejected。
// 按旧快照走下去会跳过该做的准备，然后发出挑战、最后必被提交条件挡掉。
func TestKongManualVerifyRereadsAfterSlotClaim(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	candidate := kongSeedCandidate(repo, model, 21, time.Minute)
	hooked := &kongReadHookRepo{kongStubRepo: repo}
	hooked.afterRead = func() {
		// 模拟自动任务：占槽 → 把这张候选验成 rejected → 释放槽位。
		task, outcome := svc.claimTask(1, model, kongVerifyOnlySlot(1))
		if outcome != kongClaimFresh {
			t.Fatal("构造用的自动任务占不到槽位")
		}
		account, _ := svc.accounts.GetByID(context.Background(), 1)
		cfg, _ := ParseKongTicketConfig(account.Extra)
		_, _ = svc.verifyExistingCandidate(context.Background(), account, cfg, model, time.Now(), false)
		if repo.tickets[candidate.ID].Status != KongTicketStatusRejected {
			t.Fatal("自动任务没把候选判成 rejected，构造失败")
		}
		svc.releaseTask(1, task)
	}
	svc.repo = hooked

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, candidate.ID)
	if err != nil {
		t.Fatalf("按 id 验票: %v", err)
	}
	// 重读会看到 rejected，于是先复位再验；沿用旧快照则一次复位都不会发生。
	if len(repo.revived) != 1 {
		t.Fatalf("占槽后仍在用过时快照：挑战 %d 份、复位 %v、结果 %+v",
			up.challengeCall, repo.revived, out)
	}
}

// 桩的返回值必须整份来自权威状态。生产是 `UPDATE … RETURNING`，不可能"用 A 行判断、返回 B 行字段"
// ——而调用方正是据 ExpiresAt / Source 判期限与来源的。
func TestKongStubPrepareReturnsAuthoritativeSnapshot(t *testing.T) {
	repo := newKongStubRepo()
	ticket := kongSeedCandidate(repo, "gpt-5.6-sol", 21, time.Minute)
	repo.tickets[ticket.ID].Status = KongTicketStatusRejected
	repo.tickets[ticket.ID].ExpiresAt = time.Now().Add(10 * time.Minute)
	repo.tickets[ticket.ID].Source = KongTicketSourceObserved

	got, err := repo.PrepareTicketForManualVerify(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("准备重验: %v", err)
	}
	state := repo.tickets[ticket.ID]
	if got == nil || !got.ExpiresAt.Equal(state.ExpiresAt) || got.Source != state.Source {
		t.Fatalf("返回值没反映权威行：got=%+v state=%+v", got, state)
	}
	if got.Status != KongTicketStatusUnverified || got.SkipUntilNew {
		t.Fatalf("已拒票该被复位且清掉跳过标记，实得 %+v", got)
	}
}

// 列表的行数上限要与生产一致：0、负数、超上限都归一化到同一个值。
func TestKongStubListTicketsHonorsCap(t *testing.T) {
	repo := newKongStubRepo()
	for i := int64(1); i <= kongStubTicketListMax+1; i++ {
		kongSeedCandidate(repo, "gpt-5.6-sol", i, time.Minute)
	}
	for _, limit := range []int{0, -1, kongStubTicketListMax + 1} {
		got, err := repo.ListTickets(context.Background(), 1, limit)
		if err != nil {
			t.Fatalf("列票: %v", err)
		}
		if len(got) != kongStubTicketListMax {
			t.Errorf("limit=%d 返回 %d 行，生产封顶 %d", limit, len(got), kongStubTicketListMax)
		}
	}
}

// 「别的路径撤销了票」不等于「本次测出真降档」，因此不能据此去验第二张。
//
// 时序：人工重验当前票期间，业务请求拿到上游重发的票并撤销了它；人工的挑战自己全部 429。这时库里
// 已是 rejected，但本次验证什么都没测出来——凭库状态往下走就会白烧一整张候选的额度。
func TestKongManualVerifyExternalRevocationIsNotDowngrade(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo, up, svc := kongManualFixture(t, model)
	current := kongSeedCurrentTicket(repo, model, time.Now())
	kongSeedCandidate(repo, model, 21, time.Minute)
	up.answerErr = errors.New("429")
	svc.upstream = &kongChallengeHook{kongStubUpstream: up, before: func() {
		svc.RevokeUsedTicket(context.Background(), 1, model, current.ID)
	}}

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 1 {
		t.Fatalf("外部撤销被误判成真降档：挑战 %d 份、结果 %+v", up.challengeCall, out)
	}
}

// 验证通过只是"加入可用集合"，**不等于成为当前票**。
//
// 当前票的选择依据是剩余寿命最长的合格票（VerifiedTickets 按 expires_at DESC），目标是让票服务
// 最久。让一张更早过期的票顶替一张更晚过期的好票，会让该模型更早进入无票期——那是无谓拒服。
// 这条是反向护栏：改成"最后验过的必然接替"会让它红。
func TestKongManualVerifyEarlierExpiryDoesNotReplaceCurrent(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo, _, svc := kongManualFixture(t, model)
	current := kongSeedCurrentTicket(repo, model, time.Now()) // 还剩 40 分钟
	earlier := kongSeedCandidate(repo, model, 21, 20*time.Minute)
	earlier.Source = KongTicketSourceObserved
	earlier.ExpiresAt = time.Now().Add(20 * time.Minute)
	repo.tickets[earlier.ID].Source = earlier.Source
	repo.tickets[earlier.ID].ExpiresAt = earlier.ExpiresAt

	out, err := svc.TriggerVerifyTicket(context.Background(), 1, earlier.ID)
	if err != nil || !out.Accepted {
		t.Fatalf("这张候选本身应当验得通过：out=%+v err=%v", out, err)
	}
	cur := kongStillCurrent(t, repo, svc, model)
	if cur == nil || cur.ID != current.ID {
		t.Fatalf("更早过期的票不该顶替当前票 #%d，实得 %+v", current.ID, cur)
	}
	// 但它确实进了可用集合：撤掉当前票之后它就是接替者。
	svc.RevokeUsedTicket(context.Background(), 1, model, current.ID)
	if cur := kongStillCurrent(t, repo, svc, model); cur == nil || cur.ID != earlier.ID {
		t.Fatalf("当前票撤销后应由 #%d 接替，实得 %+v", earlier.ID, cur)
	}
}

// 详情页必须按模式区分「能不能验」与「在不在注入」，并且任何模式下都不下发票原值。
func TestKongTicketDetailModePolicies(t *testing.T) {
	for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve, KongTicketModeFull} {
		t.Run(string(mode), func(t *testing.T) {
			const model = "gpt-5.6-sol"
			repo, _, svc := kongManualFixture(t, model)
			kongSeedCurrentTicket(repo, model, time.Now())
			views := &kongStubAdminAccounts{views: []KongAccountView{
				{ID: 1, Ready: true, Extra: map[string]any{KongTicketModeKey: string(mode)}},
			}}
			admin := NewKongTicketAdminService(repo, views, KongDefaultTicketParams(),
				[]string{model}, svc.accept, svc.confidence)
			admin.SetTicketService(svc)

			page, err := admin.TicketDetail(context.Background(), 1)
			if err != nil {
				t.Fatalf("票据详情: %v", err)
			}
			row := page.Tickets[0]
			// off 下 recheckVerify 必然以「已退出保护」中止，问不出答案，所以不该给验票入口。
			if mode == KongTicketModeOff {
				if row.Verifiable {
					t.Error("off 仍把票标成可验")
				}
				if row.NotVerifiableReason == "" {
					t.Error("不可验必须给出原因，否则页面只能猜")
				}
			} else if !row.Verifiable {
				t.Errorf("%s 应当可以验票（验证走流量出口，不要求票据出口可用）：%s",
					mode, row.NotVerifiableReason)
			}
			// 只有 full 在注入；off / observe 的存量票只是"首选"，不是"在用"。
			if mode != KongTicketModeFull && row.IsCurrent {
				t.Error("不注入的模式不该宣称有票在服务")
			}
			if !row.Preferred {
				t.Error("按当前判据它就是首选票，这个事实各模式都该给出")
			}

			encoded, err := json.Marshal(page)
			if err != nil {
				t.Fatalf("序列化: %v", err)
			}
			if strings.Contains(string(encoded), strings.Repeat("a", 292)) ||
				strings.Contains(string(encoded), `"State"`) || strings.Contains(string(encoded), `"state"`) {
				t.Error("票原值泄漏进了管理响应")
			}
		})
	}
}
