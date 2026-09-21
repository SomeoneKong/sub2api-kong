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
	"fmt"
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

// 详情页对三种模式的口径：**都能验**，但只有 full 宣称"在服务"，且任何模式下都不下发票原值。
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
				[]string{model}, svc.accept, svc.stg0, svc.confidence)
			admin.SetTicketService(svc)

			page, err := admin.TicketDetail(context.Background(), 1)
			if err != nil {
				t.Fatalf("票据详情: %v", err)
			}
			row := page.Tickets[0]
			// **三种模式都能验**：验证走流量出口、不注入、不要求票据出口可用。off 手上那些被动
			// 收下的票，档位正是"该不该给它开 full"的判据——藏掉入口就只能靠猜。
			if !row.Verifiable {
				t.Errorf("%s 应当可以验票，实得不可验：%s", mode, row.NotVerifiableReason)
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

// 别的账号此刻有可用票时，本账号的请求**不等**票据任务——报 preparing 让上层换号，而任务照常在
// 后台起（否则本账号永远补不上票）。
func TestKongHandsOffWhenAnotherAccountHasTicket(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	up.fetchState = strings.Repeat("a", 292)
	// 出口已静默过门槛，于是决策会选择取票。
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	// 另一个账号有一张可用票，且**按当前白名单仍然合格**——判据要求重判，所以归因分布必须给。
	kongSeedElsewhereTicket(repo, 2, model, 99, map[string]float64{model: 0.97})

	grant, err := svc.EnsureTicket(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if grant.Allowed || grant.DenyReason != KongDenyPreparing {
		t.Fatalf("别家有票时该报 preparing 让上层换号，实得 %+v", grant)
	}
}

// 别家也没票时退回同步等：换号救不了，等是唯一出路。
func TestKongWaitsWhenNoOtherAccountHasTicket(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	up.fetchState = strings.Repeat("a", 292)
	up.answers = kongVerifyAnswers()
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	// 库里只有本账号，没有别家的票。

	grant, err := svc.EnsureTicket(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("准备票: %v", err)
	}
	// 同步等意味着这次请求会拿到结果（票或明确的拒服原因），而不是 preparing。
	if grant.DenyReason == KongDenyPreparing {
		t.Fatalf("别家没票时不该让上层换号，换过去也没票：%+v", grant)
	}
	if up.fetchCalls == 0 {
		t.Fatal("应当在本请求上同步取票")
	}
}

// 票据拒服**都要能换账号**：票是按账号持有的，一个账号取不到票不说明别的账号也取不到。
//
// ⚠️ 这条用例此前断言的**恰好相反**（只有 preparing 可 failover，其余"换号救不了"）。那个假设把
// 账号级条件当成了全局条件：静默与冷却是按**该账号的票据出口**累积的，出口配置也是账号自己的。
// 生产实测（2026-09-20）有 64 个请求在同分组另一账号已持有合格票的情况下被直接拒掉——无谓拒服，
// 与放行降智同级。真正全局的情况（所有账号都没票）由 failover 框架自然耗尽，不在这一层判。
func TestKongTicketDenyAlwaysAllowsFailover(t *testing.T) {
	ctx := WithPrefetchedStickySession(context.Background(), 7, 1, false)
	allReasons := []string{
		KongDenyPreparing, KongDenyWindowClosed, KongDenyEgressUnusable,
		KongDenyNoTicketSource, KongDenyEgressBusy, KongDenyWaitTimeout,
		KongDenyAccountUnready, KongDenyOtherModelTask, KongDenyTaskNoTicket,
	}
	for _, reason := range allReasons {
		canFailover, _ := KongTicketFailover(ctx, &KongErrTicketDenied{Reason: reason})
		if !canFailover {
			t.Errorf("%s 必须允许换号——票是按账号持有的", reason)
		}
	}
	// 非票据的错误不受影响。
	if canFailover, _ := KongTicketFailover(ctx, errors.New("普通错误")); canFailover {
		t.Error("非票据拒服不该走这条路")
	}
}

// 转成 failover 之后，**每一种**拒服原因都必须仍能被认出来——包括带自由文本的技术原因。
//
// 上一版靠一张九个字符串的清单反查，于是 `PrepareUpstream` 产出的 `ensure_failed: <err>` /
// `model_undeterminable: <reason>` 一律漏掉：调度豁免失效，本地拒服被计入账号错误率 EWMA
// （实测 0 → 0.20），"票越缺、账号越被判坏"。身份因此改为认类型 + 受控前缀。
func TestKongTicketDenyIdentitySurvivesFailoverConversion(t *testing.T) {
	reasons := []string{
		KongDenyPreparing, KongDenyWindowClosed, KongDenyEgressUnusable,
		KongDenyNoTicketSource, KongDenyEgressBusy, KongDenyWaitTimeout,
		KongDenyAccountUnready, KongDenyOtherModelTask, KongDenyTaskNoTicket,
		// PrepareUpstream / WS 侧带自由文本的技术原因，清单形态的判据正是在这里漏的。
		"ensure_failed: dial tcp 10.0.0.1:5432: connect: refused",
		"model_undeterminable: duplicate_model_key",
		"frame_undeterminable: duplicate_key:model",
		"inject_failed: sjson: invalid path",
		"inject_unverified: turn_state_mismatch",
		"payload_missing",
	}
	for _, reason := range reasons {
		wrapped := newKongTicketDenyFailover(&KongErrTicketDenied{Reason: reason}, false)
		// 调度上报的两道判据都要认得它，否则这次本地拒服会被算成账号故障。
		if !KongIsTicketDeniedFailover(wrapped) {
			t.Errorf("%s: 转换后不再被识别成票据拒服", reason)
		}
		if !KongIsTicketDenied(wrapped) {
			t.Errorf("%s: 转换后丢了原始拒服类型", reason)
		}
		// handler 只拿得到摘出来的 failover 错误，身份必须落在它自己身上。
		var failoverErr *UpstreamFailoverError
		if !errors.As(wrapped, &failoverErr) {
			t.Fatalf("%s: 换号框架取不到 failover 错误", reason)
		}
		category, ok := KongTicketDenyReasonOf(failoverErr)
		if !ok {
			t.Errorf("%s: 耗尽呈现认不出票据拒服", reason)
		}
		// 自由文本只保留冒号前的分类：Reason 会一路流进客户端文案，不能搬运错误详情。
		if strings.ContainsAny(category, ": ") {
			t.Errorf("%s: 分类未规范化，实得 %q", reason, category)
		}
	}
	// **三种传参形态都要认**：调用方可能传完整包装、可能先 errors.As 摘出 failover 指针再单独传它
	// （WS 换号上报就是这样），也可能再 %w 包一层。多值 Unwrap 只能由外向内查找，所以摘出来的指针
	// 反查不到并列的 *KongErrTicketDenied——那时必须靠受控命名空间认出来，否则这次本地拒服又会被
	// 计进账号健康度 EWMA。
	for _, reason := range []string{KongDenyWindowClosed, "ensure_failed: dial tcp: refused"} {
		wrapped := newKongTicketDenyFailover(&KongErrTicketDenied{Reason: reason}, false)
		var extracted *UpstreamFailoverError
		if !errors.As(wrapped, &extracted) {
			t.Fatalf("%s: 取不到 failover 错误", reason)
		}
		forms := map[string]error{
			"完整包装":    wrapped,
			"摘出的指针":   extracted,
			"再包一层":    fmt.Errorf("websocket relay: %w", wrapped),
			"摘出后再包一层": fmt.Errorf("websocket relay: %w", extracted),
		}
		for name, form := range forms {
			if !KongIsTicketDeniedFailover(form) {
				t.Errorf("%s / %s: 调度上报的豁免不命中，本地拒服会被计进账号健康度", reason, name)
			}
		}
	}

	// 真实上游故障不得被当成票据拒服——否则就是把归因错误反着犯一遍。
	if _, ok := KongTicketDenyReasonOf(&UpstreamFailoverError{StatusCode: 500}); ok {
		t.Error("普通上游故障被识别成票据拒服")
	}
	if KongIsTicketDeniedFailover(&UpstreamFailoverError{StatusCode: 500}) {
		t.Error("普通上游故障被豁免了账号健康度")
	}
}

// 「先在同账号等一等」只对**几秒内可能自行好转**的原因成立。
//
// 对 window_closed 这类"要等到某个时刻才满"的原因先等本号纯属浪费重试次数，还会延后真正能服务的
// 那次换号。注意 retrySameAccount 为真也**不阻止**换号：框架重试耗尽后照样切。
func TestKongTicketDenySameAccountWaitOnlyWhenItCanImprove(t *testing.T) {
	ctx := WithPrefetchedStickySession(context.Background(), 7, 1, false)
	worthWaiting := map[string]bool{
		KongDenyPreparing:      true, // 本账号的任务正在跑，产物就是本账号要的票
		KongDenyOtherModelTask: true, // 同账号别的模型的任务，可能是不碰票据出口的候选验证
		// egress_busy 是**另一个账号**占着共享出口：它那次取票会把这条出口的静默清零，一释放本号
		// 就变成 window_closed，先等等于把重试次数花在一个确定不会变的状态上。
		KongDenyEgressBusy:     false,
		KongDenyWindowClosed:   false,
		KongDenyEgressUnusable: false,
		KongDenyNoTicketSource: false,
		KongDenyWaitTimeout:    false,
		KongDenyTaskNoTicket:   false,
		KongDenyAccountUnready: false,
	}
	for reason, want := range worthWaiting {
		_, retrySame := KongTicketFailover(ctx, &KongErrTicketDenied{Reason: reason})
		if retrySame != want {
			t.Errorf("%s 的同账号重试判定 = %v, want %v", reason, retrySame, want)
		}
	}

	// **读不到绑定信息时仍按"可能绑定了"处理**（对值得等的那几种）：OpenAI 主路径不写那个 context
	// 键，而两种猜错的后果不对称——该换却先等只多花几秒重试延时，该等却直接换会破坏会话。
	if _, retrySame := KongTicketFailover(context.Background(),
		&KongErrTicketDenied{Reason: KongDenyPreparing}); !retrySame {
		t.Error("绑定信息未知时应当保守先等本号")
	}
}

// kongSeedElsewhereTicket 给**别的账号**放一张可用票。probs 为 nil 时模拟"白名单收紧后已不合格"。
func kongSeedElsewhereTicket(repo *kongStubRepo, accountID int64, model string, id int64,
	probs map[string]float64,
) {
	now := time.Now()
	t := &KongTicket{
		ID: id, AccountID: accountID, Model: model, State: strings.Repeat("e", 292),
		Status: KongTicketStatusVerified, Source: KongTicketSourceFetch,
		ExpiresAt: now.Add(30 * time.Minute), CapturedAt: now,
		FingerprintProbs: probs,
	}
	key := kongStubKey(accountID, model)
	repo.current[key] = append(repo.current[key], t)
	repo.tickets[id] = &kongStubTicketState{
		AccountID: accountID, Model: model, Status: KongTicketStatusVerified,
		ExpiresAt: t.ExpiresAt, CapturedAt: t.CapturedAt, Source: KongTicketSourceFetch,
	}
}

// 别家那张票**按当前白名单已经不合格**时不交接：据它换号会让本账号被记进失败列表，而它即将补上的
// 那张票再也用不上——代价不是"多换一次号"，是把一个可恢复的请求拒掉。
func TestKongDoesNotHandOffForStaleWhitelistTicket(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	up.fetchState = strings.Repeat("a", 292)
	up.answers = kongVerifyAnswers()
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	// 别家有一张 verified 票，但它的归因按当前白名单不合格（白名单收紧后的常见局面）。
	kongSeedElsewhereTicket(repo, 2, model, 98, map[string]float64{"gpt-5.5": 0.99})

	grant, err := svc.EnsureTicket(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if grant.DenyReason == KongDenyPreparing {
		t.Fatalf("别家的票按当前白名单不合格，不该交接：%+v", grant)
	}
	if up.fetchCalls == 0 {
		t.Fatal("应当退回同步取票")
	}
}

// 已有在途任务时后到的请求也要判交接，不能直接进最长五分钟的同步等待。
func TestKongHandsOffWhileTaskInFlight(t *testing.T) {
	const model = "gpt-6-astra"
	repo, _, svc := kongManualFixture(t, model)
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	kongSeedElsewhereTicket(repo, 2, model, 97, map[string]float64{model: 0.97})
	// 预置一个在途任务：决策会给出 Wait。
	task, outcome := svc.claimTask(1, model, KongEgressKey(KongTicketEgressDirect, nil))
	if outcome != kongClaimFresh {
		t.Fatal("构造在途任务失败")
	}
	defer svc.releaseTask(1, task)

	grant, err := svc.EnsureTicket(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if grant.DenyReason != KongDenyPreparing {
		t.Fatalf("有在途任务且别家有票时该交接，而不是原地等：%+v", grant)
	}
}

// 原生 WS 路径**不交接**：那条路上的拒服会直接关闭连接，没有 failover 可走，返回 preparing 等于
// 把一条只需等几十秒的连接断掉（改动前它是同步等的）。
func TestKongWSPathNeverHandsOff(t *testing.T) {
	const model = "gpt-6-astra"
	repo, up, svc := kongManualFixture(t, model)
	up.fetchState = strings.Repeat("a", 292)
	up.answers = kongVerifyAnswers()
	idleSince := time.Now().Add(-40 * time.Minute)
	repo.lastEgressUsed = &idleSince
	kongSeedElsewhereTicket(repo, 2, model, 96, map[string]float64{model: 0.97})

	grant, err := svc.EnsureTicketNoHandoff(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if grant.DenyReason == KongDenyPreparing {
		t.Fatalf("WS 路径不该交接：%+v", grant)
	}
	if up.fetchCalls == 0 {
		t.Fatal("WS 路径应当同步取票")
	}
}
